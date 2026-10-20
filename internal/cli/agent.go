package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/OcheOps/Infra-Monitor/internal/compose"
	"github.com/OcheOps/Infra-Monitor/internal/config"
)

// installCommand is how a new host gets the CLI.
const installCommand = "curl -fsSL https://raw.githubusercontent.com/OcheOps/Infra-Monitor/main/install.sh | sh"

func (a *app) cmdAgent(args []string) error {
	sub, rest := "list", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "add":
		return a.agentAdd(rest)
	case "list", "ls":
		return a.agentList()
	case "remove", "rm":
		return a.agentRemove(rest)
	case "join":
		return a.agentJoin(rest)
	case "leave":
		return a.agentLeave()
	default:
		return fmt.Errorf("unknown agent command %q (server: add, list, remove; agent host: join, leave)", sub)
	}
}

// ---- server side --------------------------------------------------------

func (a *app) agentAdd(args []string) error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	if cfg.IsAgent() {
		return errors.New("this host is itself an agent; run `agent add` on the server")
	}
	if len(args) != 1 {
		return errors.New("usage: infra-monitor agent add NAME   (e.g. web-2)")
	}
	name := args[0]
	if slices.ContainsFunc(cfg.Agents, func(ag config.Agent) bool { return ag.Name == name }) {
		return fmt.Errorf("an agent named %q already exists; remove it first to issue a new token", name)
	}
	host := cfg.PublicHost()
	if host == "" {
		return errors.New("agents need to reach this server over HTTPS; set a domain first with `infra-monitor expose --domain mon.example.com`")
	}

	var caPin string
	if cfg.Gateway.TLS == "internal" {
		pemBytes, err := a.readGatewayCA()
		if err != nil {
			return fmt.Errorf("couldn't read the gateway's CA certificate (%v); start the stack with `infra-monitor compose up` first, so the gateway creates it", err)
		}
		if caPin, err = caFingerprint(pemBytes); err != nil {
			return err
		}
	}

	token, err := newToken()
	if err != nil {
		return err
	}
	// The token is 256 random bits, so bcrypt's work factor adds nothing
	// against guessing; the minimum keeps per-request checks cheap.
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.MinCost)
	if err != nil {
		return err
	}
	cfg.Agents = append(cfg.Agents, config.Agent{Name: name, PasswordHash: string(hash)})
	if err := a.save(cfg); err != nil {
		return err
	}

	join := fmt.Sprintf("infra-monitor agent join https://%s --name %s --token %s", host, name, token)
	if caPin != "" {
		join += " --ca-sha256 " + caPin
	}
	fmt.Fprintf(a.stdout, "Agent %q registered. On that host, run:\n\n  %s\n  %s\n\n", name, installCommand, join)
	fmt.Fprintln(a.stdout, "The token is shown only this once. If you lose it, remove the agent and add it again.")
	fmt.Fprintln(a.stdout, "Without public DNS for the server, add --server-ip IP to the join command.")
	return a.changed()
}

func (a *app) agentList() error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	if cfg.IsAgent() {
		fmt.Fprintf(a.stdout, "This host is agent %q of %s.\n", cfg.Agent.Name, cfg.Agent.Server)
		return nil
	}
	if len(cfg.Agents) == 0 {
		fmt.Fprintln(a.stdout, "No agents yet. Add one with `infra-monitor agent add NAME`.")
		return nil
	}
	seen := a.agentsLastSeen()
	names := make([]string, 0, len(cfg.Agents))
	for _, ag := range cfg.Agents {
		names = append(names, ag.Name)
	}
	sort.Strings(names)
	for _, n := range names {
		status := "never seen (has `agent join` run on that host?)"
		if t, ok := seen[n]; ok {
			ago := time.Since(t).Round(time.Second)
			status = "last seen " + ago.String() + " ago"
			if ago > 5*time.Minute {
				status += " (silent)"
			}
		} else if seen == nil {
			status = "last seen: unknown (Prometheus not reachable on localhost:9090)"
		}
		fmt.Fprintf(a.stdout, "%-24s %s\n", n, status)
	}
	return nil
}

// agentsLastSeen asks the local Prometheus when each agent last reported.
// It returns nil when Prometheus can't be reached.
func (a *app) agentsLastSeen() map[string]time.Time {
	q := url.QueryEscape(`max by (agent) (timestamp(up{job="node",agent!=""}))`)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:9090/api/v1/query?query=" + q)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	out := map[string]time.Time{}
	for _, r := range body.Data.Result {
		var secs float64
		if s, ok := r.Value[1].(string); ok {
			fmt.Sscan(s, &secs)
		}
		out[r.Metric["agent"]] = time.Unix(int64(secs), 0)
	}
	return out
}

func (a *app) agentRemove(args []string) error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: infra-monitor agent remove NAME")
	}
	before := len(cfg.Agents)
	cfg.Agents = slices.DeleteFunc(cfg.Agents, func(ag config.Agent) bool { return ag.Name == args[0] })
	if len(cfg.Agents) == before {
		return fmt.Errorf("no agent named %q; see `infra-monitor agent list`", args[0])
	}
	if err := a.save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Removed agent %s; its token stops working once you apply. Its old data stays until retention expires.\n", args[0])
	return a.changed()
}

