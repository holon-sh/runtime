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
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const testSecret = "reload-secret"

// childSource is a fake application. Its message is part of its source, so a reply shows which source is running.
func childSource(message, beforeReady string) string {
	return `import json, os, sys, time, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

` + beforeReady + `

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({
            "message": "` + message + `",
            "cwd": os.getcwd(),
            "secret": os.environ.get("HOLON_RELOAD_SECRET") or os.environ.get("HOLON_OVERLAY"),
            "modules": open("node_modules/marker").read() if os.path.exists("node_modules/marker") else None,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass

server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
ready = urllib.request.Request(
    os.environ["HOLON_HOST_URL"] + "/ready",
    data=json.dumps({"port": server.server_address[1], "frontend": "web"}).encode(),
    method="POST",
)
urllib.request.urlopen(ready).close()
server.serve_forever()
`
}

// sourceFiles is a whole application tree whose message and static page are recognisable.
func sourceFiles(message, beforeReady string) map[string]string {
	return map[string]string{"main.py": childSource(message, beforeReady), "web/index.html": message}
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

func blobDigest(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeRegistry serves blobs by digest to the bearer token "pull".
func fakeRegistry(t *testing.T, blobs map[string][]byte) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		digest, found := strings.CutPrefix(r.URL.Path, "/v2/holon/app/blobs/")
		if !found || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer pull" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		blob, found := blobs[digest]
		if !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(blob)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

type testRuntime struct {
	t        *testing.T
	url      string
	image    string
	overlays string
	registry string
	exited   chan error
}

// startRuntime runs the supervisor on a child that serves the "image" source from a fresh working directory. A boot
// request becomes HOLON_OVERLAY, aimed at the runtime's registry.
func startRuntime(t *testing.T, secret string, blobs map[string][]byte, boot ...reloadRequest) *testRuntime {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	image, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, image, sourceFiles("image", ""))
	writeTree(t, image, map[string]string{"node_modules/marker": "from image"})
	t.Chdir(image)
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	overlays := filepath.Join(base, "overlays")
	t.Setenv("PORT", strconv.Itoa(port))
	t.Setenv(reloadSecretEnv, secret)
	t.Setenv(reloadDirEnv, overlays)
	ctx, cancel := context.WithCancel(context.Background())
	rt := &testRuntime{
		t: t, url: "http://127.0.0.1:" + strconv.Itoa(port), image: image, overlays: overlays,
		registry: fakeRegistry(t, blobs), exited: make(chan error, 1),
	}
	for _, request := range boot {
		request.Registry = rt.registry
		value, _ := json.Marshal(request)
		t.Setenv(overlayEnv, string(value))
	}
	go func() { rt.exited <- Run(ctx, []string{python, "./main.py"}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-rt.exited:
		case <-time.After(10 * time.Second):
			t.Error("runtime did not stop")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if response, err := http.Get(rt.url + "/mcp"); err == nil {
			_ = response.Body.Close()
			return rt
		}
		select {
		case err := <-rt.exited:
			t.Fatalf("runtime exited: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not start")
		}
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (rt *testRuntime) reload(digest string) (int, []byte) {
	rt.t.Helper()
	body, _ := json.Marshal(reloadRequest{
		Registry: rt.registry, Repository: "holon/app", Token: "pull", Digest: digest, MediaType: overlayMediaType,
	})
	request, err := http.NewRequest(http.MethodPost, rt.url+reloadPath, bytes.NewReader(body))
	if err != nil {
		rt.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testSecret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		rt.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		rt.t.Fatal(err)
	}
	return response.StatusCode, data
}

type childReply struct {
	Message string  `json:"message"`
	Cwd     string  `json:"cwd"`
	Secret  *string `json:"secret"`
	Modules *string `json:"modules"`
}

func (rt *testRuntime) get(path string) *http.Response {
	rt.t.Helper()
	response, err := http.Get(rt.url + path)
	if err != nil {
		rt.t.Fatal(err)
	}
	rt.t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func (rt *testRuntime) child() childReply {
	rt.t.Helper()
	var reply childReply
	if response := rt.get("/mcp"); response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&reply) != nil {
		rt.t.Fatalf("child answered %d", response.StatusCode)
	}
	return reply
}

func (rt *testRuntime) overlayDirectories() []string {
	rt.t.Helper()
	entries, err := os.ReadDir(rt.overlays)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		rt.t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestReload(t *testing.T) {
	// The new source lacks the image's frontend and node_modules, which it is served with.
	source := overlay(t, map[string]string{"main.py": childSource("new", "")})
	exits := overlay(t, sourceFiles("exits", "sys.exit(3)"))
	silent := overlay(t, sourceFiles("silent", "time.sleep(60)"))
	escapes := overlay(t, nil, &tar.Header{Name: "../../outside", Typeflag: tar.TypeReg, Mode: 0o644})
	tampered := "sha256:" + strings.Repeat("a", 64)
	blobs := map[string][]byte{
		blobDigest(source): source, blobDigest(exits): exits, blobDigest(silent): silent, blobDigest(escapes): escapes, tampered: source,
	}
	for _, tc := range []struct {
		name    string
		secret  string // the runtime's; the request presents testSecret
		digest  string
		image   map[string]string // written over the image after boot
		timeout time.Duration
		status  int
		exit    string // the failed child's status in the reply
		child   string // the source serving afterwards, or "" when the runtime has exited
	}{
		{name: "new source", secret: testSecret, digest: blobDigest(source), status: http.StatusOK, child: "new"},
		{name: "wrong secret", secret: "other", digest: blobDigest(source), status: http.StatusUnauthorized, child: "image"},
		{name: "no secret", digest: blobDigest(source), status: http.StatusNotFound, child: "image"},
		{name: "malformed request", secret: testSecret, digest: "sha256:abc", status: http.StatusBadRequest, child: "image"},
		{name: "digest mismatch", secret: testSecret, digest: tampered, status: http.StatusBadGateway, child: "image"},
		{name: "archive escapes", secret: testSecret, digest: blobDigest(escapes), status: http.StatusInternalServerError, child: "image"},
		{
			name: "new child exits", secret: testSecret, digest: blobDigest(exits),
			status: http.StatusInternalServerError, exit: "exit status 3", child: "image",
		},
		{
			name: "new child never ready", secret: testSecret, digest: blobDigest(silent), timeout: 500 * time.Millisecond,
			status: http.StatusGatewayTimeout, child: "image",
		},
		{
			name: "rollback fails", secret: testSecret, digest: blobDigest(exits), image: map[string]string{"main.py": "raise SystemExit(4)"},
			status: http.StatusInternalServerError, exit: "exit status 3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.timeout != 0 {
				previous := reloadTimeout
				reloadTimeout = tc.timeout
				t.Cleanup(func() { reloadTimeout = previous })
			}
			rt := startRuntime(t, tc.secret, blobs)
			writeTree(t, rt.image, tc.image)
			status, body := rt.reload(tc.digest)
			var failure reloadFailure
			_ = json.Unmarshal(body, &failure)
			if status != tc.status || failure.Exit != tc.exit {
				t.Fatalf("%d %s", status, body)
			}
			if tc.child == "" {
				select {
				case err := <-rt.exited:
					rt.exited <- err
					if err == nil || !strings.Contains(err.Error(), "restore application") {
						t.Fatalf("exit: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("runtime kept running with no application")
				}
				return
			}
			got := rt.child()
			page, _ := io.ReadAll(rt.get("/").Body)
			if got.Message != tc.child || got.Secret != nil || got.Modules == nil || *got.Modules != "from image" || string(page) != "image" {
				t.Fatalf("serving %+v %q", got, page)
			}
			running := 0
			if filepath.Dir(got.Cwd) == rt.overlays {
				running = 1
			}
			if directories := rt.overlayDirectories(); len(directories) != running {
				t.Fatalf("overlays: %v", directories)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(rt.overlays), "outside")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wrote outside the overlay root: %v", err)
			}
		})
	}
}

func TestReloadAnswers503DuringRestart(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	gate := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(gate.Close)
	open := sync.OnceFunc(func() { close(release) })
	t.Cleanup(open)
	first := overlay(t, sourceFiles("first", ""))
	gated := overlay(t, sourceFiles("gated", "urllib.request.urlopen("+strconv.Quote(gate.URL)+").close()"))
	rt := startRuntime(t, testSecret, map[string][]byte{blobDigest(first): first, blobDigest(gated): gated})
	if status, body := rt.reload(blobDigest(first)); status != http.StatusOK {
		t.Fatalf("first reload: %d %s", status, body)
	}
	finished := make(chan int)
	go func() {
		status, _ := rt.reload(blobDigest(gated))
		finished <- status
	}()
	<-started
	if response := rt.get("/mcp"); response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" {
		t.Fatalf("during restart: %d %v", response.StatusCode, response.Header)
	}
	if status, body := rt.reload(blobDigest(gated)); status != http.StatusConflict {
		t.Fatalf("concurrent reload: %d %s", status, body)
	}
	open()
	if status := <-finished; status != http.StatusOK {
		t.Fatalf("reload: %d", status)
	}
	if got, directories := rt.child(), rt.overlayDirectories(); got.Message != "gated" || len(directories) != 1 {
		t.Fatalf("after restart: %+v %v", got, directories)
	}
}

// bootLog collects the runtime's log while a test runs.
type bootLog struct {
	sync.Mutex
	strings.Builder
}

func (l *bootLog) Write(p []byte) (int, error) {
	l.Lock()
	defer l.Unlock()
	return l.Builder.Write(p)
}

func (l *bootLog) String() string {
	l.Lock()
	defer l.Unlock()
	return l.Builder.String()
}

func TestBootAppliesOverlay(t *testing.T) {
	kept, crashing := overlay(t, sourceFiles("kept", "")), overlay(t, sourceFiles("crashing", "sys.exit(3)"))
	tampered := "sha256:" + strings.Repeat("a", 64)
	blobs := map[string][]byte{blobDigest(kept): kept, blobDigest(crashing): crashing, tampered: kept}
	for _, tc := range []struct {
		name, digest, token string
		child               string // the source the first request reaches
		because             string // in the log when the image boots instead
	}{
		{"last good overlay", blobDigest(kept), "pull", "kept", ""},
		{"token refused", blobDigest(kept), "other", "image", "registry answered 401"},
		{"digest mismatch", tampered, "pull", "image", "digest mismatch"},
		{"overlay child fails", blobDigest(crashing), "pull", "image", "application exited before ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := &bootLog{}
			log.SetOutput(logged)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			rt := startRuntime(t, testSecret, blobs, reloadRequest{
				Repository: "holon/app", Token: tc.token, Digest: tc.digest, MediaType: overlayMediaType,
			})
			if got := rt.child(); got.Message != tc.child || got.Secret != nil {
				t.Fatalf("first request: %+v", got)
			}
			if tc.because == "" && logged.String() != "" || !strings.Contains(logged.String(), tc.because) {
				t.Fatalf("log: %q", logged.String())
			}
		})
	}
}
