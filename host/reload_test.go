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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
            "secret": os.environ.get("HOLON_RELOAD_SECRET"),
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

func overlay(t *testing.T, files map[string]string) []byte {
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

// startRuntime runs the supervisor on a child that serves the "image" source from a fresh working directory.
func startRuntime(t *testing.T, secret string, blobs map[string][]byte) *testRuntime {
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
	Message string  `json:"message"`
	Cwd     string  `json:"cwd"`
	Pwd     string  `json:"pwd"`
	Secret  *string `json:"secret"`
	Modules *string `json:"modules"`
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
	first, second := overlay(t, sourceFiles("first", "")), overlay(t, sourceFiles("second", ""))
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
	if got.Modules == nil || *got.Modules != "from image" {
		t.Fatalf("image entries are not visible from the overlay: %+v", got)
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
	if got, page := rt.child(), rt.page(); got.Message != "new" || page != "image" {
		t.Fatalf("%+v %q", got, page)
	}
}

func TestLinkImageEntries(t *testing.T) {
	image, directory := t.TempDir(), t.TempDir()
	writeTree(t, image, map[string]string{"node_modules/a": "", "dist/b": "", "__pycache__/c": "", "source.py": "image"})
	writeTree(t, directory, map[string]string{"source.py": "overlay"})
	if err := linkImageEntries(image, directory); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"dist", "node_modules", "source.py"}) {
		t.Fatalf("entries: %v", names)
	}
	if data, err := os.ReadFile(filepath.Join(directory, "source.py")); err != nil || string(data) != "overlay" {
		t.Fatalf("overlay file: %q %v", data, err)
	}
}

func TestReloadRejectsBeforeRestart(t *testing.T) {
	blob := overlay(t, sourceFiles("new", ""))
	digest := blobDigest(blob)
	rt := startRuntime(t, testSecret, map[string][]byte{digest: blob})
	request := func(fields map[string]string) string {
		merged := map[string]string{"registry": rt.registry, "repository": "holon/app", "token": "pull", "digest": digest}
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
		{"other media type", testSecret, request(map[string]string{"mediaType": "application/x-tar"}), http.StatusBadRequest},
		{"registry refuses token", testSecret, request(map[string]string{"token": "other"}), http.StatusBadGateway},
		{"registry has no blob", testSecret, request(map[string]string{"digest": "sha256:" + strings.Repeat("0", 64)}), http.StatusBadGateway},
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
		})
	}
}

func TestReloadPathWithoutSecretIsLikeAnyOther(t *testing.T) {
	rt := startRuntime(t, "", nil)
	if status, _ := rt.post("anything", `{}`); status != http.StatusNotFound {
		t.Fatalf("reload path: %d", status)
	}
}

