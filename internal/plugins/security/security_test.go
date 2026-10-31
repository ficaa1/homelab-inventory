package security

import (
	"context"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testHost struct{ dir string }

func (h testHost) HTTP() *http.Client { return http.DefaultClient }
func (h testHost) CacheDir() string   { return h.dir }
func (testHost) Logf(string, ...any)  {}
func (testHost) Now() time.Time       { return time.Now() }
func TestCanaryDurableDedupedAndRealCredentialExcluded(t *testing.T) {
	dir := t.TempDir()
	p := New(testHost{dir}).(*Integration)
	fake := strings.Repeat("f", 32)
	if e := p.Configure(plugin.Config{"tokens": "backup=" + fake, "paths": "bait=/vault-bait", "window_hours": 24.0}); e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("GET", "http://example.invalid/private?credential=never-record", nil)
	r.Header.Set("Authorization", "Bearer "+fake)
	p.ExcludeCredentials(fake)
	if p.Observe(r) {
		t.Fatal("real credential intercepted")
	}
	p.ExcludeCredentials()
	if !p.Observe(r) || !p.Observe(r) {
		t.Fatal("canary missed")
	}
	b, e := os.ReadFile(filepath.Join(dir, "canary-trips.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(string(b), "\n") != 1 || strings.Contains(string(b), fake) || strings.Contains(string(b), "credential") || strings.Contains(string(b), "/private") {
		t.Fatal("duplicate trip or secret path recorded")
	}
	alerts, e := p.Alerts(context.Background())
	if e != nil || len(alerts) != 1 {
		t.Fatal("alert absent")
	}
	reopened := New(testHost{dir}).(*Integration)
	reopened.Configure(plugin.Config{"paths": "bait=/vault-bait"})
	v, e := reopened.View(context.Background(), "trips", nil)
	if e != nil || len(v.Blocks[0].Rows) != 1 {
		t.Fatal("durable trip missing")
	}
}

func TestCanaryCooldownMapIsBounded(t *testing.T) {
	p := New(testHost{t.TempDir()}).(*Integration)
	p.Configure(plugin.Config{"paths": "bait=/bait"})
	for i := 0; i < 4096; i++ {
		p.last[fmt.Sprint(i)] = time.Now()
	}
	r := httptest.NewRequest("GET", "http://example.invalid/bait", nil)
	r.RemoteAddr = "[2001:db8::99]:1234"
	if !p.Observe(r) || len(p.last) > 4096 {
		t.Fatal("unbounded source cooldown map")
	}
}
