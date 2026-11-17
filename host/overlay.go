package host

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func (s *supervisor) fetch(ctx context.Context) (bool, error) {
	var manifest ocispec.Manifest
	tagged, err := s.get(ctx, "manifests", s.record.Tag)
	if tagged == nil {
		return false, err
	}
	err = json.NewDecoder(tagged.Body).Decode(&manifest)
	_ = tagged.Body.Close()
	if err != nil || len(manifest.Layers) == 0 || !protocol.ValidDigest(manifest.Layers[0].Digest.String()) {
		return false, errors.New("the tag names no overlay")
	}
	blob, err := s.get(ctx, "blobs", manifest.Layers[0].Digest.String())
	if blob == nil {
		return false, err
	}
	defer func() { _ = blob.Body.Close() }()
	unpacked, err := s.unpack(blob.Body)
	if err == nil && unpacked != manifest.Layers[0].Digest {
		err = errors.New("digest mismatch")
	}
	return err == nil, err
}

// unpack removes what the previous overlay wrote, keeping what the app made beside it, such as its installed
// dependencies, and writes overlay into the source directory. It returns the overlay's digest.
func (s *supervisor) unpack(overlay io.Reader) (digest.Digest, error) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	slices.Sort(s.written)
	for _, name := range slices.Backward(s.written) {
		_ = root.Remove(name)
	}
	digester := digest.SHA256.Digester()
	stream := io.TeeReader(overlay, digester.Hash())
	if s.written, err = extract(stream, root); err == nil {
		_, err = io.Copy(io.Discard, stream)
	}
	if err != nil {
		return "", fmt.Errorf("unpack source: %w", err)
	}
	s.unpacked = digester.Digest()
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

// opaque marks the app directory in the lowest frozen layer as an [opaque whiteout], so nothing of the base image shows
// through there.
//
// [opaque whiteout]: https://github.com/opencontainers/image-spec/blob/main/layer.md#opaque-whiteout
const opaque = ".wh..wh..opq"

func (s *supervisor) freeze(ctx context.Context, token string, dependencies []string) (digest.Digest, error) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	source := map[string]bool{path.Dir(runtimecontract.PlanFile): true}
	for _, name := range dependencies {
		source[name] = true
	}
	image := ocispec.Image{
		Platform: ocispec.Platform{Architecture: runtime.GOARCH, OS: runtime.GOOS},
		Config:   ocispec.ImageConfig{WorkingDir: s.root},
		RootFS:   ocispec.RootFS{Type: "layers"},
	}
	_, base, _ := strings.Cut(os.Getenv(runtimecontract.BaseImageEnv), "@")
	manifest := ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Annotations: map[string]string{ocispec.AnnotationBaseImageDigest: base},
	}
	for _, selected := range []struct {
		starts   []string
		excluded map[string]bool
	}{{dependencies, nil}, {[]string{"."}, source}} {
		if len(selected.starts) == 0 {
			continue
		}
		var diffID digest.Digest
		layer, err := s.push(ctx, token, ocispec.MediaTypeImageLayerZstd, func(w io.Writer) (err error) {
			diffID, err = s.layer(w, root.FS(), selected.starts, selected.excluded, len(manifest.Layers) == 0)
			return err
		})
		if err != nil {
			return "", err
		}
		manifest.Layers = append(manifest.Layers, layer)
		image.RootFS.DiffIDs = append(image.RootFS.DiffIDs, diffID)
	}
	config, _ := json.Marshal(image)
	manifest.Config, err = s.push(ctx, token, ocispec.MediaTypeImageConfig, func(w io.Writer) error {
		_, err := w.Write(config)
		return err
	})
	if err != nil {
		return "", err
	}
	document, _ := json.Marshal(manifest)
	reference := digest.FromBytes(document)
	request := s.request(ctx, token, http.MethodPut, s.record.Location().JoinPath("manifests", reference.String()), bytes.NewReader(document))
	request.Header.Set(protocol.HeaderContentType, ocispec.MediaTypeImageManifest)
	_, err = s.send(request, http.StatusCreated)
	return reference, err
}

