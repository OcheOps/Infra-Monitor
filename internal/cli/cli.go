// Package cli implements the infra-monitor command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/OcheOps/Infra-Monitor/internal/compose"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/k8s"
)

const usage = `infra-monitor: self-hosted observability stack (metrics, logs, traces, alerts)

Usage:
  infra-monitor [--dir DIR] <command> [flags]

Commands:
  init                     create or update the config (Telegram, Grafana password)
  compose up               run the stack on this host with Docker Compose
  compose down             stop the stack (data volumes are kept)
  compose restart          re-sync config files and restart all services
  compose status           show service status
  compose logs [SERVICE]   follow logs
  k8s render               print the Kubernetes manifests
  k8s apply                deploy to the current kubectl context
  k8s status               show pods, services and volumes
  k8s port-forward SVC     forward a service to localhost (grafana, prometheus, ...)
  k8s delete [--purge]     remove the stack (--purge also deletes the namespace and its data)
  version                  print the version

Global flags:
  --dir DIR   working directory for config and the compose project
              (default $INFRA_MONITOR_DIR or ~/.infra-monitor)

Run 'infra-monitor <command> --help' for command flags.
`

type app struct {
	dir     string
	version string
	stdout  io.Writer
}

func (a *app) envFile() string    { return filepath.Join(a.dir, ".env") }
func (a *app) composeDir() string { return filepath.Join(a.dir, "compose") }

