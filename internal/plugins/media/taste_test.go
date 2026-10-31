package media

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func TestTasteTypedFactsAndOwnerHistory(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/movie/12":
			fmt.Fprint(w, `{"id":12,"title":"Synthetic film","releaseDate":"2020-02-03","posterPath":"/synthetic.jpg","voteAverage":8.1,"voteCount":400,"credits":{"crew":[{"id":90,"name":"Fixture Creator","job":"Director"}],"cast":[]},"keywords":[{"id":2,"name":"friendship"}],"genres":[{"id":3,"name":"Drama"}],"productionCountries":[{"iso_3166_1":"FR"}],"originalLanguage":"fr"}`)
		case "/api/v1/movie/12/recommendations":
			if r.URL.Query().Get("page") != "2" {
				t.Error("recommendation page lost")
			}
			fmt.Fprint(w, `{"results":[{"id":13,"title":"Synthetic neighbor","releaseDate":"2021-01-02","posterPath":"/neighbor.jpg","voteAverage":7.9,"voteCount":200}]}`)
		case "/api/v2":
			if r.URL.Query().Get("user_id") != "7" || r.URL.Query().Get("grouping") != "0" {
				t.Error("unbound history request")
			}
			fmt.Fprint(w, `{"response":{"result":"success","data":{"data":[{"user_id":7,"media_type":"episode","rating_key":42,"grandparent_rating_key":40,"grandparent_title":"Series","parent_media_index":2,"media_index":3,"percent_complete":99,"play_duration":1800,"date":1700000000},{"user_id":8,"media_type":"movie","rating_key":55,"title":"Another household","percent_complete":99}]}}}`)
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	m := New(&testHost{now: time.Now(), client: upstream.Client()}).(*Media)
	if e := m.Configure(plugin.Config{"overseerr_url": upstream.URL, "overseerr_key": "fixture", "tautulli_url": upstream.URL, "tautulli_key": "fixture"}); e != nil {
		t.Fatal(e)
	}
	facts, e := m.TitleFacts(context.Background(), "movie", 12)
	if e != nil || facts.ID != 12 || facts.Date != "2020-02-03" || len(facts.People) != 1 || facts.People[0].Role != "dir" || facts.Language != "fr" {
		t.Fatalf("facts %#v %v", facts, e)
	}
	rows, e := m.RecommendationsPage(context.Background(), "movie", 12, 2)
	if e != nil || len(rows) != 1 || rows[0].Votes != 200 || rows[0].Kind != "movie" {
		t.Fatal(rows, e)
	}
	history, e := m.WatchHistory(context.Background(), 7)
	if e != nil || len(history) != 1 || history[0].Season != 2 || history[0].Episode != 3 || history[0].Seconds != 1800 || history[0].Key != "40" {
		t.Fatal(history, e)
	}
	if _, e = m.RecommendationsPage(context.Background(), "movie", 12, 4); e == nil {
		t.Fatal("unbounded page accepted")
	}
}
