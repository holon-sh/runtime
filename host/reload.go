package host

import (
	"archive/tar"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/klauspost/compress/zstd"
)

// A source overlay lets `holon dev` restart the application in place from new source. It is not durable: when the
// pod or its container restarts, the application boots from its image again and the CLI re-sends on its next save.
const (
	reloadPath        = "/.holon/reload"
	reloadSecretEnv   = "HOLON_RELOAD_SECRET"
	reloadDirEnv      = "HOLON_RELOAD_DIR"
	defaultReloadDir  = "/tmp/holon-source"
	overlayMediaType  = "application/vnd.holon.source-overlay.v0.tar+zstd"
	fetchTimeout      = 45 * time.Second
	maxReloadBodySize = 64 << 10
)

// readyTimeout bounds a restarted child's startup, inside the minute the caller waits for the reply.
var readyTimeout = 45 * time.Second

type reloadRequest struct {
	Registry   string `json:"registry,omitempty"`
	Repository string `json:"repository"`
	Token      string `json:"token"`
	Digest     string `json:"digest"`
	MediaType  string `json:"mediaType,omitempty"`
}

type reloadReply struct {
	RestartedAt time.Time `json:"restartedAt"`
	ReadyAt     time.Time `json:"readyAt"`
	Digest      string    `json:"digest"`
}

type reloadFailure struct {
	status  int
	Message string `json:"error"`
	Digest  string `json:"digest,omitempty"`
	Exit    string `json:"exit,omitempty"`
}

func (f *reloadFailure) Error() string { return f.Message }

func (f *reloadFailure) outcome() reloadOutcome { return reloadOutcome{f.status, f} }

type reloadOutcome struct {
	status int
	body   any
}

// reloadJob hands an unpacked overlay to Run, which owns the child.
type reloadJob struct {
	directory, digest string
	reply             chan reloadOutcome
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *host) reload(w http.ResponseWriter, r *http.Request) {
	presented, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	got, want := sha256.Sum256([]byte(presented)), sha256.Sum256([]byte(h.reloadSecret))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, reloadFailure{Message: "invalid reload secret"})
		return
	}
	var request reloadRequest
	data, err := io.ReadAll(io.LimitReader(r.Body, maxReloadBodySize))
	if err != nil || protocol.DecodeJSONNonNull(data, &request) != nil || !protocol.ValidDigest(request.Digest) || request.Token == "" ||
		request.MediaType != "" && request.MediaType != overlayMediaType {
		writeJSON(w, http.StatusBadRequest, reloadFailure{Message: "invalid reload request"})
		return
	}
	blob, err := blobURL(request)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, reloadFailure{Message: "invalid reload request"})
		return
	}
	if !h.reloading.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, reloadFailure{Message: "reload in progress"})
		return
	}
	defer h.reloading.Store(false)
	directory, failure := h.fetchOverlay(r.Context(), blob, request)
	if failure != nil {
		writeJSON(w, failure.status, failure)
		return
	}
	job := reloadJob{directory: directory, digest: request.Digest, reply: make(chan reloadOutcome, 1)}
	select {
	case h.reloads <- job:
	case <-r.Context().Done():
		_ = os.RemoveAll(directory)
		writeJSON(w, http.StatusServiceUnavailable, reloadFailure{Message: "reload cancelled"})
		return
	}
	// The restart is past the point of return, so this waits for it even if the caller has gone.
	outcome := <-job.reply
	writeJSON(w, outcome.status, outcome.body)
}

// blobURL accepts the registry's URL beside a repository name, or a repository that starts with its registry's host.
func blobURL(request reloadRequest) (*url.URL, error) {
	registry, name := request.Registry, request.Repository
	if registry == "" {
		var host string
		host, name, _ = strings.Cut(request.Repository, "/")
		registry = "https://" + host
	}
	base, err := url.Parse(registry)
	if err != nil || base.Host == "" || base.Scheme != "http" && base.Scheme != "https" || name == "" {
		return nil, errors.New("invalid registry")
	}
	return base.JoinPath("v2", name, "blobs", request.Digest), nil
}

