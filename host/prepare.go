package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/holon-sh/runtime/host/javascript"
	"github.com/holon-sh/runtime/host/python"
)

var entries = []string{"server.py", "src/server.py", "server.ts", "src/server.ts", "server.js", "src/server.js"}

type ecosystem struct {
	manifest     string
	inputs       []string
	dependencies string
}

var ecosystems = []ecosystem{
	{manifest: "package.json", inputs: []string{"package.json", "package-lock.json", "pnpm-lock.yaml"}, dependencies: "node_modules"},
	{manifest: "pyproject.toml", inputs: []string{"pyproject.toml", "uv.lock", ".python-version"}, dependencies: ".venv"},
}

func prepare(ctx context.Context, directory, previous string) *reloadFailure {
	err := launcher(directory)
	if err == nil {
		err = install(ctx, directory, previous)
	}
	if err != nil {
		_ = os.RemoveAll(directory)
		return &reloadFailure{status: http.StatusInternalServerError, Message: err.Error()}
	}
	return nil
}

func launcher(directory string) error {
	found := slices.DeleteFunc(slices.Clone(entries), func(entry string) bool { return !present(directory, entry) })
	if len(found) != 1 {
		return fmt.Errorf("the source needs exactly one of %s", strings.Join(entries, ", "))
	}
	launch := filepath.Join(directory, ".holon")
	if err := os.Mkdir(launch, 0o755); err != nil {
		return err
	}
	entry := found[0]
	if filepath.Ext(entry) == ".py" {
		module := strings.TrimSuffix(strings.ReplaceAll(entry, "/", "."), ".py")
		return errors.Join(
			os.WriteFile(filepath.Join(launch, "holon_host.py"), []byte(python.Adapter), 0o644),
			os.WriteFile(filepath.Join(launch, "main.py"), fmt.Appendf(nil, "import asyncio\nfrom holon_host import start\nasyncio.run(start(%q))\n", module), 0o644),
		)
	}
	return errors.Join(
		os.WriteFile(filepath.Join(launch, "adapter.mjs"), []byte(javascript.Bundle), 0o644),
		os.WriteFile(filepath.Join(launch, "main.mjs"), []byte("import { start } from './adapter.mjs';\nawait start(() => import('../"+entry+"'));\n"), 0o644),
	)
}

func install(ctx context.Context, directory, previous string) error {
	for _, e := range ecosystems {
		if err := seed(filepath.Join(previous, e.dependencies), filepath.Join(directory, e.dependencies)); err != nil {
			return err
		}
	}
	for _, step := range installs(directory, previous) {
		if err := run(ctx, directory, step); err != nil {
			return err
		}
	}
	return nil
}

func installs(directory, previous string) [][]string {
	var steps [][]string
	for _, e := range ecosystems {
		switch {
		case !present(directory, e.manifest) || same(directory, previous, e.inputs):
		case e.manifest == "pyproject.toml":
			steps = append(steps, []string{"uv", "sync"})
		default:
			steps = append(steps, []string{runner(directory), "install"})
		}
	}
	return steps
}

func build(ctx context.Context, directory string) {
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	data, err := os.ReadFile(filepath.Join(directory, "package.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Scripts["build"] == "" {
		return
	}
	if err := run(ctx, directory, []string{runner(directory), "run", "build"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func run(ctx context.Context, directory string, step []string) error {
	command := exec.CommandContext(ctx, step[0], step[1:]...)
	command.Dir, command.Stdout, command.Stderr = directory, os.Stderr, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(step, " "), err)
	}
	return nil
}

func runner(directory string) string {
	if present(directory, "pnpm-lock.yaml") {
		return "pnpm"
	}
	return "npm"
}

func present(directory, name string) bool {
	info, err := os.Stat(filepath.Join(directory, name))
	return err == nil && info.Mode().IsRegular()
}

func same(directory, previous string, names []string) bool {
	return !slices.ContainsFunc(names, func(name string) bool {
		current, err := os.ReadFile(filepath.Join(directory, name))
		if errors.Is(err, fs.ErrNotExist) {
			return false
		}
		earlier, earlierErr := os.ReadFile(filepath.Join(previous, name))
		return err != nil || earlierErr != nil || !bytes.Equal(current, earlier)
	})
}

func seed(source, target string) error {
	if _, err := os.Lstat(source); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(source, path)
		destination := filepath.Join(target, relative)
		switch {
		case entry.IsDir():
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.Mkdir(destination, info.Mode().Perm()|0o700)
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, destination)
		}
		return os.Link(path, destination)
	})
}
