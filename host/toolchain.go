package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/holon-sh/protocol/runtimecontract"
)

const toolchainDir = ".toolchain"

type installation struct {
	runtimecontract.Install
	node nodeProject
}

type nodeProject struct {
	manager, pin string
	versions     [3]string
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
		for _, argv := range plan.Install.Commands {
			if len(argv) != 0 && argv[0] == "uv" {
				if err := os.RemoveAll(filepath.Join(s.root, ".venv")); err != nil {
					return err
				}
			}
		}
		if node.manager != "" {
			if err := s.resolveNode(ctx, node, output); err != nil {
				return err
			}
		}
		if err := s.run(ctx, plan.Install.Commands, output, env...); err != nil {
			return err
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

var managerVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+(sha224|sha256|sha384|sha512)\.[A-Fa-f0-9]+)?$`)

func (s *supervisor) resolveNode(ctx context.Context, node nodeProject, output io.Writer) error {
	managerDir := filepath.Join(filepath.Dir(s.root), ".cache", "holon-manager")
	if err := errors.Join(os.RemoveAll(managerDir), os.RemoveAll(filepath.Join(s.root, "node_modules"))); err != nil {
		return err
	}
	install := []string{"fnm", "install", "--progress", "never"}
	if node.versions == [3]string{} {
		install = append(install, "--lts")
	}
	if err := s.run(ctx, [][]string{install}, output); err != nil {
		return err
	}
	if node.manager == "npm" && node.pin == "" {
		return nil
	}
	if err := os.MkdirAll(managerDir, 0o755); err != nil {
		return err
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