func TestReloadDigestMismatch(t *testing.T) {
	for name, served := range map[string][]byte{
		"valid archive under another digest": overlay(t, sourceFiles("tampered", "")),
		"not an archive":                     []byte("not zstd"),
	} {
		t.Run(name, func(t *testing.T) {
			claimed := "sha256:" + strings.Repeat("a", 64)
			rt := startRuntime(t, testSecret, map[string][]byte{claimed: served})
			status, body := rt.reload(claimed)
			var failure struct {
				Digest string `json:"digest"`
			}
			if status != http.StatusConflict || json.Unmarshal(body, &failure) != nil || failure.Digest != blobDigest(served) {
				t.Fatalf("%d %s", status, body)
			}
			if got := rt.child(); got.Message != "image" {
				t.Fatalf("application changed: %+v", got)
			}
			if directories := rt.overlayDirectories(); len(directories) != 0 {
				t.Fatalf("overlay left behind: %v", directories)
			}
		})
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
			if response.Header.Get("Content-Type") != "application/json" || !json.Valid(data) {
				t.Fatalf("503 body: %v %s", response.Header, data)
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
	if status, body := rt.reload(digest); status != http.StatusConflict || strings.Contains(string(body), `"digest"`) {
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

func TestReloadRollsBackFailedChild(t *testing.T) {
	good := overlay(t, sourceFiles("good", ""))
	bad := overlay(t, sourceFiles("bad", "sys.exit(3)"))
	badFrontend := overlay(t, sourceFiles("outside frontend", `FRONTEND = "/"`))
	rt := startRuntime(t, testSecret, map[string][]byte{
		blobDigest(good): good, blobDigest(bad): bad, blobDigest(badFrontend): badFrontend,
	})
	// Rolling back from the image's own directory.
	status, body := rt.reload(blobDigest(bad))
	var failure reloadFailure
	if status != http.StatusInternalServerError || json.Unmarshal(body, &failure) != nil || failure.Exit != "exit status 3" {
		t.Fatalf("failed child: %d %s", status, body)
	}
	if got := rt.child(); got.Message != "image" || got.Cwd != rt.image {
		t.Fatalf("after rollback: %+v", got)
	}
	if directories := rt.overlayDirectories(); len(directories) != 0 {
		t.Fatalf("failed overlay kept: %v", directories)
	}
	// Rolling back from an overlay, whose directory must survive.
	if status, body := rt.reload(blobDigest(good)); status != http.StatusOK {
		t.Fatalf("good reload: %d %s", status, body)
	}
	running := rt.child().Cwd
	if status, body := rt.reload(blobDigest(bad)); status != http.StatusInternalServerError {
		t.Fatalf("failed child: %d %s", status, body)
	}
	if got := rt.child(); got.Message != "good" || got.Cwd != running {
		t.Fatalf("after rollback: %+v", got)
	}
	if rt.page() != "good" {
		t.Fatal("frontend did not roll back")
	}
	// A child that posts ready but names a frontend outside the application also rolls back.
	if status, body := rt.reload(blobDigest(badFrontend)); status != http.StatusInternalServerError {
		t.Fatalf("invalid frontend: %d %s", status, body)
	}
	if got := rt.child(); got.Message != "good" {
		t.Fatalf("after rollback: %+v", got)
	}
}

func TestReloadRollsBackChildThatNeverReadies(t *testing.T) {
	previous := readyTimeout
	readyTimeout = 500 * time.Millisecond
	t.Cleanup(func() { readyTimeout = previous })
	blob := overlay(t, sourceFiles("slow", "time.sleep(60)"))
	rt := startRuntime(t, testSecret, map[string][]byte{blobDigest(blob): blob})
	if status, body := rt.reload(blobDigest(blob)); status != http.StatusGatewayTimeout {
		t.Fatalf("%d %s", status, body)
	}
	if got := rt.child(); got.Message != "image" {
		t.Fatalf("after rollback: %+v", got)
	}
}

func TestReloadExitsWhenRollbackFails(t *testing.T) {
	bad := overlay(t, sourceFiles("bad", "sys.exit(3)"))
	rt := startRuntime(t, testSecret, map[string][]byte{blobDigest(bad): bad})
	writeTree(t, rt.image, map[string]string{"main.py": "import sys\nsys.exit(4)\n"})
	if status, body := rt.reload(blobDigest(bad)); status != http.StatusInternalServerError {
		t.Fatalf("%d %s", status, body)
	}
	select {
	case err := <-rt.exited:
		rt.exited <- err
		if err == nil || !strings.Contains(err.Error(), "restore application") {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime kept running with no application")
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

func TestBlobURL(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ registry, repository, want string }{
		{"http://registry.holon-system.svc.cluster.local:5000", "holon/upload/app", "http://registry.holon-system.svc.cluster.local:5000/v2/holon/upload/app/blobs/" + digest},
		{"", "registry.example:5000/holon/app", "https://registry.example:5000/v2/holon/app/blobs/" + digest},
		{"ftp://registry", "holon/app", ""},
		{"registry", "holon/app", ""},
		{"", "registry-only", ""},
		{"http://registry", "", ""},
	} {
		got, err := blobURL(reloadRequest{Registry: tc.registry, Repository: tc.repository, Digest: digest})
		if tc.want == "" && err == nil || tc.want != "" && (err != nil || got.String() != tc.want) {
			t.Errorf("%q %q: %v %v", tc.registry, tc.repository, got, err)
		}
	}
}

func TestExtractOverlayStaysInside(t *testing.T) {
	type entry struct {
		name, link string
		kind       byte
	}
	for name, entries := range map[string][]entry{
		"parent path":        {{name: "../outside", kind: tar.TypeReg}},
		"nested parent path": {{name: "a/../../outside", kind: tar.TypeReg}},
		"absolute path":      {{name: "/outside", kind: tar.TypeReg}},
		"absolute link":      {{name: "link", link: "/etc", kind: tar.TypeSymlink}},
		"parent link":        {{name: "a/link", link: "../../outside", kind: tar.TypeSymlink}},
		"write through link": {{name: "link", link: "a", kind: tar.TypeSymlink}, {name: "a", link: "..", kind: tar.TypeSymlink}, {name: "link/outside", kind: tar.TypeReg}},
		"device":             {{name: "device", kind: tar.TypeChar}},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			directory := filepath.Join(parent, "source")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			var compressed bytes.Buffer
			encoder, _ := zstd.NewWriter(&compressed)
			archive := tar.NewWriter(encoder)
			for _, item := range entries {
				if err := archive.WriteHeader(&tar.Header{Name: item.name, Linkname: item.link, Typeflag: item.kind, Mode: 0o644}); err != nil {
					t.Fatal(err)
				}
			}
			_ = archive.Close()
			_ = encoder.Close()
			if extractOverlay(&compressed, directory) == nil {
				t.Fatal("archive accepted")
			}
			if entries, _ := os.ReadDir(parent); len(entries) != 1 {
				t.Fatalf("wrote outside the directory: %v", entries)
			}
		})
	}
}

func TestExtractOverlayKeepsTree(t *testing.T) {
	var compressed bytes.Buffer
	encoder, _ := zstd.NewWriter(&compressed)
	archive := tar.NewWriter(encoder)
	for _, header := range []*tar.Header{
		{Name: "readonly/", Typeflag: tar.TypeDir, Mode: 0o500},
		{Name: "readonly/run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 2},
		{Name: "readonly/link", Typeflag: tar.TypeSymlink, Linkname: "run.sh", Mode: 0o777},
	} {
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			_, _ = archive.Write([]byte("ok"))
		}
	}
	_ = archive.Close()
	_ = encoder.Close()
	directory := t.TempDir()
	if err := extractOverlay(&compressed, directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, "readonly/link"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("file through link: %v %v", info, err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatalf("overlay cannot be removed: %v", err)
	}
}
