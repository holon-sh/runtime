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
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/klauspost/compress/zstd"
)

func (s *supervisor) fetch(ctx context.Context, record runtimecontract.OverlayRecord) (bool, error) {
	var manifest struct {
		Layers []struct{ Digest string } `json:"layers"`
	}
	tagged, err := s.get(ctx, record, "manifests", record.Tag)
	if tagged == nil {
		return false, err
	}
	err = json.NewDecoder(tagged.Body).Decode(&manifest)
	_ = tagged.Body.Close()
	if err != nil || len(manifest.Layers) == 0 || !protocol.ValidDigest(manifest.Layers[0].Digest) {
		return false, errors.New("the tag names no overlay")
	}
	blob, err := s.get(ctx, record, "blobs", manifest.Layers[0].Digest)
	if blob == nil {
		return false, err
	}
	defer func() { _ = blob.Body.Close() }()
	digest, err := s.unpack(blob.Body)
	if err == nil && digest != manifest.Layers[0].Digest {
		err = errors.New("digest mismatch")
	}
	return err == nil, err
}

func (s *supervisor) get(ctx context.Context, record runtimecontract.OverlayRecord, kind, reference string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, record.Location().JoinPath(kind, reference).String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set(protocol.HeaderAuthorization, protocol.BearerPrefix+record.Token)
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	response, err := (&http.Client{Transport: s.transport}).Do(request)
	if err != nil {
		return nil, errors.New("registry unavailable")
	}
	if response.StatusCode == http.StatusOK {
		return response, nil
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	return nil, fmt.Errorf("registry answered %d", response.StatusCode)
}

// unpack removes what the previous overlay wrote, keeping what the app made beside it, such as its installed
// dependencies, and writes overlay into the source directory. It returns the overlay's digest.
func (s *supervisor) unpack(overlay io.Reader) (string, error) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	slices.Sort(s.written)
	for _, name := range slices.Backward(s.written) {
		_ = root.Remove(name)
	}
	hash := sha256.New()
	stream := io.TeeReader(overlay, hash)
	if s.written, err = extract(stream, root); err == nil {
		_, err = io.Copy(io.Discard, stream)
	}
	if err != nil {
		return "", fmt.Errorf("unpack source: %w", err)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// extract writes a zstd tar's entries under root, replacing what the app made at the same path, and returns their
// paths.
func extract(blob io.Reader, root *os.Root) ([]string, error) {
	decoder, err := zstd.NewReader(blob, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	var written []string
	archive := tar.NewReader(decoder)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		name := filepath.Clean(header.Name)
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return written, err
		}
		permissions := header.FileInfo().Mode().Perm()
		switch header.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, permissions|0o700)
		case tar.TypeReg:
			var file *os.File
			if file, err = root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permissions); err == nil {
				_, err = io.Copy(file, archive)
				err = errors.Join(err, file.Close())
			}
		case tar.TypeSymlink:
			_ = root.RemoveAll(name)
			err = root.Symlink(header.Linkname, name)
		default:
			err = fmt.Errorf("unsupported entry %q", header.Name)
		}
		if err != nil {
			return written, err
		}
		written = append(written, name)
	}
}
