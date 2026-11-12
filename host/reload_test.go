package host

import (
	"archive/tar"
	"bytes"
	"cmp"
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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/klauspost/compress/zstd"
)

// apiCaller is the mesh identity the runtime's configuration lets reload; edgeCaller is any other admitted caller.
const apiCaller, edgeCaller = "spiffe://cluster.local/ns/holon-system/sa/holon-api", "spiffe://edge"

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
            "leaked": os.environ.get("HOLON_MCP_RUNTIME_CONFIG"),
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
	return map[string]string{"main.py": childSource(message, beforeReady), "server.py": "", "web/index.html": message}
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

func startRuntime(t *testing.T, blobs map[string][]byte, boot ...reloadRequest) *testRuntime {
	t.Helper()
	return launch(t, true, blobs, boot...)
}

func startImage(t *testing.T) *testRuntime {
	t.Helper()
	return launch(t, false, nil)
}

func startServing(t *testing.T) *testRuntime {
	t.Helper()
	rt := startRuntime(t, nil)
	first := sourceFiles("first", "")
	first["node_modules/marker"] = "from first"
	if status, body := rt.reload(apiCaller, overlayMediaType, overlay(t, first)); status != http.StatusOK {
		t.Fatalf("first source answered %d %s", status, body)
	}
	return rt
}

