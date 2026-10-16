package stack

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
)

func TestComposeImagesMatch(t *testing.T) {
	compose, err := deploy.FS.ReadFile("compose/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range regexp.MustCompile(`(?m)^\s+image:\s*(\S+)`).FindAllStringSubmatch(string(compose), -1) {
		got = append(got, m[1])
	}
	var want []string
	for _, img := range Images {
		want = append(want, img)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("compose images differ from stack.Images\ncompose:\n%s\nstack.Images:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAlertmanagerConfig(t *testing.T) {
	out, err := AlertmanagerConfig(config.Config{TelegramToken: "123:abc", TelegramChatID: "-42"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`bot_token: "123:abc"`, "chat_id: -42", "{{ .CommonAnnotations.summary }}"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered config missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "${") {
		t.Errorf("unexpanded variable in:\n%s", s)
	}

	out, err = AlertmanagerConfig(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "telegram") {
		t.Errorf("expected null receiver without Telegram, got:\n%s", out)
	}
}
