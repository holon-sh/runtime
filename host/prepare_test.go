package host

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLauncher(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source map[string]string
		main   string
		starts string
	}{
		{name: "python entry", source: map[string]string{"server.py": ""}, main: "main.py", starts: `start("server")`},
		{name: "typescript entry under src", source: map[string]string{"src/server.ts": ""}, main: "main.mjs", starts: "import('../src/server.ts')"},
		{name: "two entries", source: map[string]string{"server.py": "", "server.ts": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			writeTree(t, directory, tc.source)
			err := launcher(directory)
			main, _ := os.ReadFile(filepath.Join(directory, ".holon", tc.main))
			if (err == nil) != (tc.main != "") || !strings.Contains(string(main), tc.starts) {
				t.Fatalf("launcher: %v, %s", err, main)
			}
		})
	}
}

func TestInstalls(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous map[string]string
		source   map[string]string
		want     [][]string
	}{
		{name: "new package", source: map[string]string{"package.json": "{}"}, want: [][]string{{"npm", "install"}}},
		{
			name: "lockfile written by the previous install", previous: map[string]string{"package.json": "{}", "package-lock.json": "{}"},
			source: map[string]string{"package.json": "{}"},
		},
		{
			name: "pnpm lockfile added", previous: map[string]string{"package.json": "{}"},
			source: map[string]string{"package.json": "{}", "pnpm-lock.yaml": ""}, want: [][]string{{"pnpm", "install"}},
		},
		{
			name: "python lockfile changed beside an unchanged package", previous: map[string]string{"package.json": "{}", "pyproject.toml": "", "uv.lock": "a"},
			source: map[string]string{"package.json": "{}", "pyproject.toml": "", "uv.lock": "b"}, want: [][]string{{"uv", "sync"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous, source := t.TempDir(), t.TempDir()
			writeTree(t, previous, tc.previous)
			writeTree(t, source, tc.source)
			if got := installs(source, previous); !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Fatalf("installs %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFrontendServesItsDirectoryOnceBuilt(t *testing.T) {
	source := t.TempDir()
	handler, err := frontend("dist", source)
	if err != nil {
		t.Fatal(err)
	}
	get := func() (int, string) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		body, _ := io.ReadAll(response.Body)
		return response.Code, string(body)
	}
	if status, _ := get(); status != http.StatusNotFound {
		t.Fatalf("before the build: %d", status)
	}
	writeTree(t, source, map[string]string{"dist/index.html": "built"})
	if status, body := get(); status != http.StatusOK || body != "built" {
		t.Fatalf("after the build: %d %q", status, body)
	}
}
