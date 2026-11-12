package host

import (
	"archive/tar"
	"cmp"
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
	"time"

	"github.com/holon-sh/protocol"
	"github.com/klauspost/compress/zstd"
)

// A source overlay lets `holon dev` restart the application in place from new source. The platform records the latest
// one in the file HOLON_OVERLAY_FILE names, which a new container starts from.
const (
	reloadPath       = "/.holon/reload"
	reloadDirEnv     = "HOLON_RELOAD_DIR"
	overlayFileEnv   = "HOLON_OVERLAY_FILE"
	overlayMediaType = "application/vnd.holon.source-overlay.v0.tar+zstd"
)

type reloadRequest struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Token      string `json:"token"`
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
	gone                 <-chan struct{}
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
	if r.Header.Get("Content-Type") != overlayMediaType {
		writeJSON(w, http.StatusUnsupportedMediaType, reloadFailure{Message: "invalid reload request"})
		return
	}
	if !h.reloading.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, reloadFailure{Message: "reload in progress"})
		return
	}
	defer h.reloading.Store(false)
	h.mu.Lock()
	previous := cmp.Or(h.serving, h.image)
	h.mu.Unlock()
	directory, digest, err := h.unpackOverlay(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, reloadFailure{Message: err.Error()})
		return
	}
	job := &reloadJob{directory: directory, gone: r.Context().Done(), reply: make(chan *reloadFailure, 1)}
	failure := prepare(r.Context(), job.directory, previous)
	if failure == nil {
		h.reloads <- job
		failure = <-job.reply
	}
	if failure != nil {
		writeJSON(w, failure.status, failure)
		return
	}
	writeJSON(w, http.StatusOK, reloadReply{job.restartedAt.UTC(), job.readyAt.UTC(), digest})
}

// bootOverlay unpacks the overlay the platform recorded. A development image holds no source of its own, so a failure
// here fails the boot.
func (h *host) bootOverlay(ctx context.Context, recorded []byte) (string, error) {
	if len(recorded) == 0 {
		return "", nil
	}
	var request reloadRequest
	if protocol.DecodeJSONNonNull(recorded, &request) != nil || request.blob() == nil {
		return "", errors.New("invalid " + overlayFileEnv)
	}
	directory, err := h.fetchOverlay(ctx, request)
	if err == nil && directory != "" {
		if failure := prepare(ctx, directory, h.image); failure != nil {
			err = failure
		}
	}
	if err != nil {
		return "", fmt.Errorf("boot overlay %s: %w", request.Digest, err)
	}
	return directory, nil
}

// blob is where a well-formed request's overlay is, and nil when the request is malformed.
func (request reloadRequest) blob() *url.URL {
	base, err := url.Parse(request.Registry)
	if err != nil || base.Host == "" || base.Scheme != "http" && base.Scheme != "https" || request.Repository == "" ||
		request.Token == "" || !protocol.ValidDigest(request.Digest) || request.MediaType != overlayMediaType {
		return nil
	}
	return base.JoinPath("v2", request.Repository, "blobs", request.Digest)
}

// fetchOverlay streams the blob into a fresh directory while hashing it. Nothing runs from the directory until the
// digest has matched, and it is removed if it does not.
func (h *host) fetchOverlay(ctx context.Context, request reloadRequest) (string, error) {
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, request.blob().String(), nil)
	if err != nil {
		return "", err
	}
	get.Header.Set("Authorization", "Bearer "+request.Token)
	response, err := (&http.Client{Transport: h.transport}).Do(get)
	if err != nil {
		return "", errors.New("registry unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry answered %d", response.StatusCode)
	}
	directory, digest, err := h.unpackOverlay(response.Body)
	if err == nil && digest != request.Digest {
		_ = os.RemoveAll(directory)
		err = errors.New("digest mismatch")
	}
	if err != nil {
		return "", err
	}
	return directory, nil
}

func (h *host) unpackOverlay(overlay io.Reader) (string, string, error) {
	directory, err := os.MkdirTemp(h.overlays, "")
	if err != nil {
		return "", "", fmt.Errorf("prepare source directory: %w", err)
	}
	hash := sha256.New()
	stream := io.TeeReader(overlay, hash)
	if err = extractOverlay(stream, directory); err == nil {
		_, err = io.Copy(io.Discard, stream)
	}
	if err != nil {
		_ = os.RemoveAll(directory)
		return "", "", fmt.Errorf("unpack source: %w", err)
	}
	return directory, "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
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

// srcLayout reports an overlay whose packages live under its own src/. A project packaged that way is also installed
// in the image's environment, which would otherwise win the import.
func srcLayout(directory string) bool {
	src, err := os.Lstat(filepath.Join(directory, "src"))
	packages, _ := fs.Glob(os.DirFS(directory), "*/__init__.py")
	return err == nil && src.IsDir() && len(packages) == 0
}
