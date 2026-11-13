package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/holon-sh/protocol/runtimecontract"
)

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !condition() {
		select {
		case <-deadline:
			t.Fatal("condition did not become true")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestReloadRetiresACrashedAppsWorkers(t *testing.T) {
	r := run(t, nil)
	files := source("", "exec python3 main.py", "import subprocess, sys\nsubprocess.Popen([sys.executable, 'worker.py'])\n"+app("first"))
	files["worker.py"] = `from http.server import BaseHTTPRequestHandler, HTTPServer
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"old worker")
server=HTTPServer(("127.0.0.1",0),Handler)
open("worker_address","w").write("http://127.0.0.1:"+str(server.server_address[1]))
server.serve_forever()
`
	if status, body := r.reload(overlay(t, files)); status != 200 {
		t.Fatalf("reload: %d %s", status, body)
	}
	data, err := os.ReadFile(filepath.Join(r.root, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid application pid %q: %v", data, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	var address []byte
	waitFor(t, func() bool {
		address, _ = os.ReadFile(filepath.Join(r.root, "worker_address"))
		return len(address) > 0
	})
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { status, _ := r.get(); return status == 503 })
	if status, body := r.reload(overlay(t, source("", "exec python3 main.py", app("second")))); status != 200 {
		t.Fatalf("repair: %d %s", status, body)
	}
	response, err := http.Get(string(address))
	if err == nil {
		response.Body.Close()
		t.Fatal("retired generation's worker still answers after a successful reload")
	}
}

func TestReloadWaitsForBuildAndApp(t *testing.T) {
	for _, outcome := range []string{"build finishes last", "app starts last", "build fails", "cancel", "app exits"} {
		t.Run(outcome, func(t *testing.T) {
			r := run(t, nil)
			appStarted, buildStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/app":
					close(appStarted)
					if outcome != "app starts last" {
						return
					}
				case "/build":
					close(buildStarted)
					if outcome == "app starts last" {
						return
					}
				}
				select {
				case <-release:
				case <-req.Context().Done():
				}
			}))
			defer func() {
				gate.CloseClientConnections()
				gate.Close()
			}()
			start := "import urllib.request\nurllib.request.urlopen(" + strconv.Quote(gate.URL+"/app") + ").read()\n" + app("built")
			files := source("echo installed", "exec python3 main.py", start)
			files[runtimecontract.BuildFile] = "exec python3 build.py"
			code := 0
			if outcome == "build fails" {
				code = 7
			}
			files["build.py"] = fmt.Sprintf("import urllib.request,sys\nprint('building',flush=True)\nurllib.request.urlopen(%q).read()\nopen('build_finished','w').close()\nsys.exit(%d)\n", gate.URL+"/build", code)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			type result struct {
				status int
				body   string
				err    error
			}
			finished := make(chan result, 1)
			blob := overlay(t, files)
			go func() {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.url+runtimecontract.ReloadPath, bytes.NewReader(blob))
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					finished <- result{err: err}
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				finished <- result{response.StatusCode, string(body), err}
			}()
			for _, started := range []chan struct{}{buildStarted, appStarted} {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("app startup did not overlap the blocked build")
				}
			}
			if outcome == "app starts last" {
				waitFor(t, func() bool {
					_, err := os.Stat(filepath.Join(r.root, "build_finished"))
					return err == nil
				})
			}
			if status, _ := r.get(); status != 503 {
				t.Fatalf("traffic reached an unfinished reload: %d", status)
			}
			select {
			case answer := <-finished:
				t.Fatalf("reload completed before both commands were ready: %+v", answer)
			default:
			}
			switch outcome {
			case "cancel":
				cancel()
			case "app exits":
				var pid int
				waitFor(t, func() bool {
					data, _ := os.ReadFile(filepath.Join(r.root, "pid"))
					pid, _ = strconv.Atoi(string(data))
					return pid > 0
				})
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			default:
				close(release)
			}
			answer := <-finished
			success := outcome == "build finishes last" || outcome == "app starts last"
			if outcome == "cancel" {
				if !errors.Is(answer.err, context.Canceled) {
					t.Fatalf("cancelled upload: %v", answer.err)
				}
			} else {
				if answer.err != nil {
					t.Fatal(answer.err)
				}
				var reply runtimecontract.ReloadReply
				if err := json.Unmarshal([]byte(answer.body), &reply); err != nil {
					t.Fatal(err)
				}
				want := 500
				if success {
					want = 200
				}
				if answer.status != want || !strings.Contains(reply.Output, "building") {
					t.Fatalf("reload: %+v", answer)
				}
			}
			if success {
				if status, body := r.get(); status != 200 || !strings.Contains(body, "built") {
					t.Fatalf("ready app: %d %s", status, body)
				}
			} else {
				if status, body := r.reload(overlay(t, source("", "exec python3 main.py", app("repaired")))); status != 200 {
					t.Fatalf("repair: %d %s", status, body)
				}
			}
		})
	}
}
