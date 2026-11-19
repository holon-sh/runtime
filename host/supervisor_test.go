package host

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// app is a fake application whose reply names its source. It records its pid and makes a file of its own, as an
// install would, and prints that it serves.
func app(message string) string {
	return `import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer

open("pid", "w").write(str(os.getpid()))
open("made", "w").write("")
print("serving", flush=True)

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(json.dumps(["` + message + `", self.headers["Host"], self.headers["X-Forwarded-Host"]]).encode())

HTTPServer((os.environ["HOST"], int(os.environ["PORT"])), Handler).serve_forever()
`
}

func source(install runtimecontract.Install, prepare [][]string, start []string, main string) map[string]string {
	plan, _ := json.Marshal(runtimecontract.Plan{Install: install, Prepare: prepare, Start: start})
	return map[string]string{runtimecontract.PlanFile: string(plan), "main.py": main}
}

func overlay(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, _ := zstd.NewWriter(&compressed)
	archive := tar.NewWriter(encoder)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		_ = archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name]))})
		_, _ = archive.Write([]byte(files[name]))
	}
	if err := archive.Close(); err != nil || encoder.Close() != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

type running struct {
	t    *testing.T
	url  string
	root string
}

// run starts the supervisor, from record when it is set, and returns once it listens.
func run(t *testing.T, record *runtimecontract.OverlayRecord) *running {
	t.Helper()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	r := &running{t: t, url: "http://127.0.0.1:" + port, root: filepath.Join(t.TempDir(), "source")}
	setup(t, port, r.root, record)
	ctx, cancel := context.WithCancel(context.Background())
	var err error
	exited := make(chan struct{})
	go func() {
		err = Run(ctx)
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		<-exited
	})
	for {
		if connection, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
			_ = connection.Close()
			return r
		}
		select {
		case <-exited:
			t.Fatalf("runtime exited: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func setup(t *testing.T, port, root string, record *runtimecontract.OverlayRecord) {
	t.Setenv("PORT", port)
	t.Setenv(reloadDirEnv, root)
	t.Setenv(runtimecontract.OverlayFileEnv, "")
	if record != nil {
		file := filepath.Join(t.TempDir(), "overlay")
		data, _ := json.Marshal(record)
		_ = os.WriteFile(file, data, 0o600)
		t.Setenv(runtimecontract.OverlayFileEnv, file)
	}
}

func (r *running) reload(body []byte) (int, runtimecontract.BuildReply) {
	r.t.Helper()
	return r.post(runtimecontract.ReloadPath, body)
}

func (r *running) build(body []byte) (int, runtimecontract.BuildReply) {
	r.t.Helper()
	return r.post(runtimecontract.BuildPath, body)
}

func (r *running) post(path string, body []byte) (int, runtimecontract.BuildReply) {
	r.t.Helper()
	request, _ := http.NewRequest(http.MethodPost, r.url+path, bytes.NewReader(body))
	request.Header.Set(protocol.HeaderContentType, runtimecontract.OverlayMediaType)
	request.Header.Set(runtimecontract.BuildTokenHeader, "push")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var reply runtimecontract.BuildReply
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		r.t.Fatal(err)
	}
	return response.StatusCode, reply
}

func (r *running) pid() int {
	r.t.Helper()
	pid, _ := os.ReadFile(filepath.Join(r.root, "pid"))
	id, _ := strconv.Atoi(string(pid))
	return id
}

