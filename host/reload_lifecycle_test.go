package host

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
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
