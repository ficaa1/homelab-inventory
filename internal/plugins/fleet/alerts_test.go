package fleet

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func TestAlertsFromOverviewPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stale        int
		warn, crit   float64
		staleC       bool
		diskA, diskC string
	}{
		{"hub defaults", 259200, .15, .08, true, "warning", "critical"},
		{"inclusive boundaries", 345600, 61.0 / 512, .04, false, "", "warning"},
		{"higher critical threshold", 300, .30, .20, true, "critical", "critical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(fixture(t, "overview"), &data); err != nil {
				t.Fatal(err)
			}
			pol := data["policy"].(map[string]any)
			pol["stale_after_seconds"], pol["disk_warn_free_ratio"], pol["disk_crit_free_ratio"] = tc.stale, tc.warn, tc.crit
			h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/overview" {
					t.Error("alerts used another route")
				}
				json.NewEncoder(w).Encode(data)
			})
			alerts, err := h.Alerts(t.Context())
			if err != nil || h.calls.Load() != 1 {
				t.Fatal("poll failed", err)
			}
			assertPrivate(t, alerts)
			type key struct{ rule, subject string }
			got := map[key]plugin.Alert{}
			for _, a := range alerts {
				got[key{a.Rule, a.Subject}] = a
			}
			want := map[key]string{
				{"host_stale", "host-b"}: "warning", {"drift", "host-a"}: "warning", {"refused", "host-a"}: "warning", {"trial_due", "sample-skills"}: "info",
				{"disk", "host-c"}: tc.diskC,
			}
			if tc.stale < 720 {
				want[key{"host_stale", "host-a"}] = "warning"
			}
			if tc.staleC {
				want[key{"host_stale", "host-c"}] = "warning"
			}
			if tc.diskA != "" {
				want[key{"disk", "host-a"}] = tc.diskA
			}
			if len(want) != len(alerts) {
				t.Fatal(alerts)
			}
			for k, sev := range want {
				if got[k].Severity != sev {
					t.Errorf("%v severity %q, want %q", k, got[k].Severity, sev)
				}
			}
			if got[key{"drift", "host-a"}].Summary != "host-a: 2 drifted files, 1 broken link" || got[key{"host_stale", "host-b"}].Summary != "host-b has never reported" {
				t.Fatal(alerts)
			}
			if tc.staleC && got[key{"host_stale", "host-c"}].Summary != "host-c last reported 4 days ago" {
				t.Fatal(alerts)
			}
			if tc.diskA != "" && !strings.Contains(got[key{"disk", "host-a"}].Summary, "61 GB free of 512 GB (12%)") {
				t.Fatal(alerts)
			}
		})
	}
}
func TestPollFailureAndSilence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		fail       bool
	}{
		{"HTTP failure", `{"error":{"message":"Retry read-fixture-only operator-fixture-only"}}`, 500, true},
		{"incomplete", "{", 200, true},
		{"incompatible", `{"api_version":2}`, 200, true},
		{"unknown hosts", `{"api_version":1,"generated_at":"2026-12-01T09:15:00Z","policy":{},"hosts":null,"trials":[]}`, 200, true},
		{"empty", `{"api_version":1,"generated_at":"2026-12-01T09:15:00Z","policy":{"stale_after_seconds":60,"disk_warn_free_ratio":0.2,"disk_crit_free_ratio":0.1,"trial_due_soon_days":7},"hosts":[],"trials":[]}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) })
			a, err := h.Alerts(t.Context())
			if (err != nil) != tc.fail || len(a) > 0 || h.calls.Load() != 1 {
				t.Fatal(a, err)
			}
			if err != nil {
				assertPrivate(t, err.Error())
			}
		})
	}
}
func TestAlertTextIsSafe(t *testing.T) {
	data := string(fixture(t, "overview"))
	data = strings.ReplaceAll(data, "host-a", `host-<>\u0001a-read-fixture-only`)
	data = strings.ReplaceAll(data, "sample-skills", "<sample>-operator-fixture-only")
	h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(data)) })
	alerts, err := h.Alerts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, alerts)
	assertText(t, alerts, "host-a-[redacted]", "sample-[redacted]")
	for _, a := range alerts {
		if strings.ContainsAny(a.Subject+a.Summary, "<>\u0001") {
			t.Fatal("unsafe alert")
		}
	}
}
