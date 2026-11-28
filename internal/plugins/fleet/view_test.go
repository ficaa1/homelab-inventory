package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func assertText(t *testing.T, v any, parts ...string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range parts {
		if !strings.Contains(string(b), s) {
			t.Fatalf("missing %q in %s", s, b)
		}
	}
}

func TestManifestAndViews(t *testing.T) {
	h := fixtureHub(t)
	m := h.Manifest()
	if m.ID != "fleet" || m.Version != "1" || m.Name != "Fleet" || m.Tab.Icon != "grid" || len(m.Actions) > 0 {
		t.Fatal(m)
	}
	var names []string
	for _, v := range m.Tab.Views {
		names = append(names, v.Name)
	}
	if !slices.Equal(names, []string{"overview", "hosts", "skills", "trials", "host"}) {
		t.Fatal(names)
	}
	for _, tc := range []struct {
		name, host string
		check      func(*testing.T, plugin.View)
	}{
		{"overview", "", func(t *testing.T, v plugin.View) {
			stats := v.Blocks[0].Items
			if stats[0].Value != "3" || stats[0].Hint != "2 stale" || stats[1].Value != "4" || stats[1].Hint != "from 2 of 3 hosts" || stats[2].Value != "1" || stats[2].Hint != "1 past end date · 0 end within 7 days" || stats[2].Tone != "warn" || stats[3].Value != "1" {
				t.Fatal(stats)
			}
			hosts := v.Blocks[1].Items
			if hosts[0].Subtitle != "Reported 12 min ago · fleet 0.7.0" || hosts[0].Meta != "61 GB free of 512 GB (12%)" || hosts[0].Link.Params["name"] != "host-a" || hosts[1].Subtitle != "Never reported" {
				t.Fatal(hosts)
			}
			assertText(t, hosts[0], "2 drift", "1 refused", "1 broken link", "Pending sync", "Disk low")
			assertText(t, v.Blocks[2], "Characters", "2280", "9120")
			assertText(t, v.Blocks[3], "Context cost not reported for host-b")
			assertText(t, v.Blocks[4], "Set skill mode · 3 h ago", "Done", "Pending on 2 hosts")
		}},
		{"hosts", "", func(t *testing.T, v plugin.View) {
			assertText(t, v.Blocks[0], "2 updates · 3 pending", "Disk not reported", "Counts not reported")
			rows := v.Blocks[1].Rows
			if rows[0]["status"] != "Attention" || rows[0]["free"] != 61.0/512 || rows[1]["status"] != "Stale" || rows[1]["drift"] != "—" || rows[2]["status"] != "Stale" || rows[2]["updates"] != 0 {
				t.Fatal(rows)
			}
		}},
		{"host", "host-a", func(t *testing.T, v plugin.View) {
			if v.Title != "host-a" || v.Blocks[0].Items[1].Value != "1234567" {
				t.Fatal(v.Title, v.Blocks[0])
			}
			assertText(t, v, "12 min ago", "Pending sync", "Disk low", "Needs attention", "+4 −1", "Refused", "Broken link", "fleet on the host", "fleet sync", "Force requests", "Queued")
			if stat := v.Blocks[0].Items[3]; stat.Label != "Match fleet" || stat.Value != "12" || stat.Hint != "files" {
				t.Fatal(stat)
			}
			if got := v.Blocks[1].Text; got != "1 file changed on the host and has a newer version waiting. Fleet won't overwrite it. Review the file on the host and keep the hand edit or restore the fleet version." {
				t.Fatal(got)
			}
			providers := v.Blocks[5]
			if providers.Title != "Providers" || len(providers.Rows) != 2 || providers.Rows[0]["endpoint"] != "https://provider.example.lan:8443/api%2Fv1" || providers.Rows[1]["endpoint"] != "Not a URL" {
				t.Fatal(providers)
			}
		}},
		{"host", "host-b", func(t *testing.T, v plugin.View) {
			if len(v.Blocks) != 1 || v.Blocks[0].Tone != "warn" || v.Blocks[0].Text != "Stale · host-b hasn't reported yet. Run fleet sync on it." {
				t.Fatal(v)
			}
		}},
		{"host", "host-missing", func(t *testing.T, v plugin.View) {
			if len(v.Blocks) != 1 || v.Blocks[0].Text != "No host named host-missing reports to this hub." {
				t.Fatal(v)
			}
		}},
		{"skills", "", func(t *testing.T, v plugin.View) {
			show, sources, rows := v.Blocks[0], v.Blocks[1], v.Blocks[2]
			if len(show.Items) != 4 || len(show.Items[0].Badges) != 1 || sources.Items[2].Link != nil {
				t.Fatal(show, sources)
			}
			assertText(t, sources, "2 skills · 1 model-invoked", "Hand-written", "On trial until 10 Dec", "Own skills")
			assertText(t, rows, "Threads (30 days)", "Source lists", "Decision, 20 Nov", "SKILL.md", "Until 10 Dec")
			if rows.Rows[0]["hosts"] != "host-a 5" || rows.Rows[1]["threads"] != 0 || rows.Rows[1]["hosts"] != "—" || rows.Rows[2]["threads"] != "No data" || rows.Rows[2]["source"] != "Own" {
				t.Fatal(rows.Rows)
			}
		}},
		{"trials", "", func(t *testing.T, v plugin.View) {
			if len(v.Blocks) != 2 || v.Blocks[0].Title != "example-skills · ends 10 Dec" {
				t.Fatal(v)
			}
			assertText(t, v.Blocks[0], "10 Nov", "in 9 days", "All hosts", "Keep model-invoked", "Yes")
			assertText(t, v.Blocks[1], "3 days ago", "Past end date", "This trial ended on 28 Nov with 1 undecided skill. Decide each one, then end the trial.", "Drop", "No")
		}},
	} {
		t.Run(tc.name+tc.host, func(t *testing.T) {
			before := h.calls.Load()
			params := url.Values{}
			if tc.host != "" {
				params.Set("name", tc.host)
			}
			tc.check(t, h.view(t, tc.name, params))
			if h.calls.Load() != before+1 {
				t.Fatal("view should read exactly once")
			}
		})
	}
}

