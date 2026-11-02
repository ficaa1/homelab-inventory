package archivetier

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestQueueDeleteClientAllowlist(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer srv.Close()
	c := NewClient("radarr", srv.URL, "key", true)
	for _, tc := range []struct {
		path, query string
		allowed     bool
	}{
		{"/queue/1", "removeFromClient=true&blocklist=true&skipRedownload=true", true},
		{"/queue/1", "removeFromClient=true&blocklist=true", false}, {"/queue/1", "removeFromClient=true&blocklist=true&skipRedownload=false", false},
		{"/queue/01", "removeFromClient=true&blocklist=true&skipRedownload=true", false}, {"/queue/0", "removeFromClient=true&blocklist=true&skipRedownload=true", false},
		{"/queue/-1", "removeFromClient=true&blocklist=true&skipRedownload=true", false}, {"/queue/bulk", "removeFromClient=true&blocklist=true&skipRedownload=true", false},
		{"/queue/1/extra", "removeFromClient=true&blocklist=true&skipRedownload=true", false}, {"/episodefile/1", "removeFromClient=true&blocklist=true&skipRedownload=true", false},
		{"/queue/1", "removeFromClient=true", false}, {"/queue/1", "removeFromClient=false&blocklist=true", false},
		{"/queue/1", "removeFromClient=true&blocklist=false", false}, {"/queue/1", "removeFromClient=true&blocklist=true&skipRedownload=true&skipRedownload=true", false},
		{"/queue/1", "removeFromClient=true&removeFromClient=true&blocklist=true&skipRedownload=true", false},
	} {
		q, _ := url.ParseQuery(tc.query)
		err := c.call(context.Background(), http.MethodDelete, tc.path, q, nil, nil)
		if (err == nil) != tc.allowed {
			t.Errorf("%s?%s: %v", tc.path, tc.query, err)
		}
	}
	c.Apply = false
	if err := c.call(context.Background(), http.MethodDelete, "/queue/1", url.Values{"removeFromClient": {"true"}, "blocklist": {"true"}, "skipRedownload": {"true"}}, nil, nil); err == nil || calls != 1 {
		t.Fatal("dry-run delete escaped")
	}
}

func TestAbandonedCleanupDeadlineAndIdentity(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, attack := range []string{"none", "wrong download", "wrong id", "not completed", "absent", "lost response", "dry-run", "no-search", "blocked", "failed title", "other failed title"} {
			t.Run(kind+"/"+attack, func(t *testing.T) {
				deletes, reads := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					switch req.URL.Path {
					case "/api/v3/system/status":
						app := "Radarr"
						if kind == "sonarr" {
							app = "Sonarr"
						}
						fmt.Fprintf(w, `{"appName":%q,"version":%q}`, app, DefaultConfig().ReviewedVersions[kind])
					case "/api/v3/queue":
						reads++
						id, download, state := 41, "bad-hash", "importBlocked"
						if reads > 1 {
							if attack == "wrong download" {
								download = "innocent"
							}
							if attack == "wrong id" {
								id = 42
							}
							if attack == "not completed" {
								state = "downloading"
							}
						}
						rows := []json.RawMessage{}
						if attack != "absent" && deletes == 0 {
							b, _ := json.Marshal(QueueItem{ID: id, DownloadID: download, MovieID: 7, SeriesID: 7, State: state})
							rows = append(rows, b)
						}
						w.Write(pagedBody(t, req, rows))
					case "/api/v3/queue/41":
						if req.Method != http.MethodDelete || req.URL.RawQuery != "blocklist=true&removeFromClient=true&skipRedownload=true" {
							t.Error("incorrect delete")
						}
						deletes++
						if attack == "lost response" {
							w.WriteHeader(500)
						} else {
							w.WriteHeader(204)
						}
					default:
						t.Errorf("unexpected request %s", req.URL)
						w.WriteHeader(404)
					}
				}))
				defer srv.Close()
				r, _, _, now := runner(t)
				c := NewClient(kind, srv.URL, "key", true)
				r.Arr[kind] = c
				st := State{AbandonedDownloads: []AbandonedDownload{{Kind: kind, DownloadID: "bad-hash", CompletedAt: now, RemoveAfter: now.Add(abandonedRetention)}}}
				adm := Admission{NewWrites: true}
				if attack == "failed title" || attack == "other failed title" {
					id := "bad-hash"
					if attack == "other failed title" {
						id = "other"
					}
					st.FailedJobs = []*Job{{Target: Target{Kind: kind, ID: 9}, Phase: Failed, DownloadID: id}}
				}
				r.cleanupAbandoned(context.Background(), now.Add(abandonedRetention-time.Nanosecond), &st, adm)
				if deletes != 0 || reads != 0 || len(st.AbandonedDownloads) != 1 {
					t.Fatal("cleanup before nine days")
				}
				switch attack {
				case "dry-run":
					r.DryRun = true
				case "no-search":
					r.NoSearch = true
				case "blocked":
					adm.NewWrites = false
				}
				r.cleanupAbandoned(context.Background(), now.Add(abandonedRetention), &st, adm)
				wantDelete := attack == "none" || attack == "lost response" || attack == "other failed title"
				if (deletes == 1) != wantDelete {
					t.Fatalf("deletes=%d error=%s", deletes, st.CleanupError)
				}
				gone := attack == "none" || attack == "absent" || attack == "other failed title"
				if (len(st.AbandonedDownloads) == 0) != gone {
					t.Fatalf("cleanup lost tracking: %+v", st)
				}
				if attack == "lost response" {
					r.cleanupAbandoned(context.Background(), now.Add(abandonedRetention+time.Hour), &st, adm)
					if deletes != 1 || len(st.AbandonedDownloads) != 0 {
						t.Fatal("ambiguous delete replayed")
					}
				}
			})
		}
	}
}
