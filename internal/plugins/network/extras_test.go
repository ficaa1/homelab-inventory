package network

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

type extraHost struct{ client *http.Client }

func (h extraHost) HTTP() *http.Client { return h.client }
func (extraHost) CacheDir() string     { return "" }
func (extraHost) Logf(string, ...any)  {}
func (extraHost) Now() time.Time       { return time.Now() }
func TestSeedingOnlyFixedReads(t *testing.T) {
	calls := []string{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/v2/auth/login":
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), "password=synthetic") {
				t.Error("missing login")
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture", Path: "/"})
			w.Write([]byte("Ok."))
		case "/api/v2/transfer/info":
			if _, e := r.Cookie("SID"); e != nil {
				t.Error("cookie not kept")
			}
			w.Write([]byte(`{"dl_info_speed":5,"up_info_speed":10,"connection_status":"connected","dht_nodes":3}`))
		case "/api/v2/torrents/info":
			if r.URL.RawQuery != "filter=seeding" {
				t.Error("read not seeding-filtered")
			}
			w.Write([]byte(`[{"name":"Example","size":1000,"seeding_time":3600,"ratio":1.2,"upspeed":10,"state":"uploading","tracker":"must-not-leak","save_path":"must-not-leak"}]`))
		default:
			t.Error("unexpected upstream route")
		}
	}))
	defer s.Close()
	n := New(extraHost{s.Client()}).(*Network)
	if e := n.Configure(plugin.Config{"qbit_url": s.URL, "qbit_user": "example", "qbit_password": "synthetic", "seed_hours": 2.0}); e != nil {
		t.Fatal(e)
	}
	v, e := n.View(context.Background(), "seeding", nil)
	if e != nil {
		t.Fatal(e)
	}
	b, e := plugin.ValidateView(v, n.Manifest())
	if e != nil || strings.Contains(string(b), "must-not-leak") || len(calls) != 3 {
		t.Fatalf("unsafe projection %v", e)
	}
}