func launch(t *testing.T, development bool, blobs map[string][]byte, boot ...reloadRequest) *testRuntime {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	image, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !development {
		writeTree(t, image, sourceFiles("image", ""))
	}
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
	t.Setenv(protocol.RuntimeConfigEnv, `{"version":2,"peers":{},"callers":["`+apiCaller+`"]}`)
	t.Setenv(reloadDirEnv, overlays)
	ctx, cancel := context.WithCancel(context.Background())
	rt := &testRuntime{
		t: t, url: "http://127.0.0.1:" + strconv.Itoa(port), image: image, overlays: overlays,
		registry: fakeRegistry(t, blobs), exited: make(chan error, 1),
	}
	t.Setenv(overlayFileEnv, filepath.Join(base, "overlay"))
	if !development {
		_ = errors.Join(os.Unsetenv(overlayFileEnv), os.Unsetenv(reloadDirEnv))
	}
	for _, request := range boot {
		request.Registry = rt.registry
		value, _ := json.Marshal(request)
		writeTree(t, base, map[string]string{"overlay": string(value)})
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
			rt.exited <- err
			return rt
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

func (rt *testRuntime) reload(caller, mediaType string, blob []byte) (int, []byte) {
	rt.t.Helper()
	request, err := http.NewRequest(http.MethodPost, rt.url+reloadPath, bytes.NewReader(blob))
	if err != nil {
		rt.t.Fatal(err)
	}
	request.Header.Set(runtimecontract.CallerHeader, caller)
	request.Header.Set("Content-Type", mediaType)
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
	Leaked  *string `json:"leaked"`
	Modules *string `json:"modules"`
}

func (rt *testRuntime) get(path string) *http.Response {
	rt.t.Helper()
	request, err := http.NewRequest(http.MethodGet, rt.url+path, nil)
	if err != nil {
		rt.t.Fatal(err)
	}
	request.Header.Set(runtimecontract.CallerHeader, edgeCaller)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		rt.t.Fatal(err)
	}
	rt.t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func (rt *testRuntime) abandon(blob []byte, after time.Duration) {
	rt.t.Helper()
	ctx, cancel := context.WithTimeout(rt.t.Context(), after)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.url+reloadPath, bytes.NewReader(blob))
	if err != nil {
		rt.t.Fatal(err)
	}
	request.Header.Set(runtimecontract.CallerHeader, apiCaller)
	request.Header.Set("Content-Type", overlayMediaType)
	if response, err := http.DefaultClient.Do(request); err == nil {
		_ = response.Body.Close()
		rt.t.Fatalf("the reload answered %d before its caller gave up", response.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	for rt.get("/mcp").StatusCode != http.StatusOK {
		if time.Now().After(deadline) {
			rt.t.Fatal("no application is serving after an abandoned reload")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (rt *testRuntime) contract() int {
	rt.t.Helper()
	request, err := http.NewRequest(http.MethodGet, rt.url+contractPath, nil)
	if err != nil {
		rt.t.Fatal(err)
	}
	request.Header.Set(runtimecontract.CallerHeader, apiCaller)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		rt.t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
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
	source := overlay(t, sourceFiles("new", ""))
	exits := overlay(t, sourceFiles("exits", "sys.exit(3)"))
	silent := overlay(t, sourceFiles("silent", "time.sleep(60)"))
	escapes := overlay(t, nil, &tar.Header{Name: "../../outside", Typeflag: tar.TypeReg, Mode: 0o644})
	packaged := overlay(t, map[string]string{
		"main.py": childSource("src layout", "import overlaid"), "server.py": "", "src/overlaid/__init__.py": "", "web/index.html": "src layout",
	})
	rootPackage := overlay(t, map[string]string{
		"main.py": childSource("root package", `assert "PYTHONPATH" not in os.environ`), "server.py": "", "flat/__init__.py": "", "src/index.ts": "",
		"web/index.html": "root package",
	})
	for _, tc := range []struct {
		name      string
		caller    string // the request's mesh identity
		mediaType string
		blob      []byte
		previous  map[string]string
		patience  time.Duration
		status    int
		exit      string
		child     string // the source serving afterwards, or "" when the runtime has exited
	}{
		{name: "new source", caller: apiCaller, blob: source, status: http.StatusOK, child: "new"},
		{name: "src layout", caller: apiCaller, blob: packaged, status: http.StatusOK, child: "src layout"},
		{name: "package at the root", caller: apiCaller, blob: rootPackage, status: http.StatusOK, child: "root package"},
		{name: "caller not allowed to reload", caller: edgeCaller, blob: source, status: http.StatusNotFound, child: "first"},
		{name: "no mesh caller", blob: source, status: http.StatusForbidden, child: "first"},
		{name: "not an overlay", caller: apiCaller, mediaType: "application/json", blob: source, status: http.StatusUnsupportedMediaType, child: "first"},
		{name: "corrupt overlay", caller: apiCaller, blob: source[:len(source)/2], status: http.StatusBadRequest, child: "first"},
		{name: "archive escapes", caller: apiCaller, blob: escapes, status: http.StatusBadRequest, child: "first"},
		{
			name: "new child exits", caller: apiCaller, blob: exits,
			status: http.StatusInternalServerError, exit: "exit status 3", child: "first",
		},
		{name: "caller gives up before the new child is ready", blob: silent, patience: 500 * time.Millisecond, child: "first"},
		{
			name: "rollback fails", caller: apiCaller, blob: exits, previous: map[string]string{"main.py": "raise SystemExit(4)"},
			status: http.StatusInternalServerError, exit: "exit status 3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := startServing(t)
			writeTree(t, rt.child().Cwd, tc.previous)
			if tc.patience != 0 {
				rt.abandon(tc.blob, tc.patience)
			} else {
				status, body := rt.reload(tc.caller, cmp.Or(tc.mediaType, overlayMediaType), tc.blob)
				var reply struct {
					reloadFailure
					reloadReply
				}
				_ = json.Unmarshal(body, &reply)
				if status != tc.status || reply.Exit != tc.exit || status == http.StatusOK && reply.Digest != blobDigest(tc.blob) {
					t.Fatalf("%d %s", status, body)
				}
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
			if got.Message != tc.child || got.Leaked != nil || got.Modules == nil || *got.Modules != "from first" || string(page) != tc.child {
				t.Fatalf("serving %+v %q", got, page)
			}
			if directories := rt.overlayDirectories(); len(directories) != 1 {
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
	rt := startRuntime(t, nil)
	if status, body := rt.reload(apiCaller, overlayMediaType, first); status != http.StatusOK {
		t.Fatalf("first reload: %d %s", status, body)
	}
	finished := make(chan int)
	go func() {
		status, _ := rt.reload(apiCaller, overlayMediaType, gated)
		finished <- status
	}()
	<-started
	if response := rt.get("/mcp"); response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" {
		t.Fatalf("during restart: %d %v", response.StatusCode, response.Header)
	}
	if status := rt.contract(); status != http.StatusServiceUnavailable {
		t.Fatalf("contract during restart: %d", status)
	}
	if status, body := rt.reload(apiCaller, overlayMediaType, gated); status != http.StatusConflict {
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

func TestBootOverlayFile(t *testing.T) {
	kept := overlay(t, sourceFiles("kept", ""))
	recorded := reloadRequest{Repository: "holon/app", Token: "pull", Digest: blobDigest(kept), MediaType: overlayMediaType}
	tampered := recorded
	tampered.Digest = "sha256:" + strings.Repeat("a", 64)
	unsent := recorded
	unsent.Digest = "sha256:" + strings.Repeat("b", 64)
	bin := t.TempDir()
	writeTree(t, bin, map[string]string{"npm": "#!/bin/sh\nexit 1\n"})
	if err := os.Chmod(filepath.Join(bin, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	exits := overlay(t, sourceFiles("exits", "raise SystemExit(1)"))
	uninstallable := sourceFiles("uninstallable", "")
	uninstallable["package.json"] = "{}"
	broken := map[string][]byte{}
	for _, blob := range [][]byte{exits, overlay(t, uninstallable)} {
		broken[blobDigest(blob)] = blob
	}
	stored := func(blob []byte) []reloadRequest {
		return []reloadRequest{{Repository: recorded.Repository, Token: recorded.Token, Digest: blobDigest(blob), MediaType: overlayMediaType}}
	}
	for _, tc := range []struct {
		name  string
		boot  []reloadRequest
		send  map[string]string
		child string
	}{
		{"no record", nil, sourceFiles("sent", ""), "sent"},
		{"recorded overlay", []reloadRequest{recorded}, nil, "kept"},
		{"overlay the registry does not hold", []reloadRequest{unsent}, sourceFiles("sent", ""), "sent"},
		{"recorded overlay that does not install", stored(overlay(t, uninstallable)), sourceFiles("sent", ""), "sent"},
		{"recorded overlay that exits before it is ready", stored(exits), sourceFiles("sent", ""), "sent"},
		{"digest mismatch", []reloadRequest{tampered}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blobs := maps.Clone(broken)
			blobs[recorded.Digest], blobs[tampered.Digest] = kept, kept
			rt := startRuntime(t, blobs, tc.boot...)
			if tc.send != nil {
				if status, contract := rt.get("/mcp").StatusCode, rt.contract(); status != http.StatusServiceUnavailable || contract != http.StatusNotFound {
					t.Fatalf("a runtime without source answered %d, and %d for its contract", status, contract)
				}
				if status, body := rt.reload(apiCaller, overlayMediaType, overlay(t, sourceFiles("", "raise SystemExit(1)"))); status == http.StatusOK {
					t.Fatalf("a source that exits before ready started: %s", body)
				}
				if status, body := rt.reload(apiCaller, overlayMediaType, overlay(t, tc.send)); status != http.StatusOK {
					t.Fatalf("first source answered %d %s", status, body)
				}
			}
			if tc.child == "" {
				select {
				case err := <-rt.exited:
					rt.exited <- err
					if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
						t.Fatalf("boot: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("runtime booted an overlay whose digest does not match")
				}
				return
			}
			if got := rt.child(); got.Message != tc.child {
				t.Fatalf("first request: %+v", got)
			}
		})
	}
}

func blockingBuild(t *testing.T, message string) []byte {
	t.Helper()
	bin := t.TempDir()
	writeTree(t, bin, map[string]string{
		"npm": "#!/bin/sh\n[ \"$1\" = run ] || exit 0\nwhile [ ! -e release ]; do sleep 0.01; done\necho built > web/index.html\n",
	})
	if err := os.Chmod(filepath.Join(bin, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	source := sourceFiles(message, "")
	source["package.json"] = `{"scripts":{"build":"build"}}`
	return overlay(t, source)
}

func TestReloadAnswersBeforeTheBuildAndRequestsAfterIt(t *testing.T) {
	rt := startRuntime(t, nil)
	if status, body := rt.reload(apiCaller, overlayMediaType, blockingBuild(t, "unbuilt")); status != http.StatusOK {
		t.Fatalf("reload answered %d %s", status, body)
	}
	answers := make(chan string)
	for _, path := range []string{"/", "/mcp"} {
		go func() {
			body, _ := io.ReadAll(rt.get(path).Body)
			answers <- path + " " + strings.TrimSpace(string(body))
		}()
	}
	select {
	case answer := <-answers:
		t.Fatalf("answered before the build finished: %s", answer)
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(rt.overlays, rt.overlayDirectories()[0], "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := []string{<-answers, <-answers}
	slices.Sort(got)
	if got[0] != "/ built" || !strings.Contains(got[1], `"message": "unbuilt"`) {
		t.Fatalf("after the build: %q", got)
	}
}

func TestImageTakesNoSource(t *testing.T) {
	rt := startImage(t)
	if status, body := rt.reload(apiCaller, overlayMediaType, overlay(t, sourceFiles("sent", ""))); status == http.StatusOK {
		t.Fatalf("an image took source: %s", body)
	}
	if got := rt.child(); got.Message != "image" {
		t.Fatalf("serving %+v", got)
	}
}

func TestReloadStopsThePreviousBuild(t *testing.T) {
	blob := blockingBuild(t, "building")
	rt := startRuntime(t, nil)
	for _, source := range [][]byte{blob, overlay(t, sourceFiles("next", ""))} {
		if status, body := rt.reload(apiCaller, overlayMediaType, source); status != http.StatusOK {
			t.Fatalf("reload answered %d %s", status, body)
		}
	}
	if child := rt.child(); child.Message != "next" {
		t.Fatalf("%q is serving after a reload during the previous build", child.Message)
	}
}
