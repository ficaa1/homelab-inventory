package archivetier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type tagArr struct {
	*fakeArr
	removals   int
	removalErr error
}

func (a *tagArr) RemoveArchiveTag(context.Context, Target) error { a.removals++; return a.removalErr }

func TestTagRemovalRetriesAndSeasonCompletion(t *testing.T) {
	for _, scenario := range []string{"movie", "movie active", "other eligible season", "other pending season", "other active season", "series done", "ineligible season", "failure", "dry run", "admission closed"} {
		t.Run(scenario, func(t *testing.T) {
			r, a, _, now := runner(t)
			ta := &tagArr{fakeArr: a}
			r.Arr["sonarr"] = ta
			target := target()
			st := State{TagRemovals: map[string]Target{target.Key(): target}, CompletedFiles: map[string]string{targetKey(target): filesKey(target)}}
			switch scenario {
			case "movie", "movie active":
				target = movieTarget()
				st.TagRemovals = map[string]Target{target.Key(): target}
				r.Arr["radarr"] = ta
				if scenario == "movie active" {
					st.Job = &Job{Target: target, Phase: Downloading}
				}
			case "other eligible season", "ineligible season":
				other := target
				other.Season = 4
				if scenario == "ineligible season" {
					other.Files = nil
				}
				a.targets = append(a.targets, other)
			case "other pending season":
				st.Approvals = []Approval{{ApprovalRequest: ApprovalRequest{Kind: "sonarr", ID: target.ID, Season: 4}}}
			case "other active season":
				other := target
				other.Season = 4
				st.Job = &Job{Target: other, Phase: Downloading}
			case "failure":
				ta.removalErr = errors.New("fixture failure")
			case "dry run":
				r.DryRun = true
			}
			adm := Admission{NewWrites: scenario != "admission closed"}
			r.retryTagRemovals(context.Background(), now, &st, adm)
			want := scenario == "movie" || scenario == "series done" || scenario == "ineligible season" || scenario == "failure"
			if (ta.removals > 0) != want {
				t.Fatalf("removals: %d", ta.removals)
			}
			if scenario == "failure" {
				if len(st.TagRemovals) != 1 || st.TagRemovalError == "" || st.Error != "" {
					t.Fatal("tag removal failure poisoned job or lost retry")
				}
				ta.removalErr = nil
				r.retryTagRemovals(context.Background(), now, &st, adm)
				if ta.removals != 2 || len(st.TagRemovals) != 0 || st.TagRemovalError != "" {
					t.Fatal("did not retry")
				}
			} else if want && len(st.TagRemovals) != 0 {
				t.Fatal("successful removal remains queued")
			}
		})
	}
}

func TestSuccessfulJobQueuesTagRemovalWithoutFailing(t *testing.T) {
	r, a, _, now := runner(t)
	ta := &tagArr{fakeArr: a, removalErr: errors.New("unavailable")}
	r.Arr["sonarr"] = ta
	st := check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, Title: st.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	check(t, r, now)
	a.current = target()
	for i := range a.current.Files {
		a.current.Files[i].ID += 100
		a.current.Files[i].Size = 2 * GB
		a.current.Files[i].Quality = qual(1080)
		a.current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
		a.current.Episodes[i].FileID = a.current.Files[i].ID
	}
	a.targets = []Target{a.current}
	st = check(t, r, now)
	if st.Job.Phase != Complete || ta.removals != 1 || st.Error != "" || st.TagRemovalError == "" || len(st.TagRemovals) != 1 {
		t.Fatal("completion lost on tag failure")
	}
	ta.removalErr = nil
	st = check(t, r, now)
	if ta.removals != 2 || len(st.TagRemovals) != 0 || a.grabs != 1 {
		t.Fatal("next cycle did not retry safely")
	}
}

func TestRemoveArchiveTagEditorContract(t *testing.T) {
	for _, kind := range []string{"radarr", "sonarr"} {
		t.Run(kind, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v3/system/status":
					app := "Radarr"
					if kind == "sonarr" {
						app = "Sonarr"
					}
					json.NewEncoder(w).Encode(map[string]string{"appName": app, "version": DefaultConfig().ReviewedVersions[kind]})
				case "/api/v3/tag":
					w.Write([]byte(`[{"id":3,"label":"archive"},{"id":4,"label":"4k"}]`))
				default:
					writes++
					path, ids := "/api/v3/movie/editor", "movieIds"
					if kind == "sonarr" {
						path, ids = "/api/v3/series/editor", "seriesIds"
					}
					var body map[string]json.RawMessage
					json.NewDecoder(r.Body).Decode(&body)
					if r.Method != "PUT" || r.URL.Path != path || len(body) != 3 || string(body[ids]) != "[2]" || string(body["tags"]) != "[3]" || string(body["applyTags"]) != `"remove"` {
						t.Error("wrong editor request")
					}
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer server.Close()
			c := NewClient(kind, server.URL, "fixture", true)
			target := Target{Kind: kind, ID: 2}
			if err := c.RemoveArchiveTag(context.Background(), target); err != nil || writes != 1 {
				t.Fatal("removal failed")
			}
			c.Apply = false
			if err := c.RemoveArchiveTag(context.Background(), target); err == nil || writes != 1 {
				t.Fatal("dry-run wrote")
			}
			if err := c.call(context.Background(), http.MethodPut, "/movie/editor/extra", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatal("broad editor allowance")
			}
		})
	}
}