func (s *supervisor) get(ctx context.Context, kind, reference string) (*http.Response, error) {
	request := s.request(ctx, s.record.Token, http.MethodGet, s.record.Location().JoinPath(kind, reference), nil)
	request.Header.Set("Accept", ocispec.MediaTypeImageManifest)
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

func (s *supervisor) push(ctx context.Context, token, mediaType string, write func(io.Writer) error) (ocispec.Descriptor, error) {
	digester := digest.SHA256.Digester()
	var size counter
	if err := write(io.MultiWriter(digester.Hash(), &size)); err != nil {
		return ocispec.Descriptor{}, err
	}
	blob := ocispec.Descriptor{MediaType: mediaType, Digest: digester.Digest(), Size: int64(size)}
	blobs := s.record.Location().JoinPath("blobs")
	held, err := s.send(s.request(ctx, token, http.MethodHead, blobs.JoinPath(blob.Digest.String()), nil), http.StatusOK, http.StatusNotFound)
	if err != nil || held.StatusCode == http.StatusOK {
		return blob, err
	}
	started, err := s.send(s.request(ctx, token, http.MethodPost, blobs.JoinPath("uploads/"), nil), http.StatusAccepted)
	var location *url.URL
	if err == nil {
		location, err = started.Location()
	}
	if err != nil {
		return blob, err
	}
	query := location.Query()
	query.Set("digest", blob.Digest.String())
	location.RawQuery = query.Encode()
	reader, writer := io.Pipe()
	written := make(chan struct{})
	go func() {
		_ = writer.CloseWithError(write(writer))
		close(written)
	}()
	upload := s.request(ctx, token, http.MethodPut, location, reader)
	upload.ContentLength = blob.Size
	upload.Header.Set(protocol.HeaderContentType, "application/octet-stream")
	_, err = s.send(upload, http.StatusCreated)
	_ = reader.Close()
	<-written
	return blob, err
}

func (s *supervisor) request(ctx context.Context, token, method string, target *url.URL, body io.Reader) *http.Request {
	request, _ := http.NewRequestWithContext(ctx, method, target.String(), body)
	request.Header.Set(protocol.HeaderAuthorization, protocol.BearerPrefix+token)
	return request
}

func (s *supervisor) send(request *http.Request, statuses ...int) (*http.Response, error) {
	response, err := (&http.Client{Transport: s.transport}).Do(request)
	if err != nil {
		return nil, err
	}
	_ = response.Body.Close()
	if !slices.Contains(statuses, response.StatusCode) {
		return nil, fmt.Errorf("registry answered %d", response.StatusCode)
	}
	return response, nil
}

func (s *supervisor) layer(w io.Writer, fsys fs.FS, starts []string, excluded map[string]bool, lowest bool) (digest.Digest, error) {
	uncompressed := digest.SHA256.Digester()
	encoder, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return "", err
	}
	archive := tar.NewWriter(io.MultiWriter(encoder, uncompressed.Hash()))
	prefix := strings.TrimPrefix(s.root, "/")
	buffer := make([]byte, 32<<10)
	walk := func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if excluded[name] && entry.IsDir() {
			return fs.SkipDir
		}
		if excluded[name] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &tar.Header{
			Name: path.Join(prefix, name), Mode: int64(info.Mode().Perm()), Uid: appUser, Gid: appUser, ModTime: time.Unix(0, 0),
		}
		switch info.Mode().Type() {
		case fs.ModeDir:
			header.Typeflag, header.Name = tar.TypeDir, header.Name+"/"
		case fs.ModeSymlink:
			header.Typeflag = tar.TypeSymlink
			header.Linkname, err = fs.ReadLink(fsys, name)
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
		file, err := fsys.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		_, err = io.CopyBuffer(archive, io.LimitReader(file, header.Size), buffer)
		return err
	}
	if lowest {
		err = archive.WriteHeader(&tar.Header{
			Name: path.Join(prefix, opaque), Typeflag: tar.TypeReg, Uid: appUser, Gid: appUser, ModTime: time.Unix(0, 0),
		})
	}
	for _, start := range starts {
		if err == nil {
			err = fs.WalkDir(fsys, start, walk)
		}
	}
	err = errors.Join(err, archive.Close(), encoder.Close())
	return uncompressed.Digest(), err
}

type counter int64

func (c *counter) Write(p []byte) (int, error) {
	*c += counter(len(p))
	return len(p), nil
}
