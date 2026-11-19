package host

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
)

// The release puts this catalog in the worker's immutable root. Each base is
// an OCI index in the same repository as the worker.
var cache struct {
	Bases    map[string]string
	Managers map[string]string
}

func init() {
	data, err := os.ReadFile("/etc/holon-runtime.json")
	if err == nil {
		if err := json.Unmarshal(data, &cache); err != nil {
			panic(err)
		}
	}
}

const nodeVersions = "/opt/holon/node/versions"

func (s *supervisor) executableBase(dir string) string {
	target, _ := filepath.EvalSymlinks(dir)
	return cmp.Or(cache.Bases[target], cache.Bases[filepath.Dir(filepath.Dir(target))])
}

func (s *supervisor) productionBase(dependencies []string, fallback string) string {
	core := cmp.Or(cache.Bases["core"], fallback)
	node := s.executableBase(filepath.Join(s.root, toolchainDir, "node"))
	if !slices.Contains(dependencies, ".venv") {
		return cmp.Or(node, core)
	}
	python := s.executableBase(filepath.Join(s.root, ".venv/bin/python"))
	if !slices.Contains(dependencies, "node_modules") {
		return cmp.Or(python, core)
	}
	if node != "" && python != "" {
		return fallback
	}
	return cmp.Or(node, python, core)
}

func link(target, name string) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	return os.Symlink(target, name)
}
