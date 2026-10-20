package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/OcheOps/Infra-Monitor/internal/compose"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/integrations"
)

func (a *app) cmdIntegrations(args []string) error {
	sub, rest := "list", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		return a.integrationsList()
	case "detect", "scan":
		return a.integrationsDetect(rest)
	case "add":
		return a.integrationsAdd(rest)
	case "remove", "rm", "delete":
		return a.integrationsRemove(rest)
	default:
		return fmt.Errorf("unknown integrations command %q (use list, detect, add or remove)", sub)
	}
}

func (a *app) integrationsList() error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	if len(cfg.Integrations) == 0 {
		fmt.Fprintln(a.stdout, "No integrations. Find services on this host with `infra-monitor integrations detect`.")
		return nil
	}
	for _, in := range cfg.Integrations {
		extra := ""
		if in.Network != "" {
			extra = " (network " + in.Network + ")"
		}
		fmt.Fprintf(a.stdout, "%-20s %-9s %s%s\n", in.Name, in.Type, in.Address, extra)
	}
	return nil
}

// detectRunner runs commands for integrations.Detect; tests replace it.
var detectRunner integrations.Runner = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

func (a *app) integrationsDetect(args []string) error {
	fs := a.newFlags("integrations detect")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := a.load()
	if err != nil {
		return err
	}
	var procNet []string
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(f); err == nil {
			procNet = append(procNet, string(b))
		}
	}
	found := integrations.Detect(detectRunner, procNet, []string{compose.ProjectName, compose.AgentProjectName})
	// Skip what's already monitored.
	found = slices.DeleteFunc(found, func(c integrations.Candidate) bool {
		return slices.ContainsFunc(cfg.Integrations, func(in config.Integration) bool {
			return in.Type == c.Type && in.Address == c.Address
		})
	})
	if len(found) == 0 {
		fmt.Fprintf(a.stdout, "Found no new services to monitor. Supported: %s.\n", strings.Join(integrations.Types(), ", "))
		fmt.Fprintln(a.stdout, "Add one by hand with `infra-monitor integrations add TYPE NAME HOST:PORT`.")
		return nil
	}

	fmt.Fprintf(a.stdout, "Found %d service(s):\n", len(found))
	for _, c := range found {
		fmt.Fprintf(a.stdout, "  %-9s %s → %s\n", c.Type, c.Source, c.Address)
		if c.Warning != "" {
			fmt.Fprintf(a.stdout, "            ! %s\n", c.Warning)
		}
	}
	if !a.interactive {
		fmt.Fprintln(a.stdout, "\nAdd them with:")
		for _, c := range found {
			fmt.Fprintf(a.stdout, "  infra-monitor integrations add %s\n", strings.Join(addArgs(c), " "))
		}
		return nil
	}

	added := 0
	for _, c := range found {
		ok, err := a.p.Confirm(fmt.Sprintf("Monitor %s at %s?", integrations.Specs[c.Type].Title, c.Address), c.Warning == "" || c.Type == "nginx")
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		in := config.Integration{Type: c.Type, Name: uniqueName(cfg, c.Name), Address: c.Address, Network: c.Network}
		if err := a.askCredentials(&in); err != nil {
			return err
		}
		cfg.Integrations = append(cfg.Integrations, in)
		added++
	}
	if added == 0 {
		return nil
	}
	if err := a.save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Added %d integration(s).\n", added)
	return a.changed()
}

// addArgs is the `integrations add` command line for a candidate.
func addArgs(c integrations.Candidate) []string {
	args := []string{c.Type, c.Name, c.Address}
	if c.Network != "" {
		args = append(args, "--network", c.Network)
	}
	if spec := integrations.Specs[c.Type]; spec.DefaultUser != "" || c.Type == "mongodb" {
		args = append(args, "--username", orDefault(spec.DefaultUser, "USER"), "--password", "PASSWORD")
	}
	return args
}

func uniqueName(cfg config.Config, name string) string {
	taken := func(n string) bool {
		return slices.ContainsFunc(cfg.Integrations, func(in config.Integration) bool { return in.Name == n })
	}
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", name, i); !taken(n) {
			return n
		}
	}
}

