// Package compose runs the stack on a single host with Docker Compose.
package compose

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/stack"
)

const ProjectName = "infra-monitor"

// EnvFile is the generated Compose env file inside the project directory.
const EnvFile = ".env"

// managedDirs hold only files the CLI writes; anything else in them is
// removed on sync, so deleted custom rules and dashboards disappear.
var managedDirs = []string{"alerts", "provisioning/dashboards"}

// Sync writes the compose project into dir, replacing files the CLI owns.
// It reports whether a file that already existed changed, meaning running
// containers need a restart to pick it up.
func Sync(dir string, cfg config.Config, extras stack.Extras) (changed bool, err error) {
	if err := cfg.Validate(); err != nil {
		return false, err
	}
	d, err := stack.NewData(cfg)
	if err != nil {
		return false, err
	}
	files := map[string][]byte{}

	// Static files from the compose bundle; *.tmpl files are rendered below.
	err = fs.WalkDir(deploy.FS, "compose", func(p string, ent fs.DirEntry, err error) error {
		if err != nil || ent.IsDir() || strings.HasSuffix(p, ".tmpl") {
			return err
		}
		b, err := deploy.FS.ReadFile(p)
		files[strings.TrimPrefix(p, "compose/")] = b
		return err
	})
	if err != nil {
		return false, err
	}
	if files["provisioning/datasources/datasources.yml"], err = deploy.FS.ReadFile("shared/grafana/datasources/datasources.yml"); err != nil {
		return false, err
	}
	if files["provisioning/dashboards/dashboard.yml"], err = deploy.FS.ReadFile("shared/grafana/dashboards/dashboard.yml"); err != nil {
		return false, err
	}

	for name, render := range map[string]func() ([]byte, error){
		"config/prometheus.yml":   func() ([]byte, error) { return stack.PrometheusConfig(stack.Compose, d) },
		"config/alertmanager.yml": func() ([]byte, error) { return stack.AlertmanagerConfig(cfg) },
		"config/loki.yaml":        func() ([]byte, error) { return stack.LokiConfig(d) },
		"config/jaeger.yaml":      func() ([]byte, error) { return stack.JaegerConfig(d) },
	} {
		b, err := render()
		if err != nil {
			return false, fmt.Errorf("rendering %s: %w", name, err)
		}
		files[name] = b
	}

	rules, err := stack.Rules(stack.Compose, extras)
	if err != nil {
		return false, err
	}
	for k, v := range rules {
		files["alerts/"+k] = v
	}
	dashboards, err := stack.Dashboards(extras)
	if err != nil {
		return false, err
	}
	for k, v := range dashboards {
		files["provisioning/dashboards/"+k] = v
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	for rel, data := range files {
		c, err := writeFile(dir, rel, data, 0o644)
		if err != nil {
			return changed, err
		}
		changed = changed || c
	}
	env := fmt.Sprintf("GRAFANA_ADMIN_PASSWORD='%s'\nPROMETHEUS_RETENTION=%dh\n", cfg.Grafana.AdminPassword, d.Retention.Metrics)
	if _, err := writeFile(dir, EnvFile, []byte(env), 0o600); err != nil {
		return changed, err
	}

	for _, md := range managedDirs {
		entries, err := os.ReadDir(filepath.Join(dir, md))
		if err != nil {
			return changed, err
		}
		for _, ent := range entries {
			if _, keep := files[path.Join(md, ent.Name())]; !keep {
				if err := os.RemoveAll(filepath.Join(dir, md, ent.Name())); err != nil {
					return changed, err
				}
				changed = true
			}
		}
	}
	return changed, nil
}

// writeFile writes data if it differs from what is on disk and reports
// whether an existing file was changed.
func writeFile(dir, rel string, data []byte, mode os.FileMode) (bool, error) {
	p := filepath.Join(dir, filepath.FromSlash(rel))
	old, err := os.ReadFile(p)
	existed := err == nil
	switch {
	case existed && bytes.Equal(old, data):
		return false, os.Chmod(p, mode)
	case !existed && !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	// Containers run as non-root users and must be able to read config
	// files; the 0700 project directory keeps other host users out.
	if err := os.WriteFile(p, data, mode); err != nil {
		return false, err
	}
	return existed, os.Chmod(p, mode)
}

// Command builds a `docker compose` invocation for the project in dir.
func Command(dir string, args ...string) *exec.Cmd {
	full := append([]string{
		"compose",
		"--project-name", ProjectName,
		"--project-directory", dir,
		"--file", filepath.Join(dir, "docker-compose.yml"),
		"--env-file", filepath.Join(dir, EnvFile),
	}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}
