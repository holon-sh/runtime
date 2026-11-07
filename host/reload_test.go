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
        if "exit" in self.path:
            os._exit(7)
        body = json.dumps({
            "message": "` + message + `",
            "cwd": os.getcwd(),
            "pwd": os.environ.get("PWD"),
            "secret": os.environ.get("HOLON_RELOAD_SECRET") or os.environ.get("HOLON_OVERLAY"),
            "modules": open("node_modules/marker").read() if os.path.exists("node_modules/marker") else None,
            "bytecode": os.path.exists("__pycache__"),
            "executable": os.access("tools/run", os.X_OK),
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
    data=json.dumps({"port": server.server_address[1], "frontend": globals().get("FRONTEND", "web")}).encode(),
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
	writeTree(t, image, map[string]string{"node_modules/marker": "from image", "__pycache__/main.pyc": ""})
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

func (rt *testRuntime) post(secret, body string) (int, []byte) {
	rt.t.Helper()
	request, err := http.NewRequest(http.MethodPost, rt.url+reloadPath, strings.NewReader(body))
	if err != nil {
		rt.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+secret)
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

func (rt *testRuntime) reload(digest string) (int, []byte) {
	rt.t.Helper()
	body, _ := json.Marshal(reloadRequest{
		Registry: rt.registry, Repository: "holon/app", Token: "pull", Digest: digest, MediaType: overlayMediaType,
	})
	return rt.post(testSecret, string(body))
}

type childReply struct {
	Message    string  `json:"message"`
	Cwd        string  `json:"cwd"`
	Pwd        string  `json:"pwd"`
	Secret     *string `json:"secret"`
	Modules    *string `json:"modules"`
	Bytecode   bool    `json:"bytecode"`
	Executable bool    `json:"executable"`
}

func (rt *testRuntime) child() childReply {
	rt.t.Helper()
	response, err := http.Get(rt.url + "/mcp")
	if err != nil {
		rt.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var reply childReply
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&reply) != nil {
		rt.t.Fatalf("child answered %d", response.StatusCode)
	}
	return reply
}

func (rt *testRuntime) page() string {
	rt.t.Helper()
	response, err := http.Get(rt.url + "/")
	if err != nil {
		rt.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return string(data)
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

func TestReloadRestartsFromNewSource(t *testing.T) {
	// A read-only directory must still be removable once a later overlay replaces this one.
	first := overlay(t, sourceFiles("first", ""),
		&tar.Header{Name: "readonly/", Typeflag: tar.TypeDir, Mode: 0o500},
		&tar.Header{Name: "readonly/run.sh", Typeflag: tar.TypeReg, Mode: 0o755},
		&tar.Header{Name: "tools/run", Typeflag: tar.TypeSymlink, Linkname: "../readonly/run.sh", Mode: 0o777},
	)
	second := overlay(t, sourceFiles("second", ""))
	rt := startRuntime(t, testSecret, map[string][]byte{blobDigest(first): first, blobDigest(second): second})
	if got := rt.child(); got.Message != "image" || got.Cwd != rt.image || got.Secret != nil {
		t.Fatalf("boot: %+v", got)
	}
	started := time.Now()
	status, body := rt.reload(blobDigest(first))
	if status != http.StatusOK {
		t.Fatalf("reload: %d %s", status, body)
	}
	var reply reloadReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	t.Logf("request to readyAt %s, restartedAt to readyAt %s", reply.ReadyAt.Sub(started), reply.ReadyAt.Sub(reply.RestartedAt))
	if reply.Digest != blobDigest(first) || reply.RestartedAt.IsZero() || reply.ReadyAt.Before(reply.RestartedAt) {
		t.Fatalf("reply: %s", body)
	}
	got := rt.child()
	short := strings.TrimPrefix(blobDigest(first), "sha256:")[:12]
	if got.Message != "first" || filepath.Dir(got.Cwd) != rt.overlays || !strings.HasPrefix(filepath.Base(got.Cwd), short) ||
		got.Pwd != got.Cwd || got.Secret != nil {
		t.Fatalf("overlay: %+v", got)
	}
	if !got.Executable {
		t.Fatalf("overlay lost its modes: %+v", got)
	}
	if page := rt.page(); page != "first" {
		t.Fatalf("frontend: %q", page)
	}
	if status, body := rt.reload(blobDigest(second)); status != http.StatusOK {
		t.Fatalf("second reload: %d %s", status, body)
	}
	if got := rt.child(); got.Message != "second" {
		t.Fatalf("second overlay: %+v", got)
	}
	if directories := rt.overlayDirectories(); len(directories) != 1 {
		t.Fatalf("previous overlay kept: %v", directories)
	}
	// The same digest again must not collide with the directory it is running from.
	if status, body := rt.reload(blobDigest(second)); status != http.StatusOK {
		t.Fatalf("repeated digest: %d %s", status, body)
	}
	if got := rt.child(); got.Message != "second" {
		t.Fatalf("repeated digest: %+v", got)
	}
}

func TestReloadKeepsWhatTheImageBuilt(t *testing.T) {
	// A frontend is often built into the image and absent from the source.
	blob := overlay(t, map[string]string{"main.py": childSource("new", "")})
	rt := startRuntime(t, testSecret, map[string][]byte{blobDigest(blob): blob})
	if status, body := rt.reload(blobDigest(blob)); status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	got, page := rt.child(), rt.page()
	if got.Message != "new" || page != "image" || got.Modules == nil || *got.Modules != "from image" || got.Bytecode {
		t.Fatalf("%+v %q", got, page)
	}
}

func TestReloadRejectsBeforeRestart(t *testing.T) {
	blob := overlay(t, sourceFiles("new", ""))
	digest := blobDigest(blob)
	blobs := map[string][]byte{digest: blob}
	// served adds a blob under its own digest, or under claimed when that is given.
	served := func(blob []byte, claimed ...string) string {
		digest := append(claimed, blobDigest(blob))[0]
		blobs[digest] = blob
		return digest
	}
	file := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644} }
	link := func(name, target string) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}
	}
	unpackable := func(entries ...*tar.Header) string { return served(overlay(t, nil, entries...)) }
	tampered := served(overlay(t, sourceFiles("tampered", "")), "sha256:"+strings.Repeat("a", 64))
	notArchive := served([]byte("not zstd"))
	parent, nestedParent, grandparent := unpackable(file("../outside")), unpackable(file("a/../../outside")), unpackable(file("../../outside"))
	absolute, absoluteLink, parentLink := unpackable(file("/outside")), unpackable(link("link", "/etc")), unpackable(link("a/link", "../../../outside"))
	throughLink := unpackable(link("link", "a"), link("a", ".."), file("link/outside"))
	device := unpackable(&tar.Header{Name: "device", Typeflag: tar.TypeChar, Mode: 0o644})
	rt := startRuntime(t, testSecret, blobs)
	request := func(fields map[string]string) string {
		merged := map[string]string{
			"registry": rt.registry, "repository": "holon/app", "token": "pull", "digest": digest, "mediaType": overlayMediaType,
		}
		maps.Copy(merged, fields)
		body, _ := json.Marshal(merged)
		return string(body)
	}
	for _, tc := range []struct {
		name, secret, body string
		status             int
	}{
		{"wrong secret", "wrong", request(nil), http.StatusUnauthorized},
		{"empty secret", "", request(nil), http.StatusUnauthorized},
		{"truncated body", testSecret, `{"digest":`, http.StatusBadRequest},
		{"null body", testSecret, `null`, http.StatusBadRequest},
		{"unknown field", testSecret, request(map[string]string{"other": "x"}), http.StatusBadRequest},
		{"bad digest", testSecret, request(map[string]string{"digest": "sha256:abc"}), http.StatusBadRequest},
		{"no token", testSecret, request(map[string]string{"token": ""}), http.StatusBadRequest},
		{"no repository", testSecret, request(map[string]string{"repository": ""}), http.StatusBadRequest},
		{"no media type", testSecret, request(map[string]string{"mediaType": ""}), http.StatusBadRequest},
		{"other media type", testSecret, request(map[string]string{"mediaType": "application/x-tar"}), http.StatusBadRequest},
		{"no registry", testSecret, request(map[string]string{"registry": "", "repository": "127.0.0.1/holon/app"}), http.StatusBadRequest},
		{"registry without scheme", testSecret, request(map[string]string{"registry": "registry"}), http.StatusBadRequest},
		{"registry of another scheme", testSecret, request(map[string]string{"registry": "ftp://registry"}), http.StatusBadRequest},
		{"registry refuses token", testSecret, request(map[string]string{"token": "other"}), http.StatusBadGateway},
		{"registry has no blob", testSecret, request(map[string]string{"digest": "sha256:" + strings.Repeat("0", 64)}), http.StatusBadGateway},
		{"blob does not match its digest", testSecret, request(map[string]string{"digest": tampered}), http.StatusBadGateway},
		{"not an archive", testSecret, request(map[string]string{"digest": notArchive}), http.StatusInternalServerError},
		{"parent path", testSecret, request(map[string]string{"digest": parent}), http.StatusInternalServerError},
		{"nested parent path", testSecret, request(map[string]string{"digest": nestedParent}), http.StatusInternalServerError},
		{"path above the overlay root", testSecret, request(map[string]string{"digest": grandparent}), http.StatusInternalServerError},
		{"absolute path", testSecret, request(map[string]string{"digest": absolute}), http.StatusInternalServerError},
		{"absolute link", testSecret, request(map[string]string{"digest": absoluteLink}), http.StatusInternalServerError},
		{"parent link", testSecret, request(map[string]string{"digest": parentLink}), http.StatusInternalServerError},
		{"write through links", testSecret, request(map[string]string{"digest": throughLink}), http.StatusInternalServerError},
		{"device", testSecret, request(map[string]string{"digest": device}), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := rt.post(tc.secret, tc.body); status != tc.status {
				t.Fatalf("%d %s", status, body)
			}
			if got := rt.child(); got.Message != "image" {
				t.Fatalf("application changed: %+v", got)
			}
			if directories := rt.overlayDirectories(); len(directories) != 0 {
				t.Fatalf("overlay left behind: %v", directories)
			}
			if entries, _ := os.ReadDir(filepath.Dir(rt.overlays)); len(entries) != 1 {
				t.Fatalf("wrote outside the overlay root: %v", entries)
			}
		})
	}
}

