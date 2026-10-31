package media

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func TestDuskValidatePolicyFreshPermissionsAndSelections(t *testing.T) {
	var permissions atomic.Int32
	var missing atomic.Bool
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			posts.Add(1)
			t.Error("policy validation attempted write")
		}
		switch r.URL.Path {
		case "/api/v1/user":
			fmt.Fprint(w, `{"results":[{"id":12,"displayName":"Synthetic requester"}]}`)
		case "/api/v1/user/12":
			if missing.Load() {
				fmt.Fprint(w, `{"id":12}`)
			} else {
				fmt.Fprintf(w, `{"id":12,"permissions":%d}`, permissions.Load())
			}
		case "/api/v1/settings/radarr", "/api/v1/settings/sonarr":
			fmt.Fprint(w, `[{"id":4,"name":"Synthetic server"}]`)
		case "/api/v1/settings/radarr/4", "/api/v1/settings/sonarr/4":
			fmt.Fprint(w, `{"profiles":[{"id":6,"name":"Standard"}],"rootFolders":[{"path":"/synthetic/library"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	m := New(&testHost{now: time.Now(), client: upstream.Client()}).(*Media)
	if err := m.Configure(plugin.Config{"overseerr_url": upstream.URL, "overseerr_key": "synthetic-secret"}); err != nil {
		t.Fatal(err)
	}
	selection := mediadata.Selection{ServerID: 4, ProfileID: 6, Root: "/synthetic/library"}
	policy := mediadata.RequestPolicy{RequesterID: 12, Approval: "manual", Movie: selection}
	permissions.Store(8)
	if err := m.TVValidatePolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	permissions.Store(64)
	if err := m.TVValidatePolicy(context.Background(), policy); err == nil {
		t.Fatal("fresh automatic permission accepted manual policy")
	}
	policy.Approval = "automatic"
	if err := m.TVValidatePolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	policy.TV = selection
	if err := m.TVValidatePolicy(context.Background(), policy); err == nil {
		t.Fatal("movie-only approval accepted for selected TV destination")
	}
	permissions.Store(64 | 128)
	if err := m.TVValidatePolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	permissions.Store(2)
	if err := m.TVValidatePolicy(context.Background(), policy); err != nil {
		t.Fatal("administrator permission should auto approve both kinds")
	}
	missing.Store(true)
	if err := m.TVValidatePolicy(context.Background(), policy); err == nil {
		t.Fatal("missing permissions accepted")
	}
	missing.Store(false)
	bad := policy
	bad.Movie.ProfileID = 999
	if err := m.TVValidatePolicy(context.Background(), bad); err == nil {
		t.Fatal("unknown profile accepted")
	}
	bad = policy
	bad.TV.Root = "/unconfigured"
	if err := m.TVValidatePolicy(context.Background(), bad); err == nil {
		t.Fatal("unknown root accepted")
	}
	bad = policy
	bad.RequesterID = 99
	if err := m.TVValidatePolicy(context.Background(), bad); err == nil {
		t.Fatal("unknown requester accepted")
	}
	bad = policy
	bad.Movie = mediadata.Selection{}
	bad.TV = mediadata.Selection{}
	if err := m.TVValidatePolicy(context.Background(), bad); err == nil {
		t.Fatal("empty destinations accepted")
	}
	if posts.Load() != 0 {
		t.Fatal("validation caused upstream mutation")
	}
}
