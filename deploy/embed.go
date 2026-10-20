// Package deploy embeds the stack definitions shipped inside the CLI binary.
//
//   - compose/  Docker Compose bundle (single host)
//   - k8s/      Kubernetes manifest templates and k8s-specific configs
//   - agent/    Compose bundle for agent hosts (`agent join`)
//   - integrations/  exporter catalog for `integrations add`
//   - shared/   alert rules, Alertmanager config and Grafana provisioning used by both
package deploy

import "embed"

//go:embed agent compose integrations shared k8s
var FS embed.FS