func TestReloadPathWithoutSecretIsLikeAnyOther(t *testing.T) {
	rt := startRuntime(t, "", nil)
	if status, _ := rt.post("anything", `{}`); status != http.StatusNotFound {
		t.Fatalf("reload path: %d", status)
	}
}

func TestReloadAnswers503AndSerialisesUntilReady(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	blob := overlay(t, sourceFiles("new", "while not os.path.exists("+strconv.Quote(release)+"):\n    time.sleep(0.01)"))
	digest := blobDigest(blob)
	rt := startRuntime(t, testSecret, map[string][]byte{digest: blob})
	finished := make(chan struct{})
	var status int
	var body []byte
	go func() {
		status, body = rt.reload(digest)
		close(finished)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := http.Get(rt.url + "/mcp")
		if err != nil {
			t.Fatal(err)
		}
		code := response.StatusCode
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if code == http.StatusServiceUnavailable {
			if response.Header.Get("Retry-After") != "1" {
				t.Fatalf("503: %v %s", response.Header, data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("application kept serving during restart")
		}
		time.Sleep(time.Millisecond)
	}
	if page := rt.page(); !strings.Contains(page, "restarting") {
		t.Fatalf("static route during restart: %q", page)
	}
	if status, body := rt.reload(digest); status != http.StatusConflict {
		t.Fatalf("concurrent reload: %d %s", status, body)
	}
	if status, _ := rt.post("wrong", `{}`); status != http.StatusUnauthorized {
		t.Fatalf("bad secret during restart: %d", status)
	}
	writeTree(t, filepath.Dir(release), map[string]string{"release": ""})
	<-finished
	if status != http.StatusOK {
		t.Fatalf("reload: %d %s", status, body)
	}
	if got := rt.child(); got.Message != "new" {
		t.Fatalf("after restart: %+v", got)
	}
}

func TestReloadRollsBack(t *testing.T) {
	previous := reloadTimeout
	reloadTimeout = 3 * time.Second
	t.Cleanup(func() { reloadTimeout = previous })
	good := overlay(t, sourceFiles("good", ""))
	bad := overlay(t, sourceFiles("bad", "sys.exit(3)"))
	for _, tc := range []struct {
		name          string
		from, overlay []byte            // from is reloaded first, and nil leaves the image running
		image         map[string]string // written over the image after boot
		status        int
		exit          string // the failed child's status in the reply
		child         string // the source running afterwards, or "" when the runtime exits
	}{
		{name: "child exits", overlay: bad, status: http.StatusInternalServerError, exit: "exit status 3", child: "image"},
		{name: "child exits on an overlay", from: good, overlay: bad, status: http.StatusInternalServerError, exit: "exit status 3", child: "good"},
		{
			name: "frontend outside the application", overlay: overlay(t, sourceFiles("outside", `FRONTEND = "/"`)),
			status: http.StatusInternalServerError, child: "image",
		},
		{name: "child never readies", overlay: overlay(t, sourceFiles("slow", "time.sleep(60)")), status: http.StatusGatewayTimeout, child: "image"},
		{
			name: "previous source fails too", overlay: bad, image: map[string]string{"main.py": "import sys\nsys.exit(4)\n"},
			status: http.StatusInternalServerError, exit: "exit status 3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blobs := map[string][]byte{blobDigest(tc.overlay): tc.overlay}
			if tc.from != nil {
				blobs[blobDigest(tc.from)] = tc.from
			}
			rt := startRuntime(t, testSecret, blobs)
			if tc.from != nil {
				if status, body := rt.reload(blobDigest(tc.from)); status != http.StatusOK {
					t.Fatalf("first reload: %d %s", status, body)
				}
			}
			before := rt.child()
			writeTree(t, rt.image, tc.image)
			status, body := rt.reload(blobDigest(tc.overlay))
			var failure reloadFailure
			if status != tc.status || json.Unmarshal(body, &failure) != nil || failure.Exit != tc.exit {
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
			if got := rt.child(); got.Message != tc.child || got.Cwd != before.Cwd {
				t.Fatalf("after rollback: %+v", got)
			}
			if page := rt.page(); page != tc.child {
				t.Fatalf("frontend after rollback: %q", page)
			}
			kept := []string{}
			if tc.from != nil {
				kept = append(kept, filepath.Base(before.Cwd))
			}
			if directories := rt.overlayDirectories(); !slices.Equal(directories, kept) {
				t.Fatalf("overlays after rollback: %v", directories)
			}
		})
	}
}

func TestRuntimeExitsWhenReloadedChildCrashes(t *testing.T) {
	blob := overlay(t, sourceFiles("new", ""))
	rt := startRuntime(t, testSecret, map[string][]byte{blobDigest(blob): blob})
	if status, body := rt.reload(blobDigest(blob)); status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	if response, err := http.Get(rt.url + "/mcp?exit"); err == nil {
		_ = response.Body.Close()
	}
	select {
	case err := <-rt.exited:
		rt.exited <- err
		if err == nil || !strings.Contains(err.Error(), "application exited") {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime outlived its application")
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
	good, next := overlay(t, sourceFiles("kept", "")), overlay(t, sourceFiles("next", ""))
	crashing := overlay(t, sourceFiles("crashing", "sys.exit(3)"))
	claimed := "sha256:" + strings.Repeat("a", 64)
	blobs := map[string][]byte{blobDigest(good): good, blobDigest(next): next, blobDigest(crashing): crashing, claimed: good}
	boot := func(digest, token string) reloadRequest {
		return reloadRequest{Repository: "holon/app", Token: token, Digest: digest, MediaType: overlayMediaType}
	}
	for _, tc := range []struct {
		name    string
		boot    reloadRequest
		child   string // the source the first request reaches
		because string // in the log when the image boots instead
	}{
		{"last good overlay", boot(blobDigest(good), "pull"), "kept", ""},
		{"token refused", boot(blobDigest(good), "other"), "image", "registry answered 401"},
		{"digest mismatch", boot(claimed, "pull"), "image", "digest mismatch"},
		{"malformed", reloadRequest{}, "image", "invalid HOLON_OVERLAY"},
		{"overlay child fails", boot(blobDigest(crashing), "pull"), "image", "application exited before ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := &bootLog{}
			log.SetOutput(logged)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			rt := startRuntime(t, testSecret, blobs, tc.boot)
			if got := rt.child(); got.Message != tc.child || got.Secret != nil {
				t.Fatalf("first request: %+v", got)
			}
			if tc.because == "" && logged.String() != "" || !strings.Contains(logged.String(), tc.because) {
				t.Fatalf("log: %q", logged.String())
			}
			// A later reload replaces whichever source booted, and removes a booted overlay.
			if status, body := rt.reload(blobDigest(next)); status != http.StatusOK {
				t.Fatalf("reload: %d %s", status, body)
			}
			if got, directories := rt.child(), rt.overlayDirectories(); got.Message != "next" || len(directories) != 1 {
				t.Fatalf("after reload: %+v %v", got, directories)
			}
		})
	}
}