func (r *running) get() (int, string) {
	r.t.Helper()
	request, _ := http.NewRequest(http.MethodGet, r.url, nil)
	request.Host = "app.example"
	request.Header.Set("X-Forwarded-Host", "session.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func TestReload(t *testing.T) {
	log, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = log, log
	t.Cleanup(func() { os.Stdout, os.Stderr = stdout, stderr })
	r := run(t, nil)
	serve := []string{"python3", "-c", "import subprocess; raise SystemExit(subprocess.run(['python3', 'main.py']).returncode)"}
	prepare := [][]string{{"echo", "prepared"}}
	install := runtimecontract.Install{Inputs: "sha256:one", Commands: [][]string{{"echo", "installing"}, {"echo", "installed"}}}
	first := source(install, prepare, serve, app("first"))
	first["old.py"] = ""
	if status, reply := r.reload(overlay(t, first)); status != http.StatusOK || reply.Output != "installing\ninstalled\nprepared\n" {
		t.Fatalf("reload: %d %+v", status, reply)
	}
	if status, body := r.get(); status != http.StatusOK || body != `["first", "app.example", "session.example"]` {
		t.Fatalf("first source: %d %s", status, body)
	}
	pid := r.pid()
	if status, reply := r.reload(overlay(t, first)); status != http.StatusOK || r.pid() != pid {
		t.Fatalf("the overlay the app runs: %d %+v", status, reply)
	}
	second := overlay(t, source(install, prepare, serve, app("second")))
	if status, reply := r.reload(second); status != http.StatusOK || reply.Output != "prepared\n" {
		t.Fatalf("unchanged install: %d %+v", status, reply)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("the first app still runs")
	}
	if _, err := os.Stat(filepath.Join(r.root, "old.py")); err == nil {
		t.Fatal("what only an earlier overlay wrote is still there")
	}
	if err := syscall.Kill(r.pid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for status, _ := r.get(); status != http.StatusServiceUnavailable; status, _ = r.get() {
	}
	if status, reply := r.reload(second); status != http.StatusOK || reply.Output != "prepared\n" {
		t.Fatalf("the overlay of an app that exited: %d %+v", status, reply)
	}

	failing := runtimecontract.Install{Inputs: "sha256:two", Commands: [][]string{
		{"python3", "-c", `print("a" * 4999); print("no such package"); raise SystemExit(3)`}, {"echo", "after"},
	}}
	for range 2 {
		if status, reply := r.reload(overlay(t, source(failing, nil, serve, app("uninstalled")))); status != http.StatusInternalServerError ||
			!strings.Contains(reply.Error, "exit status 3") || reply.Output != "no such package\n" {
			t.Fatalf("failing install: %d %+v", status, reply)
		}
	}
	if status, reply := r.reload(overlay(t, source(install, nil, serve, app("third")))); status != http.StatusOK || reply.Output != "installing\ninstalled\n" {
		t.Fatalf("install after a failing one: %d %+v", status, reply)
	}
	if status, reply := r.reload(overlay(t, source(install, nil, serve, "raise SystemExit(4)"))); status != http.StatusInternalServerError {
		t.Fatalf("failing start: %d %+v", status, reply)
	}
	if status, _ := r.get(); status != http.StatusServiceUnavailable {
		t.Fatalf("after a failing reload: %d", status)
	}
	if _, err := os.Stat(filepath.Join(r.root, "made")); err != nil {
		t.Fatal("what the app made is gone")
	}
	if logged, _ := os.ReadFile(log.Name()); !strings.Contains(string(logged), "serving") || strings.Contains(string(logged), "installed") {
		t.Fatalf("log %q", logged)
	}

	if status, _ := r.reload(overlay(t, map[string]string{"../escaped": ""})); status != http.StatusBadRequest {
		t.Fatalf("escaping reload: %d", status)
	}
}

func TestPnpmLauncher(t *testing.T) {
	s := supervisor{root: t.TempDir()}
	t.Setenv("PATH", s.root)
	commands := [][]string{{"pnpm", "a b", "$HOME"}, {"/bin/sh", "-c", "echo complete"}}
	var output bytes.Buffer
	if err := s.run(t.Context(), commands, &output); err == nil || !strings.Contains(output.String(), "pnpm") || strings.Contains(output.String(), "complete") {
		t.Fatalf("missing pnpm: %v, output %q", err, &output)
	}
	if err := os.WriteFile(filepath.Join(s.root, "pnpm"), []byte(`printf '<%s>\n' "$@"`), 0o755); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := s.run(t.Context(), commands, &output); err != nil || output.String() != "<a b>\n<$HOME>\ncomplete\n" {
		t.Fatalf("pnpm launcher: %v, output %q", err, &output)
	}
}

func TestBootStartsTheRecordedOverlay(t *testing.T) {
	blobs := map[string][]byte{}
	tags := map[string]string{"tampered": "sha256:" + strings.Repeat("0", 64), "malformed": "sha256:../../other"}
	for session, message := range map[string]string{"recorded": "recorded", "other": "another session's"} {
		blob := overlay(t, source(runtimecontract.Install{}, nil, []string{"python3", "main.py"}, app(message)))
		tags[session] = digest.FromBytes(blob).String()
		blobs[tags[session]] = blob
	}
	blobs[tags["tampered"]] = blobs[tags["recorded"]]
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, reference := path.Split(strings.TrimPrefix(r.URL.Path, "/v2/holon/app/"))
		switch {
		case r.Header.Get("Authorization") != "Bearer pull":
			w.WriteHeader(http.StatusUnauthorized)
		case kind == "manifests/" && tags[reference] != "":
			_, _ = io.WriteString(w, `{"layers":[{"mediaType":"`+runtimecontract.OverlayMediaType+`","digest":"`+tags[reference]+`"}]}`)
		case kind == "blobs/" && blobs[reference] != nil:
			_, _ = w.Write(blobs[reference])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(registry.Close)
	record := func(session string) *runtimecontract.OverlayRecord {
		return &runtimecontract.OverlayRecord{Registry: registry.URL, Repository: "holon/app", Tag: session, Token: "pull"}
	}

	t.Run("its session's tag, not another session's, which a reload of the same overlay leaves running", func(t *testing.T) {
		r := run(t, record("recorded"))
		for status, body := r.get(); !strings.Contains(body, "recorded"); status, body = r.get() {
			if status != http.StatusServiceUnavailable {
				t.Fatalf("recorded source: %d %s", status, body)
			}
			time.Sleep(10 * time.Millisecond)
		}
		pid := r.pid()
		if status, reply := r.reload(blobs[tags["recorded"]]); status != http.StatusOK || r.pid() != pid {
			t.Fatalf("reload of the recorded overlay: %d %+v", status, reply)
		}
	})
	t.Run("a session the registry holds no overlay for yet", func(t *testing.T) {
		r := run(t, record("unsaved"))
		if status, reply := r.reload(blobs[tags["recorded"]]); status != http.StatusOK {
			t.Fatalf("reload: %d %+v", status, reply)
		}
	})
	for session, want := range map[string]string{"tampered": "digest mismatch", "malformed": "the tag names no overlay"} {
		t.Run(session, func(t *testing.T) {
			setup(t, "0", filepath.Join(t.TempDir(), "source"), record(session))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := Run(ctx); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Run = %v", err)
			}
		})
	}
}

func TestBuild(t *testing.T) {
	var held sync.Map
	var uploads atomic.Int64
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, found := held.Load(path.Base(r.URL.Path))
		switch {
		case r.Header.Get("Authorization") != "Bearer push":
			http.NotFound(w, r)
		case r.Method == http.MethodHead && !found:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodHead:
		case r.Method == http.MethodPut && path.Dir(r.URL.Path) == "/v2/holon/app/manifests":
			data, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != ocispec.MediaTypeImageManifest || digest.FromBytes(data).String() != path.Base(r.URL.Path) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			held.Store(path.Base(r.URL.Path), data)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/holon/app/blobs/uploads/":
			w.Header().Set("Location", "/upload?_state=started")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && r.URL.Query().Get("_state") == "started":
			data, _ := io.ReadAll(r.Body)
			reference := r.URL.Query().Get("digest")
			if digest.FromBytes(data).String() != reference {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			held.Store(reference, data)
			uploads.Add(1)
			w.Header().Set("Location", "/v2/holon/app/blobs/"+reference)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(registry.Close)
	base := digest.FromString("base")
	t.Setenv(runtimecontract.BaseImageEnv, "registry.example/holon/development@"+base.String())
	annotations := map[string]string{ocispec.AnnotationBaseImageDigest: base.String()}
	r := run(t, &runtimecontract.OverlayRecord{Registry: registry.URL, Repository: "holon/app", Tag: "session", Token: "pull"})
	serve := []string{"python3", "main.py"}
	install := runtimecontract.Install{Inputs: "sha256:one", Commands: [][]string{{"echo", "installing"}}}
	frozen := func(build runtimecontract.Build, commands ...[]string) []byte {
		files := source(runtimecontract.Install{Inputs: "sha256:build", Commands: commands}, nil, serve, app("built"))
		data, _ := json.Marshal(build)
		files[runtimecontract.BuildFile] = string(data)
		return overlay(t, files)
	}
	freeze := runtimecontract.Build{Freeze: &runtimecontract.Freeze{Dependencies: []string{"lib"}}}
	tools := []string{"python3", "-c", `import os
assert os.environ["SOURCE_DATE_EPOCH"] == "0" and "UV_PYTHON_DOWNLOADS" not in os.environ
for key in ("UV_PYTHON_INSTALL_DIR", "FNM_DIR"):
    directory = os.environ[key]
    assert os.path.realpath(os.path.dirname(directory)) == os.path.join(os.getcwd(), ".toolchain")
    os.makedirs(directory)
os.mkdir("lib")
open("lib/tool", "w").write("tool")
os.chmod("lib/tool", 0o755)
os.symlink("lib/tool", "tool")`}
	prefix := strings.TrimPrefix(r.root, "/")
	image := func(reference string) [][]string {
		var manifest ocispec.Manifest
		var config ocispec.Image
		document, found := held.Load(reference)
		if !found || json.Unmarshal(document.([]byte), &manifest) != nil {
			t.Fatalf("no manifest %s", reference)
		}
		data, _ := held.Load(manifest.Config.Digest.String())
		if json.Unmarshal(data.([]byte), &config) != nil || manifest.Config.MediaType != ocispec.MediaTypeImageConfig || config.Architecture != runtime.GOARCH ||
			config.OS != runtime.GOOS || config.Config.WorkingDir != r.root || !maps.Equal(manifest.Annotations, annotations) {
			t.Fatalf("image %+v %+v", manifest, config)
		}
		var layers [][]string
		for i, layer := range manifest.Layers {
			blob, _ := held.Load(layer.Digest.String())
			decoder, _ := zstd.NewReader(bytes.NewReader(blob.([]byte)))
			uncompressed, _ := io.ReadAll(decoder)
			if layer.MediaType != ocispec.MediaTypeImageLayerZstd || layer.Size != int64(len(blob.([]byte))) || config.RootFS.DiffIDs[i] != digest.FromBytes(uncompressed) {
				t.Fatalf("layer %d %+v", i, layer)
			}
			var names []string
			archive := tar.NewReader(bytes.NewReader(uncompressed))
			for header, err := archive.Next(); err != io.EOF; header, err = archive.Next() {
				name := strings.TrimPrefix(header.Name, prefix)
				if err != nil || header.Uid != 10001 || header.Gid != 10001 || header.ModTime.Unix() != 0 ||
					name == "/lib/tool" && header.Mode != 0o755 || name == "/tool" && header.Linkname != "lib/tool" {
					t.Fatalf("header %+v: %v", header, err)
				}
				names = append(names, name)
			}
			layers = append(layers, names)
		}
		return layers
	}

	developed := overlay(t, source(install, nil, serve, app("developed")))
	if status, reply := r.reload(developed); status != http.StatusOK {
		t.Fatalf("reload: %d %+v", status, reply)
	}
	status, first := r.build(frozen(freeze, tools))
	if status != http.StatusOK || uploads.Swap(0) != 4 {
		t.Fatalf("build: %d %+v", status, first)
	}
	if layers := image(first.Image); len(layers) != 3 ||
		!slices.Equal(layers[0], []string{"/.wh..wh..opq", "/.toolchain/", "/.toolchain/node/", "/.toolchain/python/"}) ||
		!slices.Equal(layers[1], []string{"/lib/", "/lib/tool"}) || !slices.Equal(layers[2], []string{"/", "/main.py", "/tool"}) {
		t.Fatalf("layer entries %q", layers)
	}
	if status, second := r.build(frozen(freeze, tools)); status != http.StatusOK || second.Image != first.Image || uploads.Swap(0) != 0 {
		t.Fatalf("second build of one source: %d %+v", status, second)
	}
	page := runtimecontract.Build{Freeze: &runtimecontract.Freeze{Dependencies: []string{".venv"}}}
	status, mixed := r.build(frozen(page, tools, []string{"mkdir", ".venv"}))
	if layers := image(mixed.Image); status != http.StatusOK || !slices.Equal(layers[0], []string{"/.wh..wh..opq", "/.toolchain/", "/.toolchain/python/"}) {
		t.Fatalf("Python app with a Node-built page: %d, %q", status, layers)
	}

	check := []string{"python3", "-c", `import sys; print("a" * 9999); print("finding", file=sys.stderr); raise SystemExit(2)`}
	status, alone := r.build(frozen(runtimecontract.Build{Freeze: &runtimecontract.Freeze{}, Run: check}))
	if layers := image(alone.Image); status != http.StatusOK || len(layers) != 1 || !slices.Equal(layers[0], []string{"/.wh..wh..opq", "/", "/main.py"}) {
		t.Fatalf("build without dependencies: %d %+v %q", status, alone, layers)
	}
	if alone.Ran == nil || *alone.Ran != (runtimecontract.Ran{ExitCode: 2, Stdout: strings.Repeat("a", 9999) + "\n", Stderr: "finding\n"}) {
		t.Fatalf("failing run: %+v", alone.Ran)
	}
	uploads.Store(0)
	if status, _ := r.get(); status != http.StatusServiceUnavailable {
		t.Fatalf("after a build: %d", status)
	}

	failing := []string{"python3", "-c", `print("no such package"); raise SystemExit(3)`}
	if status, reply := r.build(frozen(freeze, failing)); status != http.StatusInternalServerError || !strings.Contains(reply.Output, "no such package") || uploads.Load() != 0 {
		t.Fatalf("failing install: %d %+v", status, reply)
	}

	if status, reply := r.reload(developed); status != http.StatusOK || !strings.Contains(reply.Output, "installing") {
		t.Fatalf("reload after a build: %d %+v", status, reply)
	}

	t.Setenv(runtimecontract.BaseImageEnv, "registry.example/holon/development:latest")
	if status, reply := r.build(frozen(freeze, tools)); status != http.StatusBadRequest || !strings.Contains(reply.Error, runtimecontract.BaseImageEnv) {
		t.Fatalf("freeze on a base without a digest: %d %+v", status, reply)
	}
}