func TestHostDetailWithSeveralRefusedFilesAndUnknownMatchCount(t *testing.T) {
	h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
		body := string(fixture(t, "host-a"))
		body = strings.Replace(body, `"refused": 1`, `"refused": 2`, 1)
		body = strings.Replace(body, `"ok_count": 12`, `"ok_count": null`, 1)
		w.Write([]byte(body))
	})
	v := h.view(t, "host", url.Values{"name": {"host-a"}})
	if stat := v.Blocks[0].Items[3]; stat.Label != "Match fleet" || stat.Value != "Not reported" {
		t.Fatal(stat)
	}
	if got := v.Blocks[1].Text; got != "2 files changed on the host and have a newer version waiting. Fleet won't overwrite them. Review each file on the host and keep the hand edit or restore the fleet version." {
		t.Fatal(got)
	}
}

func TestTruncatedHostLists(t *testing.T) {
	h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/v1/")
		body := strings.Replace(string(fixture(t, name)), `"hosts": [`, `"truncated": true, "hosts": [`, 1)
		w.Write([]byte(body))
	})
	for _, name := range []string{"overview", "hosts"} {
		v := h.view(t, name, nil)
		if b := v.Blocks[0]; b.Type != "notice" || b.Tone != "info" || b.Text != "The hub sent only the first 3 hosts." {
			t.Fatal(b)
		}
	}
	alerts, err := h.Alerts(t.Context())
	if err == nil || err.Error() != "The hub's host list was cut short; alerts need the full list." || alerts != nil {
		t.Fatal(alerts, err)
	}
}

