// Package stack holds what both backends share: pinned images and the
// rendered Alertmanager configuration.
package stack

import (
	"os"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
)

// Images pins every component. Keep in sync with deploy/compose/docker-compose.yml
// (enforced by TestComposeImagesMatch).
var Images = map[string]string{
	"prometheus":    "prom/prometheus:v2.54.1",
	"alertmanager":  "prom/alertmanager:v0.27.0",
	"grafana":       "grafana/grafana:11.2.2",
	"node_exporter": "prom/node-exporter:v1.8.2",
	"blackbox":      "prom/blackbox-exporter:v0.25.0",
	"loki":          "grafana/loki:3.1.1",
	"fluentbit":     "cr.fluentbit.io/fluent/fluent-bit:3.1.7",
	"jaeger":        "jaegertracing/all-in-one:1.60",
	"cadvisor":      "gcr.io/cadvisor/cadvisor:v0.49.1",
}

// AlertmanagerConfig renders alertmanager.yml: Telegram when configured,
// otherwise a receiver that drops notifications.
func AlertmanagerConfig(c config.Config) ([]byte, error) {
	if !c.TelegramEnabled() {
		return deploy.FS.ReadFile("shared/alertmanager-null.yml")
	}
	tmpl, err := deploy.FS.ReadFile("shared/alertmanager.yml.tmpl")
	if err != nil {
		return nil, err
	}
	vals := map[string]string{
		config.KeyTelegramToken:  c.TelegramToken,
		config.KeyTelegramChatID: c.TelegramChatID,
	}
	out := os.Expand(string(tmpl), func(k string) string {
		if v, ok := vals[k]; ok {
			return v
		}
		return "${" + k + "}"
	})
	return []byte(out), nil
}
