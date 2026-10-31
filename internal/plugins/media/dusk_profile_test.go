package media

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func TestDuskPlaybackProfileEnrichment(t *testing.T) {
	historyCalls, metadataCalls := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v2" || r.URL.Query().Get("apikey") != "synthetic-secret" {
			t.Error("invalid upstream request")
		}
		var data string
		switch r.URL.Query().Get("cmd") {
		case "get_users":
			data = `[{"user_id":"17","friendly_name":"Household viewer","username":"private-account","email":"private@example.org","token":"secret"}]`
		case "get_history":
			historyCalls++
			if r.URL.Query().Get("user_id") != "17" || r.URL.Query().Get("length") != "100" {
				t.Error("history not bounded to selected account")
			}
			data = `{"data":[{"user_id":17,"media_type":"episode","rating_key":"51","grandparent_rating_key":"50"},{"user_id":17,"media_type":"episode","rating_key":"52","grandparent_rating_key":"50"},{"user_id":17,"media_type":"movie","rating_key":"60"}]}`
		case "get_metadata":
			metadataCalls++
			if r.URL.Query().Get("rating_key") == "50" {
				data = `{"media_type":"show","title":"Synthetic series","year":"2020","guids":["tmdb://42"],"file":"/private/file"}`
			} else {
				data = `{"media_type":"movie","title":"Unmatched film","guids":["local://60"]}`
			}
		default:
			t.Error("unexpected upstream operation")
			w.WriteHeader(400)
			return
		}
		fmt.Fprintf(w, `{"response":{"result":"success","data":%s}}`, data)
	}))
	defer upstream.Close()
	m := New(&testHost{now: time.Now(), client: upstream.Client()}).(*Media)
	m.Configure(plugin.Config{"tautulli_url": upstream.URL, "tautulli_key": "synthetic-secret"})
	choices, err := m.TVProfiles(context.Background())
	if err != nil || len(choices) != 1 || choices[0].Name != "Household viewer" {
		t.Fatalf("choices %v %v", choices, err)
	}
	if _, err := m.TVHistory(context.Background(), 99); err == nil {
		t.Fatal("unconfigured profile accepted")
	}
	if historyCalls != 0 {
		t.Fatal("unknown profile history requested")
	}
	titles, err := m.TVHistory(context.Background(), 17)
	if err != nil || len(titles) != 1 || titles[0].Kind != "tv" || titles[0].ID != 42 {
		t.Fatalf("titles %v %v", titles, err)
	}
	if historyCalls != 1 || metadataCalls != 2 {
		t.Fatalf("unexpected request fanout %d %d", historyCalls, metadataCalls)
	}
	b, _ := json.Marshal(titles)
	for _, private := range []string{"private", "Household viewer", "synthetic-secret"} {
		if strings.Contains(string(b), private) {
			t.Fatal("private playback metadata escaped")
		}
	}
}
