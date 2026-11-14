package host

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/klauspost/compress/zstd"
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

// source is an app's files with the scripts that prepare and start it.
func source(prepare, start, main string) map[string]string {
	return map[string]string{runtimecontract.PrepareFile: prepare, runtimecontract.StartFile: start, "main.py": main}
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

func (r *running) reload(body []byte) (int, string) {
	r.t.Helper()
	response, err := http.Post(r.url+runtimecontract.ReloadPath, runtimecontract.OverlayMediaType, bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
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
	reply := func(body string) (reply runtimecontract.ReloadReply) {
		_ = json.Unmarshal([]byte(body), &reply)
		return reply
	}
	r := run(t, nil)
	if status, _ := r.get(); status != http.StatusServiceUnavailable {
		t.Fatalf("before any source: %d", status)
	}
	first := source("echo installed", "exec python3 main.py", app("first"))
	first["old.py"] = ""
	if status, body := r.reload(overlay(t, first)); status != http.StatusOK || reply(body).Output != "installed\n" {
		t.Fatalf("reload: %d %s", status, body)
	}
	if status, body := r.get(); status != http.StatusOK || body != `["first", "app.example", "session.example"]` {
		t.Fatalf("first source: %d %s", status, body)
	}

	failing := `head -c 4999 /dev/zero | tr '\0' a; printf '\nno such package\n'; exit 3`
	if status, body := r.reload(overlay(t, source(failing, "exec python3 main.py", app("unprepared")))); status != http.StatusInternalServerError ||
		!strings.Contains(reply(body).Error, "exit status 3") || reply(body).Output != "no such package\n" {
		t.Fatalf("failing install: %d %s", status, body)
	}
	if status, body := r.reload(overlay(t, source("echo installed", "exec python3 main.py", "raise SystemExit(4)"))); status != http.StatusInternalServerError ||
		!strings.Contains(reply(body).Error, "exit status 4") || reply(body).Output != "" {
		t.Fatalf("failing start: %d %s", status, body)
	}
	if status, _ := r.get(); status != http.StatusServiceUnavailable {
		t.Fatalf("after a failing reload: %d", status)
	}

	if status, body := r.reload(overlay(t, source("", "exec python3 main.py", app("second")))); status != http.StatusOK {
		t.Fatalf("reload: %d %s", status, body)
	}
	if _, body := r.get(); !strings.Contains(body, "second") {
		t.Fatalf("second source: %s", body)
	}
	if _, err := os.Stat(filepath.Join(r.root, "old.py")); err == nil {
		t.Fatal("what only an earlier overlay wrote is still there")
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
	if _, err := os.Stat(filepath.Join(r.root, "..", "escaped")); err == nil {
		t.Fatal("an overlay wrote outside the source directory")
	}
}

func TestReloadStopsEverythingTheCommandStarted(t *testing.T) {
	r := run(t, nil)
	r.reload(overlay(t, source("", "python3 main.py; true", app("first"))))
	data, _ := os.ReadFile(filepath.Join(r.root, "pid"))
	pid, _ := strconv.Atoi(string(data))
	if status, _ := r.reload(overlay(t, source("", "python3 main.py; true", app("second")))); status != http.StatusOK {
		t.Fatalf("reload: %d", status)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("the first app still runs")
	}
}

func TestBootStartsTheRecordedOverlay(t *testing.T) {
	blob := overlay(t, source("", "exec python3 main.py", app("recorded")))
	sum := sha256.Sum256(blob)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	tampered := "sha256:" + strings.Repeat("0", 64)
	tagged := func(layer string) string {
		return `{"layers":[{"mediaType":"` + runtimecontract.OverlayMediaType + `","digest":"` + layer + `"}]}`
	}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pull" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/holon/app/manifests/" + runtimecontract.OverlayTag:
			_, _ = io.WriteString(w, tagged(digest))
		case "/v2/holon/tampered/manifests/" + runtimecontract.OverlayTag:
			_, _ = io.WriteString(w, tagged(tampered))
		case "/v2/holon/app/blobs/" + digest, "/v2/holon/tampered/blobs/" + tampered:
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(registry.Close)
	record := func(repository string) *runtimecontract.OverlayRecord {
		return &runtimecontract.OverlayRecord{Registry: registry.URL, Repository: repository, Token: "pull"}
	}

	t.Run("recorded", func(t *testing.T) {
		r := run(t, record("holon/app"))
		for status, body := r.get(); !strings.Contains(body, "recorded"); status, body = r.get() {
			if status != http.StatusServiceUnavailable {
				t.Fatalf("recorded source: %d %s", status, body)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("not in the registry yet", func(t *testing.T) {
		r := run(t, record("holon/unsaved"))
		if status, _ := r.get(); status != http.StatusServiceUnavailable {
			t.Fatalf("without source: %d", status)
		}
		if status, body := r.reload(blob); status != http.StatusOK {
			t.Fatalf("reload: %d %s", status, body)
		}
	})
	t.Run("digest mismatch", func(t *testing.T) {
		setup(t, "0", filepath.Join(t.TempDir(), "source"), record("holon/tampered"))
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := Run(ctx); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("Run = %v", err)
		}
	})
}
