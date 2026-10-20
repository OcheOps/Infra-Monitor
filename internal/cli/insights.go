package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/digest"
	"github.com/OcheOps/Infra-Monitor/internal/explain"
	"github.com/OcheOps/Infra-Monitor/internal/promapi"
	"github.com/OcheOps/Infra-Monitor/internal/report"
)

// endpointFlags adds --prometheus and --loki, defaulting to the Compose
// stack on this host (or the app's test override).
func (a *app) endpointFlags(fs *flag.FlagSet) *promapi.Endpoints {
	e := promapi.Local
	if a.endpoints != nil {
		e = *a.endpoints
	}
	fs.StringVar(&e.Prometheus, "prometheus", e.Prometheus, "Prometheus URL (on Kubernetes, run `k8s port-forward prometheus` first)")
	fs.StringVar(&e.Loki, "loki", e.Loki, "Loki URL")
	return &e
}

func (a *app) cmdStatus(args []string) error {
	fs := a.newFlags("status")
	e := a.endpointFlags(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := report.Gather(ctx, promapi.New(*e), false)
	if err != nil {
		return fmt.Errorf("%w\n  Is the stack running? Start it with `infra-monitor compose up`", err)
	}
	r.WriteStatus(a.stdout)
	if len(r.Alerts) > 0 {
		fmt.Fprintln(a.stdout, "\nAsk Claude what an alert means: infra-monitor explain ALERTNAME")
	}
	return nil
}

func (a *app) crontabOrSystem() digest.Crontab {
	if a.crontab != nil {
		return a.crontab
	}
	return digest.SystemCrontab{}
}

func (a *app) cmdDigest(args []string) error {
	sub, rest := "show", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	fs := a.newFlags("digest " + sub)
	e := a.endpointFlags(fs)
	at := fs.String("at", "08:00", "time of day to send it (24-hour, this host's time zone)")
	if _, err := parse(fs, rest); err != nil {
		return err
	}
	cfg, err := a.load()
	if err != nil {
		return err
	}
	switch sub {
	case "show", "send":
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		r, err := report.Gather(ctx, promapi.New(*e), true)
		if err != nil {
			return err
		}
		text := r.Digest("Infra Monitor")
		if sub == "show" {
			fmt.Fprint(a.stdout, text)
			return nil
		}
		channels := digest.Channels(cfg.Notifications)
		if len(channels) == 0 {
			return errors.New("no notification channels; set one up with `infra-monitor notify setup`")
		}
		subject := "Infra Monitor: all good"
		if n := r.Problems(); n > 0 {
			subject = fmt.Sprintf("Infra Monitor: %d problem(s)", n)
		}
		errs := digest.Send(ctx, cfg.Notifications, subject, text)
		for _, err := range errs {
			fmt.Fprintf(a.stderr, "✗ %v\n", err)
		}
		if len(errs) == len(channels) {
			return errors.New("the digest couldn't be sent to any channel")
		}
		fmt.Fprintf(a.stdout, "Sent the digest to %d of %d channel(s).\n", len(channels)-len(errs), len(channels))
		return nil
	case "enable", "schedule":
		var hh, mm int
		if _, err := fmt.Sscanf(*at, "%d:%d", &hh, &mm); err != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(*at) > 5 {
			return fmt.Errorf("--at %q: use HH:MM, e.g. 08:00", *at)
		}
		if len(digest.Channels(cfg.Notifications)) == 0 {
			return errors.New("set up a notification channel first: `infra-monitor notify setup`")
		}
		bin, err := os.Executable()
		if err != nil {
			return err
		}
		dir, err := filepath.Abs(a.dir)
		if err != nil {
			return err
		}
		line := digest.CronLine(hh, mm, bin, dir)
		if _, err := digest.Schedule(a.crontabOrSystem(), line); err != nil {
			return fmt.Errorf("%w\n  Add this line to a crontab yourself instead:\n  %s", err, line)
		}
		fmt.Fprintf(a.stdout, "The digest will be sent every day at %02d:%02d to %s.\n", hh, mm, strings.Join(digest.Channels(cfg.Notifications), ", "))
		fmt.Fprintln(a.stdout, "Preview it with `infra-monitor digest show`, or send one now with `infra-monitor digest send`.")
		return nil
	case "disable", "off":
		had, err := digest.Schedule(a.crontabOrSystem(), "")
		if err != nil {
			return err
		}
		if had {
			fmt.Fprintln(a.stdout, "The daily digest is off.")
		} else {
			fmt.Fprintln(a.stdout, "The daily digest wasn't scheduled.")
		}
		return nil
	case "status":
		line, err := digest.Scheduled(a.crontabOrSystem())
		if err != nil {
			return err
		}
		if line == "" {
			fmt.Fprintln(a.stdout, "The daily digest is off. Turn it on with `infra-monitor digest enable --at 08:00`.")
		} else {
			f := strings.Fields(line)
			fmt.Fprintf(a.stdout, "The daily digest is sent at %02s:%02s (cron: %s).\n", f[1], f[0], line)
		}
		return nil
	default:
		return fmt.Errorf("unknown digest command %q (use show, send, enable, disable or status)", sub)
	}
}

func (a *app) cmdExplain(args []string) error {
	fs := a.newFlags("explain")
	e := a.endpointFlags(fs)
	instance := fs.String("instance", "", "which target, when the alert fires for several")
	dryRun := fs.Bool("dry-run", false, "print what would be sent to Claude, and send nothing")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: infra-monitor explain [ALERTNAME] [--instance INSTANCE] [--dry-run]")
	}
	cfg, err := a.load()
	if err != nil {
		return err
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if !*dryRun {
		if cfg.AI == nil {
			return errors.New("explain sends the alert, metric values and recent error logs to Claude (Anthropic's API), so it's off until you turn it on:\n  infra-monitor ai enable\n  (or see what would be sent with --dry-run)")
		}
		if key == "" {
			key = cfg.AI.APIKey
		}
		if key == "" {
			return errors.New("no Anthropic API key: set ANTHROPIC_API_KEY or run `infra-monitor ai enable`")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := promapi.New(*e)
	name := ""
	if len(pos) == 1 {
		name = pos[0]
	} else {
		alerts, err := client.Alerts(ctx)
		if err != nil {
			return fmt.Errorf("can't reach Prometheus at %s: %w", e.Prometheus, err)
		}
		if len(alerts) == 0 {
			return errors.New("no alerts are pending or firing; name one to explain its rule: infra-monitor explain ALERTNAME")
		}
		if !a.interactive {
			var names []string
			for _, al := range alerts {
				names = append(names, al.Name)
			}
			return fmt.Errorf("which alert? active now: %s", strings.Join(names, ", "))
		}
		opts := make([]string, len(alerts))
		for i, al := range alerts {
			opts[i] = fmt.Sprintf("%s (%s) %s", al.Name, al.State, al.Labels["instance"])
		}
		i, err := a.p.Select("Which alert?", opts, 0)
		if err != nil {
			return err
		}
		name, *instance = alerts[i].Name, alerts[i].Labels["instance"]
	}

	var overview bytes.Buffer
	if r, err := report.Gather(ctx, client, false); err == nil {
		r.WriteStatus(&overview)
	}
	ec, err := explain.Gather(ctx, client, name, *instance, overview.String())
	if err != nil {
		return err
	}
	prompt := ec.Prompt()
	if *dryRun {
		fmt.Fprintf(a.stdout, "Model: %s\n\n--- system ---\n%s\n\n--- user ---\n%s", aiModel(cfg), explain.System, prompt)
		return nil
	}
	fmt.Fprintf(a.stderr, "Asking Claude (%s) about %s. Sending %s.\n\n", aiModel(cfg), name, ec.Describe())
	return explain.Ask(ctx, explain.Options{APIKey: key, Model: aiModel(cfg), BaseURL: a.aiBaseURL}, prompt, a.stdout)
}

func aiModel(cfg config.Config) string {
	if cfg.AI != nil && cfg.AI.Model != "" {
		return cfg.AI.Model
	}
	return config.DefaultAIModel
}

func (a *app) cmdAI(args []string) error {
	sub, rest := "status", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	fs := a.newFlags("ai " + sub)
	model := fs.String("model", "", "Claude model id (default "+config.DefaultAIModel+")")
	keyStdin := fs.Bool("api-key-stdin", false, "read the API key from standard input")
	if _, err := parse(fs, rest); err != nil {
		return err
	}
	cfg, err := a.load()
	if err != nil {
		return err
	}
	switch sub {
	case "enable", "on":
		ai := config.AI{}
		if cfg.AI != nil {
			ai = *cfg.AI
		}
		if *model != "" {
			ai.Model = *model
		}
		switch {
		case *keyStdin:
			b, err := readAll(os.Stdin)
			if err != nil {
				return err
			}
			ai.APIKey = strings.TrimSpace(b)
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			fmt.Fprintln(a.stdout, "Using the API key from ANTHROPIC_API_KEY (not saved).")
		case a.interactive:
			a.p.Printf("Get an API key at https://console.anthropic.com/settings/keys. It's stored in %s, which only you can read.\n", config.Path(a.dir))
			if ai.APIKey, err = a.p.Secret("Anthropic API key", ai.APIKey, func(v string) error {
				if !strings.HasPrefix(v, "sk-ant-") {
					return errors.New("Anthropic API keys start with sk-ant-")
				}
				return nil
			}); err != nil {
				return err
			}
		case ai.APIKey == "":
			return errors.New("no API key: set ANTHROPIC_API_KEY, pass --api-key-stdin, or run this in a terminal")
		}
		cfg.AI = &ai
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "`infra-monitor explain` is on, using %s. It sends the alert, its rule, recent metric values, error-looking log lines (credentials masked) and a status overview to Anthropic's API.\n", aiModel(cfg))
		return nil
	case "disable", "off":
		cfg.AI = nil
		if err := a.save(cfg); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "`infra-monitor explain` is off, and the stored API key was removed.")
		return nil
	case "status":
		if cfg.AI == nil {
			fmt.Fprintln(a.stdout, "AI explanations are off. Turn them on with `infra-monitor ai enable`.")
			return nil
		}
		src := "config.yaml"
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			src = "ANTHROPIC_API_KEY"
		} else if cfg.AI.APIKey == "" {
			src = "none set"
		}
		fmt.Fprintf(a.stdout, "AI explanations are on: model %s, API key from %s.\n", aiModel(cfg), src)
		return nil
	default:
		return fmt.Errorf("unknown ai command %q (use enable, disable or status)", sub)
	}
}

func readAll(f *os.File) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(f)
	return b.String(), err
}
