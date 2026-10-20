// Package cli implements the infra-monitor command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/doctor"
	"github.com/OcheOps/Infra-Monitor/internal/prompt"
	"github.com/OcheOps/Infra-Monitor/internal/stack"
)

const usage = `infra-monitor: self-hosted observability stack (metrics, logs, traces, alerts)

Run with no arguments for an interactive menu.

Setup:
  init                         guided setup (or set values with flags)
  probes  list|add|remove      URLs to check (https://..., tcp://host:port)
  targets list|add|remove      extra Prometheus scrape targets (host:port)
  notify  list|setup|add|remove|test
                               alert channels: telegram, slack, email, webhook
  config  show|path            print the config (secrets hidden) or its location

Run on this host (Docker Compose):
  compose up|down|restart|status|logs [SERVICE]
  compose sync                 write the Compose project without starting it

Run on Kubernetes:
  k8s configure                namespace, storage, ingress (guided or flags)
  k8s apply|render|status      deploy, print manifests, show pods
  k8s port-forward SERVICE     grafana, prometheus, alertmanager, jaeger, loki
  k8s delete [--purge]         remove (--purge also deletes all data)

Other:
  doctor [compose|k8s]         check this machine's prerequisites and how to fix them
  completion bash|zsh|fish     print a shell completion script
  version

Global flags:
  --dir DIR   config directory (default $INFRA_MONITOR_DIR or ~/.infra-monitor)

Your own alert rules (*.yml) and Grafana dashboards (*.json) go in
DIR/rules and DIR/dashboards; they are deployed with the stack.

Run 'infra-monitor <command> --help' for command flags.
`

type app struct {
	dir         string
	version     string
	stdout      io.Writer
	stderr      io.Writer
	p           *prompt.Prompter
	interactive bool
	// doctor runs prerequisite checks; nil skips them (tests).
	doctor *doctor.Doctor
}

// Run executes the CLI and returns the process exit code.
func Run(args []string, version string) int {
	a := &app{
		version:     version,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		p:           prompt.New(os.Stdin, os.Stdout),
		interactive: prompt.IsTerminal(os.Stdin) && prompt.IsTerminal(os.Stdout),
		doctor:      doctor.New(),
	}
	if err := a.run(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if errors.Is(err, prompt.ErrAborted) {
			fmt.Fprintln(os.Stderr, "Cancelled.")
			return 130
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func (a *app) run(args []string) error {
	global := flag.NewFlagSet("infra-monitor", flag.ContinueOnError)
	global.SetOutput(a.stderr)
	global.Usage = func() { fmt.Fprint(a.stderr, usage) }
	global.StringVar(&a.dir, "dir", defaultDir(), "config directory")
	if err := global.Parse(args); err != nil {
		return err
	}
	rest := global.Args()
	if len(rest) == 0 {
		if a.interactive {
			return a.menu()
		}
		global.Usage()
		return errors.New("no command given")
	}

	cmd, rest := rest[0], rest[1:]
	switch cmd {
	case "init", "setup":
		return a.cmdInit(rest)
	case "probes", "probe", "urls":
		return a.cmdProbes(rest)
	case "targets", "target":
		return a.cmdTargets(rest)
	case "notify", "notifications":
		return a.cmdNotify(rest)
	case "config":
		return a.cmdConfig(rest)
	case "compose":
		return a.cmdCompose(rest)
	case "k8s", "kubernetes":
		return a.cmdK8s(rest)
	case "doctor":
		return a.cmdDoctor(rest)
	case "completion":
		return a.cmdCompletion(rest)
	case "version", "--version":
		fmt.Fprintln(a.stdout, a.version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(a.stdout, usage)
		return nil
	default:
		global.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func defaultDir() string {
	if d := os.Getenv("INFRA_MONITOR_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".infra-monitor")
	}
	return ".infra-monitor"
}

// newFlags returns a FlagSet that writes errors and help to stderr.
func (a *app) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	return fs
}

// parse parses flags that may appear before, after or between positional
// arguments, and returns the positional ones.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// load reads the config, or returns an error telling the user to run init.
func (a *app) load() (config.Config, error) {
	return config.Load(a.dir)
}

// loadOrDefault reads the config, falling back to defaults if none exists.
func (a *app) loadOrDefault() (config.Config, bool, error) {
	cfg, err := config.Load(a.dir)
	if errors.Is(err, config.ErrNotFound) {
		return cfg, false, nil
	}
	return cfg, err == nil, err
}

// save validates and writes the config, making sure a password exists.
func (a *app) save(cfg config.Config) error {
	if cfg.Grafana.AdminPassword == "" {
		pw, err := config.GeneratePassword()
		if err != nil {
			return err
		}
		cfg.Grafana.AdminPassword = pw
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(a.dir); err != nil {
		return err
	}
	for _, d := range []string{stack.RulesDir, stack.DashboardsDir} {
		if err := os.MkdirAll(filepath.Join(a.dir, d), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// loadAll reads and validates the config and the user's extra files.
func (a *app) loadAll() (config.Config, stack.Extras, error) {
	cfg, err := a.load()
	if err != nil {
		return cfg, stack.Extras{}, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, stack.Extras{}, fmt.Errorf("%s: %w", config.Path(a.dir), err)
	}
	extras, err := stack.LoadExtras(a.dir)
	return cfg, extras, err
}

const applyHint = "Apply with `infra-monitor compose up` or `infra-monitor k8s apply`."

// changed tells the user how to apply a config change, or offers to apply
// it right away in interactive mode.
func (a *app) changed() error {
	if !a.interactive {
		fmt.Fprintln(a.stdout, applyHint)
		return nil
	}
	return a.offerApply("Apply the change now?")
}