func TestFirstReportGracePeriod(t *testing.T) {
	for _, tc := range []struct {
		name, profileSince, hint, status, tone string
		stale                                  bool
	}{
		{"within grace", "2026-11-30T09:15:00Z", "1 stale · 1 waiting for first report", "Waiting for first report", "muted", false},
		{"at threshold", "2026-11-28T09:15:00Z", "1 stale · 1 waiting for first report", "Waiting for first report", "muted", false},
		{"past threshold", "2026-11-28T09:14:59Z", "2 stale", "Stale", "warn", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
				name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/"), "hosts/")
				body := strings.Replace(string(fixture(t, name)), `"name": "host-b",`, fmt.Sprintf(`"name": "host-b", "profile_since": %q,`, tc.profileSince), 1)
				w.Write([]byte(body))
			})
			overview := h.view(t, "overview", nil)
			if got := overview.Blocks[0].Items[0].Hint; got != tc.hint {
				t.Fatal(got)
			}
			host := overview.Blocks[1].Items[1]
			if host.Tone != tc.tone {
				t.Fatal(host)
			}
			assertText(t, host, tc.status)
			if got := h.view(t, "hosts", nil).Blocks[1].Rows[1]["status"]; got != tc.status {
				t.Fatal(got)
			}
			detail := h.view(t, "host", url.Values{"name": {"host-b"}}).Blocks[0]
			if detail.Tone != tc.tone || !strings.HasPrefix(detail.Text, tc.status) {
				t.Fatal(detail)
			}
			alerts, err := h.Alerts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			staleAlert := slices.ContainsFunc(alerts, func(a plugin.Alert) bool {
				return a.Rule == "host_stale" && a.Subject == "host-b"
			})
			if staleAlert != tc.stale {
				t.Fatal(alerts)
			}
		})
	}
}

func TestOverviewStatsWithPartialCountsAndPolicyWindow(t *testing.T) {
	for _, due := range []bool{false, true} {
		t.Run(fmt.Sprint(due), func(t *testing.T) {
			h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
				body := string(fixture(t, "overview"))
				body = strings.ReplaceAll(body, `"trial_due_soon_days": 7`, `"trial_due_soon_days": 10`)
				body = strings.ReplaceAll(body, `"drift": 2`, `"drift": 0`)
				body = strings.ReplaceAll(body, `"refused": 1`, `"refused": 0`)
				body = strings.ReplaceAll(body, `"broken_links": 1`, `"broken_links": 0`)
				if !due {
					body = strings.ReplaceAll(body, `"state": "due"`, `"state": "running"`)
				}
				w.Write([]byte(body))
			})
			stats := h.view(t, "overview", nil).Blocks[0].Items
			if att := stats[1]; att.Value != "0" || att.Hint != "from 2 of 3 hosts" || att.Tone != "" {
				t.Fatal(att)
			}
			wantHint, wantTone := "1 end within 10 days", ""
			if due {
				wantHint, wantTone = "1 past end date · "+wantHint, "warn"
			}
			if trial := stats[2]; trial.Hint != wantHint || trial.Tone != wantTone {
				t.Fatal(trial)
			}
		})
	}
}

