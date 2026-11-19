package host

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/holon-sh/protocol/runtimecontract"
)

const toolchainDir = ".toolchain"

//go:embed pnpm/config.yaml
var pnpmDefaults embed.FS

type installation struct {
	runtimecontract.Install
	node nodeProject
}

type nodeProject struct {
	manager, pin string
	versions     [3]string
	dev          nodeRange
}

func (s *supervisor) environment() []string {
	home := filepath.Dir(s.root)
	cache := filepath.Join(home, ".cache")
	return []string{
		"PWD=" + s.root, "HOME=" + home, "XDG_CACHE_HOME=" + cache,
		"FNM_DIR=" + filepath.Join(s.root, toolchainDir, "node"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(s.root, toolchainDir, "python"),
		"UV_COMPILE_BYTECODE=1",
		"npm_config_cache=" + filepath.Join(cache, "npm"),
		"npm_config_audit=false", "npm_config_fund=false", "npm_config_update_notifier=false",
	}
}

func (s *supervisor) prepare(ctx context.Context, plan runtimecontract.Plan, output io.Writer, env ...string) error {
	node, err := s.nodeProject(plan.Install.Commands)
	if err != nil {
		return err
	}
	next := &installation{plan.Install, node}
	if !reflect.DeepEqual(s.installed, next) {
		s.installed = nil
		// uv and fnm select from a clean tree. Their download caches live beside the source.
		if err := os.RemoveAll(filepath.Join(s.root, toolchainDir)); err != nil {
			return err
		}
		if node.manager != "" {
			if err := s.resolveNode(ctx, node, output); err != nil {
				return err
			}
		}
		if err := s.run(ctx, plan.Install.Commands, output, env...); err != nil {
			return err
		}
		// uv sync compiles packages but skips the downloaded standard library.
		// Cache only the installed interpreter for dev restarts. Source stays live.
		if len(env) == 0 && len(plan.Start) != 0 && plan.Start[0] == ".venv/bin/python" && s.executableBase(filepath.Join(s.root, plan.Start[0])) == "" {
			cache := []string{plan.Start[0], "-IS", "-c", `import compileall, sysconfig
compileall.compile_dir(sysconfig.get_path("stdlib"), quiet=2)`}
			if err := s.run(ctx, [][]string{cache}, output); err != nil {
				return err
			}
		}
		s.installed = next
	}
	return s.run(ctx, plan.Prepare, output, env...)
}

// The existing CLI already names the manager in its install command. Its older
// install digests omit the Node version files, so include their selection here.
func (s *supervisor) nodeProject(commands [][]string) (nodeProject, error) {
	var project nodeProject
	for _, argv := range commands {
		if len(argv) != 0 && (argv[0] == "npm" || argv[0] == "pnpm") {
			project.manager = argv[0]
			break
		}
	}
	if project.manager == "" {
		return project, nil
	}
	var manifest struct {
		PackageManager string
		Engines        struct{ Node string }
		DevEngines     struct{ Runtime nodeRange }
	}
	data, err := os.ReadFile(filepath.Join(s.root, "package.json"))
	if err == nil {
		err = json.Unmarshal(data, &manifest)
	}
	if err != nil {
		return project, fmt.Errorf("package.json: %w", err)
	}
	if manifest.PackageManager != "" {
		name, version, pinned := strings.Cut(manifest.PackageManager, "@")
		if name != project.manager || (pinned && !managerVersion.MatchString(version)) {
			return project, fmt.Errorf("invalid %s packageManager declaration %q", project.manager, manifest.PackageManager)
		}
		project.pin = version
	}
	project.dev = manifest.DevEngines.Runtime
	project.versions[2] = manifest.Engines.Node
	for i, name := range []string{".nvmrc", ".node-version"} {
		data, err := os.ReadFile(filepath.Join(s.root, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return project, err
		}
		project.versions[i] = strings.TrimSpace(string(data))
	}
	return project, nil
}

// npm accepts one runtime declaration or several alternatives and evaluates
// onFail itself. Only ranges for Node affect its installation.
type nodeRange string

func (result *nodeRange) UnmarshalJSON(data []byte) error {
	var alternatives []struct{ Name, Version string }
	if data[0] != '[' {
		data = slices.Concat([]byte{'['}, data, []byte{']'})
	}
	if err := json.Unmarshal(data, &alternatives); err != nil {
		return err
	}
	var ranges []string
	for _, alternative := range alternatives {
		if alternative.Name == "node" {
			if alternative.Version == "" {
				*result = ""
				return nil
			}
			ranges = append(ranges, alternative.Version)
		}
	}
	*result = nodeRange(strings.Join(ranges, " || "))
	return nil
}

var managerVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+(sha224|sha256|sha384|sha512)\.[A-Fa-f0-9]+)?$`)

func (s *supervisor) resolveNode(ctx context.Context, node nodeProject, output io.Writer) error {
	if node.manager == "pnpm" {
		if err := os.CopyFS(filepath.Join(filepath.Dir(s.root), ".config"), pnpmDefaults); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	managerDir := filepath.Join(filepath.Dir(s.root), ".cache", "holon-manager")
	if err := errors.Join(os.RemoveAll(managerDir), os.RemoveAll(filepath.Join(s.root, "node_modules"))); err != nil {
		return err
	}
	if err := os.MkdirAll(managerDir, 0o755); err != nil {
		return err
	}
	if err := s.selectNode(ctx, node, managerDir, output); err != nil {
		return err
	}
	if bin := cache.Managers[node.manager+"@"+node.pin]; node.pin != "" && bin != "" {
		return os.Symlink(bin, filepath.Join(managerDir, "bin"))
	}
	if node.manager == "npm" && node.pin == "" {
		return nil
	}
	npm := func(args ...string) error {
		cmd := s.command(ctx, append([]string{"npm"}, args...))
		cmd.Dir, cmd.Stdout, cmd.Stderr = managerDir, output, output
		return cmd.Run()
	}
	spec := node.manager
	version, integrity, _ := strings.Cut(node.pin, "+")
	if version != "" {
		spec += "@" + version
	}
	if integrity != "" {
		if err := npm("pack", "--ignore-scripts", spec); err != nil {
			return err
		}
		spec = filepath.Join(managerDir, node.manager+"-"+version+".tgz")
		algorithm, sum, _ := strings.Cut(integrity, ".")
		check := s.command(ctx, []string{algorithm + "sum", "--check", "--status"})
		check.Stdin = strings.NewReader(sum + "  " + spec + "\n")
		if err := check.Run(); err != nil {
			return fmt.Errorf("%s does not match packageManager checksum %s: %w", filepath.Base(spec), integrity, err)
		}
	}
	return npm("install", "--global", "--prefix", managerDir, "--ignore-scripts", spec)
}

// Only an exact project declaration can use an installed interpreter without
// consulting fnm. Ranges and the LTS default must see the current upstream index.
func (s *supervisor) selectNode(ctx context.Context, node nodeProject, scratch string, output io.Writer) error {
	selected := filepath.Join(s.root, toolchainDir, "node")
	for _, version := range node.versions {
		if version != "" {
			cached := nodeVersions + "/v" + strings.TrimPrefix(version, "v") + "/installation"
			if cache.Bases[cached] != "" {
				return link(cached, selected)
			}
			break
		}
	}
	tree, err := os.MkdirTemp(filepath.Dir(s.root), ".fnm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tree)
	for installed := range cache.Bases {
		if strings.HasPrefix(installed, nodeVersions+"/") {
			version := filepath.Dir(installed)
			if err := link(version, filepath.Join(tree, "node-versions", filepath.Base(version))); err != nil {
				return err
			}
		}
	}
	if err := s.installNode(ctx, node, scratch, output, "FNM_DIR="+tree); err != nil {
		return err
	}
	installed, err := filepath.EvalSymlinks(filepath.Join(tree, "aliases/default"))
	if err != nil {
		return err
	}
	if cache.Bases[installed] != "" {
		return link(installed, selected)
	}
	if err := os.MkdirAll(filepath.Dir(selected), 0o755); err != nil {
		return err
	}
	return os.Rename(installed, selected)
}

func (s *supervisor) installNode(ctx context.Context, node nodeProject, scratch string, output io.Writer, env string) error {
	install := []string{"fnm", "install", "--progress", "never"}
	if node.versions != [3]string{} {
		return s.run(ctx, [][]string{install}, output, env)
	}
	// fnm parses npm ranges from engines.node, including ranges its positional
	// argument cannot express. Keep the project's manifest untouched.
	if node.dev != "" {
		manifest, _ := json.Marshal(map[string]any{"engines": map[string]nodeRange{"node": node.dev}})
		if err := os.WriteFile(filepath.Join(scratch, "package.json"), manifest, 0o600); err != nil {
			return err
		}
		cmd := s.command(ctx, install, env)
		var message tail
		cmd.Dir, cmd.Stdout, cmd.Stderr = scratch, output, io.MultiWriter(output, &message)
		if err := cmd.Run(); err == nil || !unmatchedNodeRange.Match(message) {
			return err
		}
	}
	return s.run(ctx, [][]string{append(install, "--lts")}, output, env)
}

// fnm 1.39 reports an unparseable range, an unsatisfied range and a transport
// failure with the same exit code. Only the first two leave npm to apply onFail.
var unmatchedNodeRange = regexp.MustCompile(`(?m)^error: Can't find (version in dotfiles\.|a Node version that matches )`)