// readGatewayCA returns the gateway's internal root CA certificate (PEM).
func (a *app) readGatewayCA() ([]byte, error) {
	if a.gatewayCA != nil {
		return a.gatewayCA()
	}
	out, err := exec.Command("docker", "compose", "--project-name", compose.ProjectName,
		"exec", "-T", "gateway", "cat", "/data/caddy/pki/authorities/local/root.crt").Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- agent host side ------------------------------------------------------

func (a *app) agentJoin(args []string) error {
	fs := a.newFlags("agent join")
	ag := config.AgentConfig{}
	fs.StringVar(&ag.Name, "name", "", "this host's name, as registered with `agent add` on the server")
	fs.StringVar(&ag.Token, "token", "", "token printed by `agent add`")
	fs.StringVar(&ag.CASHA256, "ca-sha256", "", "fingerprint of the server's own CA (servers using --tls internal)")
	fs.StringVar(&ag.ServerIP, "server-ip", "", "server IP, when its hostname doesn't resolve from this host")
	force := fs.Bool("force", false, "replace this host's existing server configuration")
	noStart := fs.Bool("no-start", false, "save the configuration without starting the agent")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: infra-monitor agent join https://SERVER --name NAME --token TOKEN")
	}
	ag.Server = strings.TrimRight(pos[0], "/")
	ag.CASHA256 = strings.ToLower(ag.CASHA256)

	if existing, err := a.load(); err == nil && !existing.IsAgent() && !*force {
		return errors.New("this host is set up as a server (it has its own config); use --force to turn it into an agent instead")
	}
	cfg := config.Config{Agent: &ag}
	if err := cfg.Validate(); err != nil {
		return err
	}

	var ca []byte
	if ag.CASHA256 != "" {
		if ca, err = fetchPinnedCA(ag); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "Server CA certificate matches the fingerprint.")
	}
	if err := pingServer(ag, ca); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Connected to %s as %q.\n", ag.Server, ag.Name)

	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return err
	}
	if ca != nil {
		if err := os.WriteFile(a.caPath(), ca, 0o600); err != nil {
			return err
		}
	}
	if err := a.save(cfg); err != nil {
		return err
	}
	if *noStart {
		fmt.Fprintln(a.stdout, "Saved. Start the agent with `infra-monitor compose up`.")
		return nil
	}
	return a.composeUp(false)
}

func (a *app) agentLeave() error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	if !cfg.IsAgent() {
		return errors.New("this host isn't an agent")
	}
	if err := compose.Command(compose.AgentProjectName, a.agentDir(), "down").Run(); err != nil {
		fmt.Fprintf(a.stderr, "warning: stopping the agent failed: %v\n", err)
	}
	for _, p := range []string{config.Path(a.dir), a.caPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	fmt.Fprintf(a.stdout, "Left %s. Remove it on the server too: infra-monitor agent remove %s\n", cfg.Agent.Server, cfg.Agent.Name)
	return nil
}

// agentHTTPClient talks to the server, trusting ca (if set) and dialing
// ServerIP for the server's hostname (if set).
func agentHTTPClient(ag config.AgentConfig, ca []byte, insecure bool) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("the server CA certificate isn't valid PEM")
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	// Used only to download the server's CA, which is then checked against
	// the pinned fingerprint before anything trusts it.
	tr.TLSClientConfig.InsecureSkipVerify = insecure
	if ip := ag.ServerIP; ip != "" && ip != "host-gateway" {
		u, err := url.Parse(ag.Server)
		if err != nil {
			return nil, err
		}
		host := u.Hostname()
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if h, port, err := net.SplitHostPort(addr); err == nil && h == host {
				addr = net.JoinHostPort(ip, port)
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}, nil
}

// fetchPinnedCA downloads the server's CA certificate and checks it against
// the fingerprint from the join command.
func fetchPinnedCA(ag config.AgentConfig) ([]byte, error) {
	client, err := agentHTTPClient(ag, nil, true)
	if err != nil {
		return nil, err
	}
	resp, err := client.Get(ag.Server + "/ingest/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w", ag.Server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server didn't provide its CA certificate (%s); does it use `expose --tls internal`?", resp.Status)
	}
	pemBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	got, err := caFingerprint(pemBytes)
	if err != nil {
		return nil, err
	}
	if got != ag.CASHA256 {
		return nil, fmt.Errorf("the server's CA certificate doesn't match --ca-sha256 (got %s); check you're connecting to the right server", got)
	}
	return pemBytes, nil
}

// pingServer checks the agent's credentials before anything is saved.
func pingServer(ag config.AgentConfig, ca []byte) error {
	client, err := agentHTTPClient(ag, ca, false)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, ag.Server+"/ingest/ping", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(ag.Name, ag.Token)
	resp, err := client.Do(req)
	if err != nil {
		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) && ag.CASHA256 == "" {
			return fmt.Errorf("the server's certificate isn't trusted (%v); if it uses `expose --tls internal`, add the --ca-sha256 from `agent add`", err)
		}
		return fmt.Errorf("couldn't reach %s: %w", ag.Server, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return errors.New("the server rejected this name and token; check them, and that `infra-monitor compose up` (or `k8s apply`) ran on the server after `agent add`")
	case http.StatusNotFound, http.StatusBadGateway:
		return fmt.Errorf("the server isn't accepting agents yet (%s); apply on the server after `agent add`", resp.Status)
	default:
		return fmt.Errorf("unexpected response from the server: %s", resp.Status)
	}
}

// caFingerprint is the SHA-256 of the first certificate in pemBytes.
func caFingerprint(pemBytes []byte) (string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("not a PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", err
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// newToken returns 256 random bits, URL-safe.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
