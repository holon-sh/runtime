package host

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// fetch unpacks the recorded overlay, and reports false while the registry does not hold it yet. Nothing runs from
// the source until its digest has matched.
func (s *supervisor) fetch(ctx context.Context, record runtimecontract.OverlayRecord) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, record.Blob().String(), nil)
	if err != nil {
		return false, err
	}
	request.Header.Set(protocol.HeaderAuthorization, protocol.BearerPrefix+record.Token)
	response, err := (&http.Client{Transport: s.transport}).Do(request)
	if err != nil {
		return false, errors.New("registry unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("registry answered %d", response.StatusCode)
	}
	digest, err := s.unpack(response.Body)
	if err == nil && digest != record.Digest {
		err = errors.New("digest mismatch")
	}
	return err == nil, err
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
