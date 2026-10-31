package plugin

import "testing"

func TestSecurityCanaryPreservesLegacyNotificationIdentity(t *testing.T) {
	r := &Registry{entries: map[string]*entry{"security": {host: newHost(t.TempDir(), "security", nil), ready: true, stored: storedPlugin{Enabled: true}, alerts: []Alert{{Rule: "canary.tripped", Subject: "backup", Severity: "critical", Summary: "Canary used"}}}}}
	alerts := r.Alerts()
	if len(alerts) != 1 || alerts[0].Host != "api" || alerts[0].Rule != "security.canary.tripped" || alerts[0].Subject != "backup" {
		t.Fatalf("legacy canary identity changed %+v", alerts)
	}
}
