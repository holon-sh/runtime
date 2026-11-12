package host

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// app is a fake application. Its message is part of its source, so a reply shows which source is running. It writes
// its pid where a test can find it, and marks the directory as an install would.
func app(message string) string {
	return `import json, os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

open("pid", "w").write(str(os.getpid()))
open("installed", "w").write("kept")

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({
            "message": "` + message + `",
            "cwd": os.getcwd(),
            "host": self.headers.get("Host"),
            "forwarded": self.headers.get("X-Forwarded-Host"),
        }).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass

ThreadingHTTPServer((os.environ["HOST"], int(os.environ["PORT"])), Handler).serve_forever()
`
}

// overlay packs files, then entries without contents, into a zstd tar.
func overlay(t *testing.T, files map[string]string, entries ...*tar.Header) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(encoder)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range entries {
		if err := archive.WriteHeader(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(archive.Close(), encoder.Close()); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

type running struct {
	t      *testing.T
	url    string
	root   string
	exited chan error
}

// run starts the supervisor with command, booting from record when it is set.
func run(t *testing.T, record *runtimecontract.OverlayRecord, command ...string) *running {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	root := filepath.Join(t.TempDir(), "source")
	t.Setenv("PORT", port)
	t.Setenv(reloadDirEnv, root)
	t.Setenv(runtimecontract.OverlayFileEnv, "")
	if record != nil {
		file := filepath.Join(t.TempDir(), "overlay")
		data, _ := json.Marshal(record)
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(runtimecontract.OverlayFileEnv, file)
	}
	if len(command) == 0 {
		command = []string{"python3", "main.py"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{t: t, url: "http://127.0.0.1:" + port, root: root, exited: make(chan error, 1)}
	go func() { r.exited <- Run(ctx, command) }()
	t.Cleanup(func() {
		cancel()
		<-r.exited
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if connection, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
			_ = connection.Close()
			return r
		}
		select {
		case err := <-r.exited:
			t.Fatalf("runtime exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *running) reload(body []byte, contentType string) (int, string) {
	r.t.Helper()
	request, _ := http.NewRequest(http.MethodPost, r.url+runtimecontract.ReloadPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

// get answers the app's reply, or its status when the app is not serving.
func (r *running) get() (int, map[string]string) {
	r.t.Helper()
	request, _ := http.NewRequest(http.MethodGet, r.url+"/", nil)
	request.Host = "app.example"
	request.Header.Set("X-Forwarded-Host", "session.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var reply map[string]string
	_ = json.NewDecoder(response.Body).Decode(&reply)
	return response.StatusCode, reply
}

func (r *running) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(r.root, name))
	return err == nil
}

func alive(t *testing.T, pidFile string) bool {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return syscall.Kill(pid, 0) == nil
}

func TestReloadReplacesTheAppFromItsSource(t *testing.T) {
	r := run(t, nil)
	if status, _ := r.get(); status != http.StatusServiceUnavailable {
		t.Fatalf("before any source: %d", status)
	}
	first := overlay(t, map[string]string{"main.py": app("first"), "old.txt": "old", "lib/gone.py": ""})
	if status, body := r.reload(first, runtimecontract.OverlayMediaType); status != http.StatusNoContent {
		t.Fatalf("first reload: %d %s", status, body)
	}
	status, reply := r.get()
	if root, _ := filepath.EvalSymlinks(r.root); status != http.StatusOK || reply["message"] != "first" || reply["cwd"] != root {
		t.Fatalf("first source: %d %v", status, reply)
	}
	if reply["host"] != "app.example" || reply["forwarded"] != "session.example" {
		t.Fatalf("the app does not see the origin it is served at: %v", reply)
	}
	firstPID := filepath.Join(t.TempDir(), "pid")
	data, _ := os.ReadFile(filepath.Join(r.root, "pid"))
	_ = os.WriteFile(firstPID, data, 0o600)

	second := overlay(t, map[string]string{"main.py": app("second")})
	if status, body := r.reload(second, runtimecontract.OverlayMediaType); status != http.StatusNoContent {
		t.Fatalf("second reload: %d %s", status, body)
	}
	if status, reply := r.get(); status != http.StatusOK || reply["message"] != "second" {
		t.Fatalf("second source: %d %v", status, reply)
	}
	if alive(t, firstPID) {
		t.Fatal("the first app still runs")
	}
	if r.exists("old.txt") || r.exists("lib") {
		t.Fatal("what only the first overlay wrote is still there")
	}
	if !r.exists("installed") {
		t.Fatal("what the app made in its directory is gone")
	}
}

func TestFailedReloadLeavesTheAppDownUntilTheNext(t *testing.T) {
	r := run(t, nil)
	good := overlay(t, map[string]string{"main.py": app("good")})
	if status, body := r.reload(good, runtimecontract.OverlayMediaType); status != http.StatusNoContent {
		t.Fatalf("reload: %d %s", status, body)
	}
	for _, test := range []struct {
		name        string
		body        []byte
		contentType string
		status      int
		reply       string
		down        bool
	}{
		{"media type", good, "application/x-tar", http.StatusUnsupportedMediaType, "invalid reload request", false},
		{"escaping entry", overlay(t, map[string]string{"../main.py": ""}), runtimecontract.OverlayMediaType, http.StatusBadRequest, "escapes", true},
		{"escaping link", overlay(t, nil, &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside"}), runtimecontract.OverlayMediaType, http.StatusBadRequest, "escapes", true},
		{"exits before ready", overlay(t, map[string]string{"main.py": "raise SystemExit(3)"}), runtimecontract.OverlayMediaType, http.StatusInternalServerError, "exit status 3", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := r.reload(test.body, test.contentType)
			if status != test.status || !strings.Contains(body, test.reply) {
				t.Fatalf("reload: %d %s", status, body)
			}
			if served, _ := r.get(); (served == http.StatusServiceUnavailable) != test.down {
				t.Fatalf("after the reload the app answers %d", served)
			}
			if status, body := r.reload(good, runtimecontract.OverlayMediaType); status != http.StatusNoContent {
				t.Fatalf("the next reload: %d %s", status, body)
			}
		})
	}
}

func TestStopEndsEverythingTheCommandStarted(t *testing.T) {
	// The shell keeps a child of its own, as an install would.
	r := run(t, nil, "sh", "-c", "python3 main.py; true")
	if status, body := r.reload(overlay(t, map[string]string{"main.py": app("first")}), runtimecontract.OverlayMediaType); status != http.StatusNoContent {
		t.Fatalf("reload: %d %s", status, body)
	}
	pid := filepath.Join(t.TempDir(), "pid")
	data, _ := os.ReadFile(filepath.Join(r.root, "pid"))
	_ = os.WriteFile(pid, data, 0o600)
	if status, body := r.reload(overlay(t, map[string]string{"main.py": app("second")}), runtimecontract.OverlayMediaType); status != http.StatusNoContent {
		t.Fatalf("reload: %d %s", status, body)
	}
	if alive(t, pid) {
		t.Fatal("the stopped command's child still runs")
	}
}

func TestAppThatExitsGoesOutOfService(t *testing.T) {
	r := run(t, nil)
	source := app("exits") + "\n"
	source = strings.Replace(source, "class Handler", "import threading\nthreading.Timer(0.3, lambda: os._exit(0)).start()\n\nclass Handler", 1)
	if status, body := r.reload(overlay(t, map[string]string{"main.py": source}), runtimecontract.OverlayMediaType); status != http.StatusNoContent {
		t.Fatalf("reload: %d %s", status, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if status, _ := r.get(); status == http.StatusServiceUnavailable {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the exited app is still in service")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBootStartsTheRecordedOverlay(t *testing.T) {
	blob := overlay(t, map[string]string{"main.py": app("recorded")})
	sum := sha256.Sum256(blob)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pull" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/holon/app/blobs/" + digest:
			_, _ = w.Write(blob)
		case "/v2/holon/app/blobs/sha256:" + strings.Repeat("0", 64):
			_, _ = w.Write(overlay(t, map[string]string{"main.py": app("tampered")}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(registry.Close)
	record := func(digest string) *runtimecontract.OverlayRecord {
		return &runtimecontract.OverlayRecord{
			Registry: registry.URL, Repository: "holon/app", Token: "pull", Digest: digest, MediaType: runtimecontract.OverlayMediaType,
		}
	}

	t.Run("recorded", func(t *testing.T) {
		r := run(t, record(digest))
		deadline := time.Now().Add(10 * time.Second)
		for {
			if status, reply := r.get(); status == http.StatusOK && reply["message"] == "recorded" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the recorded overlay did not start")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	t.Run("not yet in the registry", func(t *testing.T) {
		r := run(t, record("sha256:"+strings.Repeat("f", 64)))
		if status, _ := r.get(); status != http.StatusServiceUnavailable {
			t.Fatalf("without source: %d", status)
		}
		if status, body := r.reload(blob, runtimecontract.OverlayMediaType); status != http.StatusNoContent {
			t.Fatalf("reload: %d %s", status, body)
		}
	})
	t.Run("digest mismatch", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "source")
		t.Setenv(reloadDirEnv, root)
		file := filepath.Join(t.TempDir(), "overlay")
		data, _ := json.Marshal(record("sha256:" + strings.Repeat("0", 64)))
		_ = os.WriteFile(file, data, 0o600)
		t.Setenv(runtimecontract.OverlayFileEnv, file)
		t.Setenv("PORT", "0")
		if err := Run(context.Background(), []string{"python3", "main.py"}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("Run = %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "main.py")); err == nil {
			t.Fatal("the mismatched source was left in place")
		}
	})
}