// askCredentials prompts for the login the exporter needs, if any.
func (a *app) askCredentials(in *config.Integration) error {
	spec := integrations.Specs[in.Type]
	switch in.Type {
	case "postgres", "mysql", "mongodb", "redis":
	default:
		if spec.Hint != "" {
			a.p.Printf("  %s\n", spec.Hint)
		}
		return nil
	}
	if spec.Hint != "" {
		a.p.Printf("  %s\n", spec.Hint)
	}
	optional := in.Type == "redis" || in.Type == "mongodb"
	label := "Username"
	if optional {
		label += " (blank if none)"
	}
	user, err := a.p.Input(label, orDefault(in.Username, spec.DefaultUser), nil)
	if err != nil {
		return err
	}
	in.Username = user
	if user == "" && optional {
		return nil
	}
	in.Password, err = a.p.Secret("Password", in.Password, nil)
	return err
}

func (a *app) integrationsAdd(args []string) error {
	fs := a.newFlags("integrations add")
	var in config.Integration
	fs.StringVar(&in.Network, "network", "", "Docker network the service is on (the exporter joins it)")
	fs.StringVar(&in.Username, "username", "", "login for databases (default: postgres for PostgreSQL, exporter for MySQL)")
	fs.StringVar(&in.Password, "password", "", "password for that login (prompted for when interactive)")
	fs.StringVar(&in.Database, "database", "", "PostgreSQL database to connect to (default postgres)")
	fs.StringVar(&in.Path, "path", "", "Nginx stub_status path or RabbitMQ metrics path")
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "usage: infra-monitor integrations add TYPE NAME HOST:PORT [flags]\n\nTYPE is one of: %s\n\n", strings.Join(integrations.Types(), ", "))
		fs.PrintDefaults()
	}
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 3 {
		fs.Usage()
		return errors.New("usage: infra-monitor integrations add TYPE NAME HOST:PORT")
	}
	in.Type, in.Name, in.Address = pos[0], pos[1], pos[2]
	if _, ok := integrations.Specs[in.Type]; !ok {
		return fmt.Errorf("unknown type %q (supported: %s)", in.Type, strings.Join(integrations.Types(), ", "))
	}
	cfg, err := a.load()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(cfg.Integrations, func(x config.Integration) bool { return x.Name == in.Name }) {
		return fmt.Errorf("an integration named %q already exists", in.Name)
	}
	if in.Password == "" && a.interactive && (in.Type == "postgres" || in.Type == "mysql" || in.Username != "") {
		if in.Password, err = a.p.Secret("Password for "+orDefault(in.Username, integrations.Specs[in.Type].DefaultUser), "", nil); err != nil {
			return err
		}
	}
	cfg.Integrations = append(cfg.Integrations, in)
	if err := a.save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Added %s integration %s (%s)\n", integrations.Specs[in.Type].Title, in.Name, in.Address)
	return a.changed()
}

func (a *app) integrationsRemove(names []string) error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if !a.interactive || len(cfg.Integrations) == 0 {
			return errors.New("usage: infra-monitor integrations remove NAME [NAME...]")
		}
		var opts []string
		for _, in := range cfg.Integrations {
			opts = append(opts, in.Name+" ("+in.Type+", "+in.Address+")")
		}
		i, err := a.p.Select("Which integration should be removed?", opts, -1)
		if err != nil {
			return err
		}
		names = []string{cfg.Integrations[i].Name}
	}
	for _, n := range names {
		i := slices.IndexFunc(cfg.Integrations, func(in config.Integration) bool { return in.Name == n })
		if i < 0 {
			return fmt.Errorf("no integration named %q; see `infra-monitor integrations list`", n)
		}
		cfg.Integrations = slices.Delete(cfg.Integrations, i, i+1)
		fmt.Fprintf(a.stdout, "Removed integration %s\n", n)
	}
	if err := a.save(cfg); err != nil {
		return err
	}
	return a.changed()
}

func (a *app) menuIntegrations() error {
	if err := a.integrationsList(); err != nil {
		return err
	}
	i, err := a.p.Select("Integrations:", []string{"Find services on this host", "Remove", "Back"}, 0)
	if err != nil {
		return err
	}
	switch i {
	case 0:
		return a.integrationsDetect(nil)
	case 1:
		return a.integrationsRemove(nil)
	}
	return nil
}