// fetchOverlay streams the blob into a fresh directory while hashing it. Nothing runs from the directory until the
// digest has matched, and it is removed if it does not.
func (h *host) fetchOverlay(ctx context.Context, blob *url.URL, request reloadRequest) (string, *reloadFailure) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, blob.String(), nil)
	if err != nil {
		return "", &reloadFailure{status: http.StatusBadRequest, Message: "invalid reload request"}
	}
	get.Header.Set("Authorization", "Bearer "+request.Token)
	response, err := (&http.Client{Transport: h.transport}).Do(get)
	if err != nil {
		return "", &reloadFailure{status: http.StatusBadGateway, Message: "registry unavailable"}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", &reloadFailure{status: http.StatusBadGateway, Message: fmt.Sprintf("registry answered %d", response.StatusCode)}
	}
	root := cmp.Or(os.Getenv(reloadDirEnv), defaultReloadDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", &reloadFailure{status: http.StatusInternalServerError, Message: "prepare source directory: " + err.Error()}
	}
	// The suffix keeps the directory fresh when a digest returns while its earlier directory is in use.
	short := strings.TrimPrefix(request.Digest, "sha256:")[:12]
	directory, err := os.MkdirTemp(root, short+"-")
	if err != nil {
		return "", &reloadFailure{status: http.StatusInternalServerError, Message: "prepare source directory: " + err.Error()}
	}
	failure := unpackVerified(response.Body, directory, request.Digest, h.image)
	if failure != nil {
		_ = os.RemoveAll(directory)
		return "", failure
	}
	return directory, nil
}

func unpackVerified(blob io.Reader, directory, digest, image string) *reloadFailure {
	hash := sha256.New()
	stream := io.TeeReader(blob, hash)
	unpacked := extractOverlay(stream, directory)
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return &reloadFailure{status: http.StatusBadGateway, Message: "registry read failed"}
	}
	if actual := "sha256:" + hex.EncodeToString(hash.Sum(nil)); actual != digest {
		return &reloadFailure{status: http.StatusConflict, Message: "digest mismatch", Digest: actual}
	}
	if err := errors.Join(unpacked, linkImageEntries(image, directory)); err != nil {
		return &reloadFailure{status: http.StatusInternalServerError, Message: "unpack source: " + err.Error()}
	}
	return nil
}

func extractOverlay(blob io.Reader, directory string) error {
	decoder, err := zstd.NewReader(blob, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return err
	}
	defer decoder.Close()
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	archive := tar.NewReader(decoder)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%q escapes the source directory", header.Name)
		}
		// The owner keeps write access so that a later reload can remove the directory.
		permissions := header.FileInfo().Mode().Perm()
		if header.Typeflag == tar.TypeDir {
			if err := root.MkdirAll(name, permissions|0o700); err != nil {
				return err
			}
			continue
		}
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeReg:
			file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, permissions|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(file, archive)
			if err = errors.Join(err, file.Close()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := filepath.FromSlash(header.Linkname)
			if filepath.IsAbs(link) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), link)) {
				return fmt.Errorf("link %q escapes the source directory", header.Name)
			}
			if err := root.Symlink(link, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q", header.Name)
		}
	}
}

// linkImageEntries makes what the image's application directory holds beside the source, such as node_modules or a
// built dist, visible from the overlay. The overlay wins, so a top-level source file deleted since the image was
// built stays visible until the next image build. Bytecode stays out: Python trusts a cache by its source's size and
// modification time, which do not tell two overlays apart.
func linkImageEntries(image, directory string) error {
	entries, err := os.ReadDir(image)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		link := filepath.Join(directory, entry.Name())
		if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) && entry.Name() != "__pycache__" {
			if err := os.Symlink(filepath.Join(image, entry.Name()), link); err != nil {
				return err
			}
		}
	}
	return nil
}