func TestSkillsFiltersUseOneUnfilteredResponse(t *testing.T) {
	h := fixtureHub(t)
	v := h.view(t, "skills", url.Values{"source": {"example-skills"}, "mode": {"user"}})
	if h.calls.Load() != 1 {
		t.Fatal("extra reads")
	}
	if v.Blocks[1].Items[0].Meta != "2 skills · 1 model-invoked" || v.Blocks[1].Items[0].Badges[0].Text != "Showing" || v.Blocks[0].Items[2].Badges[0].Text != "Showing" {
		t.Fatal(v)
	}
	for _, i := range v.Blocks[0].Items {
		if i.Link.Params["source"] != "example-skills" {
			t.Fatal(i)
		}
	}
	if v.Blocks[1].Items[1].Link.Params["mode"] != "user" || len(v.Blocks[2].Rows) != 1 || v.Blocks[2].Rows[0]["skill"] != "sort-notes" {
		t.Fatal(v)
	}
	empty := h.view(t, "skills", url.Values{"source": {"another-source"}})
	if len(empty.Blocks[2].Rows) != 0 || empty.Blocks[2].Empty != "No skills match this filter" {
		t.Fatal(empty)
	}
}
func TestCompactHostUsage(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"rank and limit", `{"usage":{"by_host":[{"host":"host-d","threads":2},{"host":"host-c","threads":2},{"host":"host-b","threads":5},{"host":"host-a","threads":3},{"host":"host-e","threads":0}]},"hosts":[{"host":"host-f","installed":false}]}`, "host-b 5 · host-a 3 · host-c 2 · +1 more"},
		{"all absent", `{"hosts":[{"host":"host-b","installed":false}],"usage":null}`, "Not installed on host-b"},
		{"zero", `{"hosts":[{"host":"host-b","installed":false},{"host":"host-a","installed":true}],"usage":{"threads":0,"by_host":[]}}`, "—"},
		{"missing", `{"usage":null}`, "No data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s skill
			if err := json.Unmarshal([]byte(tc.body), &s); err != nil {
				t.Fatal(err)
			}
			if got := new(Fleet).skillHosts(s); got != tc.want {
				t.Fatal(got)
			}
		})
	}
}
func TestStatusPrecedence(t *testing.T) {
	var data hostsResponse
	if err := json.Unmarshal(fixture(t, "hosts"), &data); err != nil {
		t.Fatal(err)
	}
	head, older := "head", "older"
	e := data.hostEnvelope
	e.Head = head
	for _, tc := range []struct {
		seen       bool
		age        time.Duration
		free       int64
		counts     *hostCounts
		commit     *string
		word, tone string
	}{
		{false, 0, 1, nil, nil, "Stale", "warn"},
		{true, 4 * 24 * time.Hour, 1, &hostCounts{Refused: 1}, &older, "Stale", "warn"},
		{true, time.Minute, 7, &hostCounts{Drift: 1}, &older, "Disk critical", "crit"},
		{true, time.Minute, 8, nil, &older, "Attention", "warn"},
		{true, time.Minute, 15, &hostCounts{Broken: 1}, &older, "Attention", "warn"},
		{true, time.Minute, 15, &hostCounts{}, &older, "Pending sync", "info"},
		{true, 3 * 24 * time.Hour, 15, &hostCounts{}, &head, "OK", "ok"},
	} {
		t.Run(tc.word, func(t *testing.T) {
			h := hostSummary{Counts: tc.counts, Commit: tc.commit, Disk: &disk{Total: 100, Free: tc.free}}
			if tc.seen {
				v := e.Generated.Add(-tc.age)
				h.Seen = &v
			}
			if got := h.status(e); got != badge(tc.word, tc.tone) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestUntrustedTextInViews(t *testing.T) {
	long := strings.Repeat("界", 410)
	text := "<new>\u0001 " + readToken + " " + operatorToken + " " + long
	quoted, _ := json.Marshal(text)
	h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/v1/")
		name = strings.TrimPrefix(name, "hosts/")
		body := string(fixture(t, name))
		for _, field := range []string{"Set the mode for lint-docs", "done", "0.7.0", "queued", "skills/lint-docs/SKILL.md", "lint-docs", "model", "example-skills"} {
			body = strings.ReplaceAll(body, fmt.Sprintf("%q", field), string(quoted))
		}
		w.Write([]byte(body))
	})
	for _, name := range []string{"overview", "hosts", "host", "skills", "trials"} {
		v := h.view(t, name, url.Values{"name": {"host-a"}})
		assertText(t, v, "new [redacted] [redacted]", "…")
		raw, _ := json.Marshal(v)
		if strings.Contains(string(raw), `\u0001`) {
			t.Fatal("control character in view")
		}
		if name == "overview" {
			r := v.Blocks[len(v.Blocks)-1].Items[0]
			if len([]rune(r.Title)) != 400 || len([]rune(r.Badges[0].Text)) != 200 || r.Badges[0].Tone != "muted" {
				t.Fatal("unexpected size or tone")
			}
		}
	}
}
func TestLargeViews(t *testing.T) {
	read := func(name string) map[string]any {
		var data map[string]any
		if err := json.Unmarshal(fixture(t, name), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	trials, skills := read("trials"), read("skills")
	trial := trials["trials"].([]any)[1]
	skill := skills["skills"].([]any)[0]
	trials["trials"] = slices.Repeat([]any{trial}, 30)
	skills["skills"] = slices.Repeat([]any{skill}, 2000)
	skills["truncated"] = true
	h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/trials" {
			json.NewEncoder(w).Encode(trials)
		} else {
			json.NewEncoder(w).Encode(skills)
		}
	})
	v := h.view(t, "trials", nil)
	if len(v.Blocks) != 13 || v.Blocks[12].Type != "notice" || !strings.Contains(v.Blocks[12].Text, "trials were cut") {
		t.Fatal("incorrect trial limit")
	}
	v = h.view(t, "skills", nil)
	n := 0
	for _, b := range v.Blocks {
		n += len(b.Rows)
	}
	if n != 2000 || v.Blocks[2].Text != "The hub returned the first 2000 skills." {
		t.Fatal("missing skills or cap notice")
	}
	trials["trials"] = []any{}
	v = h.view(t, "trials", nil)
	if len(v.Blocks) != 1 || v.Blocks[0].Text != "No trials are running." {
		t.Fatal(v)
	}
}
