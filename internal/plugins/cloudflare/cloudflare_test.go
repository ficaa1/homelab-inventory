package cloudflare

import (
	"context"
	"encoding/json"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type testHost struct{ client *http.Client }

func (h testHost) HTTP() *http.Client { return h.client }
func (testHost) CacheDir() string     { return "" }
func (testHost) Logf(string, ...any)  {}
func (testHost) Now() time.Time       { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
func TestAnalyticsRealShapeAndGraphQLErrors(t *testing.T) {
	bad := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("unexpected request")
		}
		var in struct {
			Variables map[string]string `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Variables["since"] != "2026-10-30" || in.Variables["until"] != "2026-10-31" {
			t.Error("not complete day")
		}
		if bad {
			w.Write([]byte(`{"errors":[{"message":"private upstream error"}],"data":{"viewer":{"zones":[]}}}`))
			return
		}
		w.Write([]byte(`{"data":{"viewer":{"zones":[{"httpRequests1hGroups":[{"dimensions":{"datetime":"2026-10-30T01:00:00Z"},"sum":{"requests":10,"bytes":1000,"threats":1,"cachedRequests":7},"uniq":{"uniques":3}}]}]}}}`))
	}))
	defer s.Close()
	p := New(testHost{s.Client()}).(*Integration)
	p.Configure(plugin.Config{"api_url": s.URL, "api_token": "fixture", "zone_id": "0123456789abcdef0123456789abcdef"})
	v, e := p.View(context.Background(), "analytics", nil)
	if e != nil {
		t.Fatal(e)
	}
	if v.Blocks[1].Items[0].Value != "10" {
		t.Fatal("wrong requests")
	}
	if _, e = plugin.ValidateView(v, p.Manifest()); e != nil {
		t.Fatal(e)
	}
	bad = true
	if _, e = p.View(context.Background(), "analytics", nil); e == nil {
		t.Fatal("GraphQL failure became empty success")
	}
}
