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
	"path"
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
	reply := func(body string) (reply runtimecontract.ReloadReply) {
		_ = json.Unmarshal([]byte(body), &reply)
		return reply
	}
	r := run(t, nil)
	serve := []string{"python3", "-c", "import subprocess; raise SystemExit(subprocess.run(['python3', 'main.py']).returncode)"}
	prepare := [][]string{{"echo", "prepared"}}
	install := runtimecontract.Install{Inputs: "sha256:one", Commands: [][]string{{"echo", "installing"}, {"echo", "installed"}}}
	first := source(install, prepare, serve, app("first"))
	first["old.py"] = ""
	if status, body := r.reload(overlay(t, first)); status != http.StatusOK || reply(body).Output != "installing\ninstalled\nprepared\n" {
		t.Fatalf("reload: %d %s", status, body)
	}
	if status, body := r.get(); status != http.StatusOK || body != `["first", "app.example", "session.example"]` {
		t.Fatalf("first source: %d %s", status, body)
	}
	pid := r.pid()
	if status, body := r.reload(overlay(t, first)); status != http.StatusOK || r.pid() != pid {
		t.Fatalf("the overlay the app runs: %d %s", status, body)
	}
	second := overlay(t, source(install, prepare, serve, app("second")))
	if status, body := r.reload(second); status != http.StatusOK || reply(body).Output != "prepared\n" {
		t.Fatalf("unchanged install: %d %s", status, body)
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
	if status, body := r.reload(second); status != http.StatusOK || reply(body).Output != "prepared\n" {
		t.Fatalf("the overlay of an app that exited: %d %s", status, body)
	}

	failing := runtimecontract.Install{Inputs: "sha256:two", Commands: [][]string{
		{"python3", "-c", `print("a" * 4999); print("no such package"); raise SystemExit(3)`}, {"echo", "after"},
	}}
	for range 2 {
		if status, body := r.reload(overlay(t, source(failing, nil, serve, app("uninstalled")))); status != http.StatusInternalServerError ||
			!strings.Contains(reply(body).Error, "exit status 3") || reply(body).Output != "no such package\n" {
			t.Fatalf("failing install: %d %s", status, body)
		}
	}
	if status, body := r.reload(overlay(t, source(install, nil, serve, app("third")))); status != http.StatusOK || reply(body).Output != "installing\ninstalled\n" {
		t.Fatalf("install after a failing one: %d %s", status, body)
	}
	if status, body := r.reload(overlay(t, source(install, nil, serve, "raise SystemExit(4)"))); status != http.StatusInternalServerError {
		t.Fatalf("failing start: %d %s", status, body)
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

func TestBootStartsTheRecordedOverlay(t *testing.T) {
	blobs := map[string][]byte{}
	tags := map[string]string{"tampered": "sha256:" + strings.Repeat("0", 64), "malformed": "sha256:../../other"}
	for session, message := range map[string]string{"recorded": "recorded", "other": "another session's"} {
		blob := overlay(t, source(runtimecontract.Install{}, nil, []string{"python3", "main.py"}, app(message)))
		sum := sha256.Sum256(blob)
		tags[session] = "sha256:" + hex.EncodeToString(sum[:])
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
		if status, body := r.reload(blobs[tags["recorded"]]); status != http.StatusOK || r.pid() != pid {
			t.Fatalf("reload of the recorded overlay: %d %s", status, body)
		}
	})
	t.Run("a session the registry holds no overlay for yet", func(t *testing.T) {
		r := run(t, record("unsaved"))
		if status, body := r.reload(blobs[tags["recorded"]]); status != http.StatusOK {
			t.Fatalf("reload: %d %s", status, body)
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
