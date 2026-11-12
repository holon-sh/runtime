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

	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/klauspost/compress/zstd"
)

// fetch unpacks the recorded overlay, and reports false while the registry does not hold it yet. Nothing runs from
// the source until its digest has matched, and it is removed if it does not.
func (s *supervisor) fetch(ctx context.Context, record runtimecontract.OverlayRecord) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, record.Blob().String(), nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+record.Token)
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
		s.prune(nil)
	}
	return err == nil, err
}

// unpack writes an overlay over the source directory and removes what the previous overlay wrote and this one does
// not, which keeps what the app made there, such as its installed dependencies. It returns the overlay's digest.
func (s *supervisor) unpack(overlay io.Reader) (string, error) {
	hash := sha256.New()
	stream := io.TeeReader(overlay, hash)
	written, err := extract(stream, s.root)
	if err == nil {
		_, err = io.Copy(io.Discard, stream)
	}
	s.prune(written)
	if err != nil {
		return "", fmt.Errorf("unpack source: %w", err)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// prune removes each path the previous overlay wrote that written does not hold, deepest first. A directory that
// still holds something is kept.
func (s *supervisor) prune(written []string) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	keep := make(map[string]bool, len(written))
	for _, name := range written {
		keep[name] = true
	}
	slices.Sort(s.written)
	for _, name := range slices.Backward(s.written) {
		if !keep[name] {
			_ = root.Remove(name)
		}
	}
	s.written = written
}

// extract writes a zstd tar's entries under directory, replacing what is at each path, and returns their paths and
// the directories they imply.
func extract(blob io.Reader, directory string) ([]string, error) {
	decoder, err := zstd.NewReader(blob, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var written []string
	seen := map[string]bool{".": true}
	record := func(name string) {
		for ; !seen[name]; name = filepath.Dir(name) {
			seen[name] = true
			written = append(written, name)
		}
	}
	archive := tar.NewReader(decoder)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if !filepath.IsLocal(name) {
			return written, fmt.Errorf("%q escapes the source directory", header.Name)
		}
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return written, err
		}
		permissions := header.FileInfo().Mode().Perm()
		if info, err := root.Lstat(name); err == nil && !(info.IsDir() && header.Typeflag == tar.TypeDir) {
			if err := root.RemoveAll(name); err != nil {
				return written, err
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, permissions|0o700)
		case tar.TypeReg:
			var file *os.File
			if file, err = root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, permissions|0o600); err == nil {
				_, err = io.Copy(file, archive)
				err = errors.Join(err, file.Close())
			}
		case tar.TypeSymlink:
			link := filepath.FromSlash(header.Linkname)
			if filepath.IsAbs(link) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), link)) {
				return written, fmt.Errorf("link %q escapes the source directory", header.Name)
			}
			err = root.Symlink(link, name)
		default:
			return written, fmt.Errorf("unsupported entry %q", header.Name)
		}
		if err != nil {
			return written, err
		}
		record(name)
	}
}