// Run executes the CLI and returns the process exit code.
func Run(args []string, version string) int {
	a := &app{version: version, stdout: os.Stdout}
	if err := a.run(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func (a *app) run(args []string) error {
	global := flag.NewFlagSet("infra-monitor", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	global.StringVar(&a.dir, "dir", defaultDir(), "working directory")
	if err := global.Parse(args); err != nil {
		return err
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return errors.New("no command given")
	}

	switch cmd, rest := rest[0], rest[1:]; cmd {
	case "init":
		return a.cmdInit(rest)
	case "compose":
		return a.cmdCompose(rest)
	case "k8s", "kubernetes":
		return a.cmdK8s(rest)
	case "version", "--version":
		fmt.Fprintln(a.stdout, a.version)
		return nil
	case "help", "-h", "--help":
		global.Usage()
		return nil
	default:
		global.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
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

func defaultDir() string {
	if d := os.Getenv("INFRA_MONITOR_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".infra-monitor")
	}
	return ".infra-monitor"
}

func (a *app) cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	token := fs.String("telegram-token", "", "Telegram bot token from @BotFather")
	chatID := fs.String("telegram-chat-id", "", "Telegram chat id to send alerts to")
	password := fs.String("grafana-password", "", "Grafana admin password (generated if unset)")
	noTelegram := fs.Bool("no-telegram", false, "clear Telegram settings and run without alert notifications")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(a.envFile())
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		return err
	}
	if *noTelegram {
		cfg.TelegramToken, cfg.TelegramChatID = "", ""
	}
	if *token != "" {
		cfg.TelegramToken = *token
	}
	if *chatID != "" {
		cfg.TelegramChatID = *chatID
	}
	if *password != "" {
		cfg.GrafanaPassword = *password
	}
	generated := false
	if cfg.GrafanaPassword == "" {
		if cfg.GrafanaPassword, err = config.GeneratePassword(); err != nil {
			return err
		}
		generated = true
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(a.envFile()); err != nil {
		return err
	}

	fmt.Fprintf(a.stdout, "Config written to %s\n", a.envFile())
	if cfg.TelegramEnabled() {
		fmt.Fprintln(a.stdout, "Alerts: Telegram")
	} else {
		fmt.Fprintln(a.stdout, "Alerts: not sent anywhere (set --telegram-token and --telegram-chat-id to enable)")
	}
	if generated {
		fmt.Fprintf(a.stdout, "Grafana login: admin / %s\n", cfg.GrafanaPassword)
	}
	fmt.Fprintln(a.stdout, "Next: `infra-monitor compose up` or `infra-monitor k8s apply`")
	return nil
}

func (a *app) loadConfig() (config.Config, error) {
	cfg, err := config.Load(a.envFile())
	if err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", a.envFile(), err)
	}
	return cfg, nil
}

func (a *app) cmdCompose(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infra-monitor compose up|down|restart|status|logs")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("compose "+sub, flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	dir := a.composeDir()
	run := func(args ...string) error { return compose.Command(dir, a.envFile(), args...).Run() }

	switch sub {
	case "up", "restart":
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		changed, err := compose.Sync(dir, cfg)
		if err != nil {
			return err
		}
		if sub == "restart" {
			return run("restart")
		}
		if err := run("up", "-d", "--remove-orphans"); err != nil {
			return err
		}
		if changed {
			// Bind-mounted config files changed; up -d alone doesn't reload them.
			if err := run("restart"); err != nil {
				return err
			}
		}
		fmt.Fprint(a.stdout, composeEndpoints)
		return nil
	case "down":
		return run("down")
	case "status", "ps":
		return run("ps")
	case "logs":
		return run(append([]string{"logs", "--follow", "--tail", "100"}, pos...)...)
	default:
		return fmt.Errorf("unknown compose command %q", sub)
	}
}

const composeEndpoints = `
Infra Monitor is running:
  Grafana:      http://localhost:3000  (user admin, password in .env)
  Prometheus:   http://localhost:9090
  Alertmanager: http://localhost:9093
  Jaeger:       http://localhost:16686  (OTLP: localhost:4317 gRPC, localhost:4318 HTTP)
  Loki:         http://localhost:3100
  cAdvisor:     http://localhost:8080
`

func (a *app) cmdK8s(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infra-monitor k8s render|apply|status|port-forward|delete")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("k8s "+sub, flag.ContinueOnError)
	opts := k8s.Options{}
	var kubeContext string
	fs.StringVar(&opts.Namespace, "namespace", "infra-monitor", "namespace to deploy into")
	fs.StringVar(&opts.Namespace, "n", "infra-monitor", "shorthand for --namespace")
	fs.StringVar(&kubeContext, "context", "", "kubectl context (default: current context)")
	if sub == "render" || sub == "apply" {
		fs.StringVar(&opts.StorageClass, "storage-class", "", "StorageClass for volumes (default: cluster default)")
	}
	purge := false
	if sub == "delete" {
		fs.BoolVar(&purge, "purge", false, "also delete the namespace, including all stored data")
	}
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	kubectl := func(stdin []byte, args ...string) error { return k8s.Kubectl(kubeContext, stdin, args...).Run() }

	switch sub {
	case "render", "apply":
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		out, err := k8s.Render(cfg, opts)
		if err != nil {
			return err
		}
		if sub == "render" {
			_, err := a.stdout.Write(out)
			return err
		}
		// Server-side apply: the dashboards ConfigMap is too large for the
		// last-applied-configuration annotation used by client-side apply.
		if err := kubectl(out, "apply", "--server-side", "--force-conflicts", "--field-manager", "infra-monitor", "-f", "-"); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "\nDeployed to namespace %s. Open Grafana with:\n  infra-monitor k8s port-forward grafana -n %s\n", opts.Namespace, opts.Namespace)
		return nil
	case "status":
		return kubectl(nil, "get", "pods,services,pvc", "--namespace", opts.Namespace)
	case "port-forward":
		if len(pos) < 1 {
			return fmt.Errorf("usage: infra-monitor k8s port-forward SERVICE [LOCAL_PORT]; services: %s", serviceNames())
		}
		svc := pos[0]
		port, ok := k8s.Ports[svc]
		if !ok {
			return fmt.Errorf("unknown service %q; choose one of: %s", svc, serviceNames())
		}
		local := port
		if len(pos) > 1 {
			p, err := strconv.Atoi(pos[1])
			if err != nil {
				return fmt.Errorf("invalid local port %q", pos[1])
			}
			local = p
		}
		fmt.Fprintf(a.stdout, "Forwarding http://localhost:%d -> %s (Ctrl-C to stop)\n", local, svc)
		return kubectl(nil, "port-forward", "--namespace", opts.Namespace, "service/"+svc, fmt.Sprintf("%d:%d", local, port))
	case "delete":
		// Deleting needs no valid credentials; render with whatever exists.
		cfg, err := config.Load(a.envFile())
		if err != nil && !errors.Is(err, config.ErrNotFound) {
			return err
		}
		opts.SkipNamespace = true
		out, err := k8s.Render(cfg, opts)
		if err != nil {
			return err
		}
		if err := kubectl(out, "delete", "--ignore-not-found", "-f", "-"); err != nil {
			return err
		}
		if purge {
			return kubectl(nil, "delete", "namespace", opts.Namespace, "--ignore-not-found")
		}
		fmt.Fprintf(a.stdout, "Stack removed; volumes in namespace %s were kept. Use --purge to delete them.\n", opts.Namespace)
		return nil
	default:
		return fmt.Errorf("unknown k8s command %q", sub)
	}
}

func serviceNames() string {
	names := make([]string, 0, len(k8s.Ports))
	for n := range k8s.Ports {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
