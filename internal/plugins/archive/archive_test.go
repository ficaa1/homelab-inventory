package archive

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type host struct{ client *http.Client }

func (h host) HTTP() *http.Client { return h.client }
func (host) CacheDir() string     { return "" }
func (host) Logf(string, ...any)  {}
func (host) Now() time.Time       { return time.Now() }
func TestFixedRouteNoRetryAndRedaction(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/seeding/move" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Errorf("unexpected request")
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"planHash":"reviewed"`) {
			t.Error("missing reviewed plan")
		}
		w.WriteHeader(503)
	}))
	defer s.Close()
	p := New(host{s.Client()}).(*Integration)
	if e := p.Configure(plugin.Config{"policy_url": s.URL, "policy_token": "synthetic-key"}); e != nil {
		t.Fatal(e)
	}
	_, e := p.Action(context.Background(), "seed-move", plugin.Config{"hash": "abcdef", "plan_hash": "reviewed"})
	if e == nil || calls != 1 {
		t.Fatalf("write repeated or failure hidden: %d %v", calls, e)
	}
	_, e = p.Action(context.Background(), "../../anything", plugin.Config{})
	if e != plugin.ErrNotFound || calls != 1 {
		t.Fatal("unknown action reached upstream")
	}
	v := p.render("fixture", map[string]any{"token": "synthetic-key", "job": map[string]any{"title": "synthetic-key <title>", "phase": "waiting"}})
	b, e := plugin.ValidateView(v, p.Manifest())
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "synthetic-key") {
		t.Fatal("secret exposed")
	}
}

func TestFixedActionRefusesRedirectReplay(t *testing.T) {
	replayed := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/archive/run" {
			w.Header().Set("Location", "/unexpected-write")
			w.WriteHeader(307)
			return
		}
		replayed++
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p := New(host{upstream.Client()}).(*Integration)
	p.Configure(plugin.Config{"policy_url": upstream.URL})
	if _, e := p.Action(context.Background(), "run", plugin.Config{}); e == nil || replayed != 0 {
		t.Fatal("fixed action followed redirect")
	}
}
