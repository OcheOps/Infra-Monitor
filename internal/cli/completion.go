package cli

import (
	"fmt"
	"sort"
	"strings"
)

// completionTree lists the words that may follow each command path. Keys are
// space-separated paths of the words already typed ("" is the top level).
var completionTree = map[string][]string{
	"":                 {"init", "probes", "targets", "notify", "config", "expose", "statuspage", "compose", "k8s", "doctor", "completion", "version", "help"},
	"expose":           {"off"},
	"statuspage":       {"show", "enable", "disable", "add", "remove"},
	"probes":           {"list", "add", "remove"},
	"targets":          {"list", "add", "remove"},
	"notify":           {"list", "setup", "add", "remove", "test"},
	"notify add":       channelNames,
	"notify remove":    channelNames,
	"config":           {"show", "path"},
	"compose":          {"up", "down", "restart", "status", "logs", "sync"},
	"compose logs":     {"prometheus", "alertmanager", "grafana", "node-exporter", "blackbox", "loki", "fluentbit", "jaeger", "cadvisor"},
	"k8s":              {"configure", "apply", "render", "status", "port-forward", "delete"},
	"k8s port-forward": {"grafana", "prometheus", "alertmanager", "jaeger", "loki"},
	"doctor":           {"compose", "k8s"},
	"completion":       {"bash", "zsh", "fish"},
}

// dynamicCompletions are shell snippets that print candidates at runtime.
var dynamicCompletions = map[string]string{
	"targets remove": `infra-monitor targets list 2>/dev/null | awk 'NF>1 && $1 !~ /^No$/ {print $1}'`,
}

func sortedPaths() []string {
	paths := make([]string, 0, len(completionTree)+len(dynamicCompletions))
	for p := range completionTree {
		paths = append(paths, p)
	}
	for p := range dynamicCompletions {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func (a *app) cmdCompletion(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: infra-monitor completion bash|zsh|fish")
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(a.stdout, bashCompletion())
	case "zsh":
		fmt.Fprint(a.stdout, "#compdef infra-monitor\n# zsh completion for infra-monitor (uses bash completion)\nautoload -U +X bashcompinit && bashcompinit\n"+bashCompletion())
	case "fish":
		fmt.Fprint(a.stdout, fishCompletion())
	default:
		return fmt.Errorf("unknown shell %q (use bash, zsh or fish)", args[0])
	}
	return nil
}

func bashCompletion() string {
	var b strings.Builder
	b.WriteString(`# bash completion for infra-monitor
# Install: infra-monitor completion bash > /etc/bash_completion.d/infra-monitor
_infra_monitor() {
  local cur="${COMP_WORDS[COMP_CWORD]}" path="" i word skip=0
  for ((i = 1; i < COMP_CWORD; i++)); do
    word="${COMP_WORDS[i]}"
    if ((skip)); then skip=0; continue; fi
    case "$word" in
      --dir|-n|--namespace|--context|--path) skip=1; continue ;;
      -*) continue ;;
    esac
    path="${path:+$path }$word"
  done
  local opts=""
  case "$path" in
`)
	for _, p := range sortedPaths() {
		if dyn, ok := dynamicCompletions[p]; ok {
			fmt.Fprintf(&b, "    %q) opts=\"$(%s)\" ;;\n", p, dyn)
		} else {
			fmt.Fprintf(&b, "    %q) opts=%q ;;\n", p, strings.Join(completionTree[p], " "))
		}
	}
	b.WriteString(`  esac
  COMPREPLY=()
  local line
  while IFS= read -r line; do COMPREPLY+=("$line"); done < <(compgen -W "$opts" -- "$cur")
}
complete -F _infra_monitor infra-monitor
`)
	return b.String()
}

func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# fish completion for infra-monitor\n# Install: infra-monitor completion fish > ~/.config/fish/completions/infra-monitor.fish\n")
	b.WriteString("complete -c infra-monitor -f\n")
	b.WriteString("complete -c infra-monitor -l dir -r -d 'config directory'\n")
	for _, p := range sortedPaths() {
		var cond []string
		if p == "" {
			cond = append(cond, "__fish_use_subcommand")
		} else {
			for _, w := range strings.Fields(p) {
				cond = append(cond, "__fish_seen_subcommand_from "+w)
			}
		}
		words := completionTree[p]
		if len(words) > 0 && p != "" {
			cond = append(cond, "not __fish_seen_subcommand_from "+strings.Join(words, " "))
		}
		arg := "'" + strings.Join(words, " ") + "'"
		if dyn, ok := dynamicCompletions[p]; ok {
			arg = "'(" + strings.ReplaceAll(dyn, "'", `\'`) + ")'"
		}
		fmt.Fprintf(&b, "complete -c infra-monitor -n '%s' -a %s\n", strings.Join(cond, "; and "), arg)
	}
	return b.String()
}
