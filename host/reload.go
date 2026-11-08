package host

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/klauspost/compress/zstd"
)

// A source overlay lets `holon dev` restart the application in place from new source. The platform records the latest
// one in the file HOLON_OVERLAY_FILE names, which a new container starts from.
const (
	reloadPath        = "/.holon/reload"
	reloadDirEnv      = "HOLON_RELOAD_DIR"
	overlayFileEnv    = "HOLON_OVERLAY_FILE"
	defaultReloadDir  = "/tmp/holon-source"
	overlayMediaType  = "application/vnd.holon.source-overlay.v0.tar+zstd"
	maxReloadBodySize = 64 << 10
)

// reloadTimeout bounds a reload from its request to the new child's ready, inside the minute the caller waits.
var reloadTimeout = 45 * time.Second

type reloadRequest struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
	MediaType  string `json:"mediaType"`
}

type reloadReply struct {
	RestartedAt time.Time `json:"restartedAt"`
	ReadyAt     time.Time `json:"readyAt"`
	Digest      string    `json:"digest"`
}

type reloadFailure struct {
	status  int
	Message string `json:"error"`
	Exit    string `json:"exit,omitempty"`
}

func (f *reloadFailure) Error() string { return f.Message }

// reloadJob hands an unpacked overlay to Run, which owns the child. Run fills in the times before a nil reply.
type reloadJob struct {
	directory            string
	deadline             time.Time
	restartedAt, readyAt time.Time
	reply                chan *reloadFailure
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *host) reload(w http.ResponseWriter, r *http.Request) {
	deadline := time.Now().Add(reloadTimeout)
	var request reloadRequest
	var blob *url.URL
	if data, err := io.ReadAll(io.LimitReader(r.Body, maxReloadBodySize)); err == nil && protocol.DecodeJSONNonNull(data, &request) == nil {
		blob = request.blob()
	}
	if blob == nil {
		writeJSON(w, http.StatusBadRequest, reloadFailure{Message: "invalid reload request"})
		return
	}
	if !h.reloading.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, reloadFailure{Message: "reload in progress"})
		return
	}
	defer h.reloading.Store(false)
	job := &reloadJob{deadline: deadline, reply: make(chan *reloadFailure, 1)}
	var failure *reloadFailure
	if job.directory, failure = h.fetchOverlay(r.Context(), blob, request, deadline); failure == nil {
		// Run receives unless it is returning, and the restart then waits for it even if the caller has gone.
		h.reloads <- job
		failure = <-job.reply
	}
	if failure != nil {
		writeJSON(w, failure.status, failure)
		return
	}
	writeJSON(w, http.StatusOK, reloadReply{job.restartedAt.UTC(), job.readyAt.UTC(), request.Digest})
}

// bootOverlay unpacks the overlay the platform recorded. A development image holds no source of its own, so a failure
// here fails the boot.
func (h *host) bootOverlay(ctx context.Context, recorded []byte) (string, error) {
	var request reloadRequest
	if protocol.DecodeJSONNonNull(recorded, &request) != nil || request.blob() == nil {
		return "", errors.New("invalid " + overlayFileEnv)
	}
	directory, failure := h.fetchOverlay(ctx, request.blob(), request, time.Now().Add(reloadTimeout))
	if failure != nil {
		return "", fmt.Errorf("boot overlay %s: %w", request.Digest, failure)
	}
	return directory, nil
}

// blob is where a well-formed request's overlay is, and nil when the request is malformed.
func (request reloadRequest) blob() *url.URL {
	base, err := url.Parse(request.Registry)
	if err != nil || base.Host == "" || base.Scheme != "http" && base.Scheme != "https" || request.Repository == "" ||
		!protocol.ValidDigest(request.Digest) || request.MediaType != overlayMediaType {
		return nil
	}
	return base.JoinPath("v2", request.Repository, "blobs", request.Digest)
}

// fetchOverlay streams the blob into a fresh directory while hashing it. Nothing runs from the directory until the
// digest has matched, and it is removed if it does not.
func (h *host) fetchOverlay(ctx context.Context, blob *url.URL, request reloadRequest, deadline time.Time) (string, *reloadFailure) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	response, err := h.getBlob(ctx, blob)
	if err != nil {
		return "", &reloadFailure{status: http.StatusBadGateway, Message: "registry: " + err.Error()}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", &reloadFailure{status: http.StatusBadGateway, Message: fmt.Sprintf("registry answered %d", response.StatusCode)}
	}
	// The suffix keeps the directory fresh when a digest returns while its earlier directory is in use.
	short := strings.TrimPrefix(request.Digest, "sha256:")[:12]
	directory, err := os.MkdirTemp(h.overlays, short+"-")
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
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
		return &reloadFailure{status: http.StatusBadGateway, Message: "digest mismatch"}
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

// srcLayout reports an overlay whose packages live under its own src/. A project packaged that way is also installed
// in the image's environment, which would otherwise win the import.
func srcLayout(directory string) bool {
	src, err := os.Lstat(filepath.Join(directory, "src"))
	packages, _ := fs.Glob(os.DirFS(directory), "*/__init__.py")
	return err == nil && src.IsDir() && len(packages) == 0
}
