// Package compose runs the stack on a single host with Docker Compose.
package compose

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/stack"
)

const ProjectName = "infra-monitor"

// bundle maps embedded directories to their place in the compose project.
var bundle = []struct{ src, dst string }{
	{"compose", "."},
	{"shared/alerts", "alerts"},
	{"shared/grafana", "provisioning"},
}

// Sync writes the compose project into dir, replacing files the CLI owns.
// It reports whether an existing file changed, meaning running containers
// need a restart to pick it up.
func Sync(dir string, cfg config.Config) (changed bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	write := func(rel string, data []byte) error {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		old, err := os.ReadFile(p)
		switch {
		case err == nil && bytes.Equal(old, data):
			return nil
		case err == nil:
			changed = true
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		// Containers run as non-root users and must be able to read these;
		// the project directory itself is 0700, which keeps other host users out.
		return os.WriteFile(p, data, 0o644)
	}

	for _, b := range bundle {
		err := fs.WalkDir(deploy.FS, b.src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(b.src, p)
			data, err := deploy.FS.ReadFile(p)
			if err != nil {
				return err
			}
			return write(path.Join(b.dst, filepath.ToSlash(rel)), data)
		})
		if err != nil {
			return changed, err
		}
	}

	am, err := stack.AlertmanagerConfig(cfg)
	if err != nil {
		return changed, err
	}
	return changed, write("config/alertmanager.yml", am)
}

// Command builds a `docker compose` invocation for the project in dir.
func Command(dir, envFile string, args ...string) *exec.Cmd {
	full := append([]string{
		"compose",
		"--project-name", ProjectName,
		"--project-directory", dir,
		"--file", filepath.Join(dir, "docker-compose.yml"),
		"--env-file", envFile,
	}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}
