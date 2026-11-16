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
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/klauspost/compress/zstd"
)

func (s *supervisor) fetch(ctx context.Context) (bool, error) {
	var manifest struct {
		Layers []struct{ Digest string } `json:"layers"`
	}
	tagged, err := s.get(ctx, "manifests", s.record.Tag)
	if tagged == nil {
		return false, err
	}
	err = json.NewDecoder(tagged.Body).Decode(&manifest)
	_ = tagged.Body.Close()
	if err != nil || len(manifest.Layers) == 0 || !protocol.ValidDigest(manifest.Layers[0].Digest) {
		return false, errors.New("the tag names no overlay")
	}
	blob, err := s.get(ctx, "blobs", manifest.Layers[0].Digest)
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

func (s *supervisor) get(ctx context.Context, kind, reference string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.record.Location().JoinPath(kind, reference).String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set(protocol.HeaderAuthorization, protocol.BearerPrefix+s.record.Token)
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
	s.unpacked = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return s.unpacked, nil
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

const appUser = 10001

func (s *supervisor) push(ctx context.Context, token string) (runtimecontract.Layer, error) {
	send := func(method string, target *url.URL, body io.Reader, status int) (*url.URL, error) {
		request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
		if err != nil {
			return nil, err
		}
		request.Header.Set(protocol.HeaderAuthorization, protocol.BearerPrefix+token)
		response, err := (&http.Client{Transport: s.transport}).Do(request)
		if err != nil {
			return nil, err
		}
		_ = response.Body.Close()
		if response.StatusCode != status {
			return nil, fmt.Errorf("registry answered %d", response.StatusCode)
		}
		return response.Location()
	}
	started, err := send(http.MethodPost, s.record.Location().JoinPath("blobs", "uploads/"), nil, http.StatusAccepted)
	if err != nil {
		return runtimecontract.Layer{}, err
	}
	reader, writer := io.Pipe()
	layers := make(chan runtimecontract.Layer, 1)
	go func() {
		layer, err := s.layer(writer)
		_ = writer.CloseWithError(err)
		layers <- layer
	}()
	patched, err := send(http.MethodPatch, started, reader, http.StatusAccepted)
	_ = reader.Close()
	layer := <-layers
	if err != nil {
		return runtimecontract.Layer{}, err
	}
	query := patched.Query()
	query.Set("digest", layer.Digest)
	patched.RawQuery = query.Encode()
	_, err = send(http.MethodPut, patched, nil, http.StatusCreated)
	return layer, err
}

func (s *supervisor) layer(w io.Writer) (runtimecontract.Layer, error) {
	compressed, uncompressed := sha256.New(), sha256.New()
	var size counter
	encoder, err := zstd.NewWriter(io.MultiWriter(w, compressed, &size), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return runtimecontract.Layer{}, err
	}
	archive := tar.NewWriter(io.MultiWriter(encoder, uncompressed))
	metadata := filepath.Join(s.root, filepath.Dir(runtimecontract.PlanFile))
	buffer := make([]byte, 32<<10)
	err = filepath.Walk(s.root, func(name string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if name == metadata {
			return filepath.SkipDir
		}
		header := &tar.Header{
			Name: strings.TrimPrefix(name, "/"), Mode: int64(info.Mode().Perm()), Uid: appUser, Gid: appUser,
			ModTime: time.Unix(0, 0),
		}
		switch info.Mode().Type() {
		case fs.ModeDir:
			header.Typeflag, header.Name = tar.TypeDir, header.Name+"/"
		case fs.ModeSymlink:
			header.Typeflag = tar.TypeSymlink
			header.Linkname, err = os.Readlink(name)
		case 0:
			header.Typeflag, header.Size = tar.TypeReg, info.Size()
		default:
			err = fmt.Errorf("unsupported entry %q", name)
		}
		if err == nil {
			err = archive.WriteHeader(header)
		}
		if err != nil || header.Typeflag != tar.TypeReg {
			return err
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		_, err = io.CopyBuffer(archive, io.LimitReader(file, header.Size), buffer)
		return err
	})
	err = errors.Join(err, archive.Close(), encoder.Close())
	return runtimecontract.Layer{
		Digest: "sha256:" + hex.EncodeToString(compressed.Sum(nil)), DiffID: "sha256:" + hex.EncodeToString(uncompressed.Sum(nil)),
		Size: int64(size), Architecture: runtime.GOARCH,
	}, err
}

type counter int64

func (c *counter) Write(p []byte) (int, error) {
	*c += counter(len(p))
	return len(p), nil
}
