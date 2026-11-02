package archivetier

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// bearPack pins the reported S05 pack: eight ranking-only rejections and an
// unrelated S00 special with Sonarr's unexpected/not-found rejections.
func bearPack(t *testing.T) (Target, []ManualItem) {
	t.Helper()
	target := target()
	target.Title, target.Season = "The Bear", 5
	target.Files, target.Episodes = nil, nil
	base := fixture[[]ManualItem](t, "manual-sonarr.json")[0]
	var items []ManualItem
	for n := 1; n <= 8; n++ {
		ep := Episode{ID: 100 + n, Season: 5, FileID: 10 + n, AirDate: "2025-01-01T00:00:00Z"}
		target.Episodes = append(target.Episodes, ep)
		target.Files = append(target.Files, File{ID: ep.FileID, Path: fmt.Sprintf("/library/The Bear/S05E%02d.mkv", n), Size: 10 * GB, Quality: qual(2160)})
		it := base
		it.Path = fmt.Sprintf("/downloads/The Bear S05/The Bear (2022) - S05E%02d - 1080p x265.mkv", n)
		it.Size, it.Episodes = GB, []Episode{ep}
		it.Rejections = []struct {
			Reason string `json:"reason"`
		}{{"Not an upgrade for existing episode file(s). Existing quality: WEBDL-2160p. New Quality WEBDL-1080p."}}
		items = append(items, it)
	}
	extra := base
	extra.Path = "/downloads/The Bear S05/The Bear (2022) - S00E01 - Gary.mkv"
	extra.Size, extra.Episodes = 100*GB, []Episode{{ID: 999, Season: 0}}
	extra.Rejections = []struct {
		Reason string `json:"reason"`
	}{
		{"Episode 0x01 was unexpected considering the The Bear S05 folder name"},
		{"Episode 0x01 was not found in the grabbed release"},
	}
	return target, append(items, extra)
}

func TestValidateImportSkipsExtraPackFiles(t *testing.T) {
	for _, scenario := range []string{"special", "no episodes", "unwanted same season", "wrong season", "unsafe extra", "wanted rejection", "missing wanted", "mixed mapping", "wanted duplicate", "wanted unsafe", "wanted size"} {
		t.Run(scenario, func(t *testing.T) {
			target, items := bearPack(t)
			wantErr := false
			switch scenario {
			case "no episodes":
				items[8].Episodes = nil
			case "unwanted same season":
				items[8].Episodes[0].Season = 5
			case "wrong season":
				items[8].Episodes[0].ID = target.Episodes[0].ID
			case "unsafe extra":
				items[8].Path, items[8].Size = "/outside/../special.mkv", -1
			case "wanted rejection":
				items[0].Rejections = items[8].Rejections
				wantErr = true
			case "missing wanted":
				items = items[1:]
				wantErr = true
			case "mixed mapping":
				items[0].Episodes = append(items[0].Episodes, items[8].Episodes[0])
				wantErr = true
			case "wanted duplicate":
				items = append(items, items[0])
				wantErr = true
			case "wanted unsafe":
				items[0].Path = "/outside/episode.mkv"
				wantErr = true
			case "wanted size":
				items[0].Size = 100 * GB
				wantErr = true
			}
			files, err := ValidateImport(items, target, testConfig(), Mode1080p)
			if scenario == "wanted rejection" && (err == nil || err.Error() != "download has non-ranking import rejection") {
				t.Fatalf("wanted rejection was not enforced: %v", err)
			}
			if (err != nil) != wantErr {
				t.Fatalf("unexpected validation result: %v", err)
			}
			if !wantErr {
				if len(files) != 8 {
					t.Fatalf("import has %d files, want 8", len(files))
				}
				for n, file := range files {
					if file["path"] != items[n].Path || len(file["episodeIds"].([]int)) != 1 || file["episodeIds"].([]int)[0] != target.Episodes[n].ID {
						t.Fatalf("wrong imported file: %+v", file)
					}
				}
			}
		})
	}
}

func TestRunnerSkipsExtraPackFiles(t *testing.T) {
	r, a, _, now := runner(t)
	st := check(t, r, now)
	target, items := bearPack(t)
	st.Job.Target, a.current, a.items = target, target, items
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	a.queue = []QueueItem{
		{ID: 1, SeriesID: target.ID, EpisodeID: 999, DownloadID: "hash", State: "failed", SizeLeft: GB},
		{ID: 2, SeriesID: target.ID, EpisodeID: 101, DownloadID: "hash", State: "importBlocked"},
	}
	probes := 0
	originalProbe := r.Probe
	r.Probe = func(ctx context.Context, f File) (*MediaInfo, error) {
		if strings.Contains(f.Path, "S00") {
			t.Fatal("extra file probed")
		}
		probes++
		return originalProbe(ctx, f)
	}
	st = check(t, r, now)
	if st.Job.Phase != NeedsApproval || probes != 0 {
		t.Fatal("extra files did not wait")
	}
	if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), true); err != nil {
		t.Fatal(err)
	}
	st = check(t, r, now)
	if a.imports != 1 || len(a.imported) != 8 || probes != 0 {
		t.Fatalf("wrong import or byte accounting: imports=%d files=%d probes=%d paused=%s", a.imports, len(a.imported), probes, st.Paused)
	}
}

func TestLifecycleIgnoresExtraPackEpisodes(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint(automatic), func(t *testing.T) {
			j, current, history := lifecycleCase(t, "sonarr")
			extra := clone(history[0])
			extra["id"], extra["episodeId"] = 999999, 999999
			extra["data"] = nil // Not a file we imported; no replacement proof needed.
			history = append(history, extra)
			queue, _ := json.Marshal(QueueItem{ID: 77, SeriesID: j.Target.ID, EpisodeID: 999999, DownloadID: j.DownloadID, State: "importBlocked"})
			srv := lifecycleServer(t, history, []json.RawMessage{queue})
			defer srv.Close()
			client := NewClient("sonarr", srv.URL, "fixture-key", false)
			if automatic {
				b, _ := json.Marshal(history[0])
				var h HistoryItem
				if err := json.Unmarshal(b, &h); err != nil {
					t.Fatal(err)
				}
				j.Started = h.Date.Add(-time.Hour)
				j.CommandID = 0
				if err := client.Lifecycle(context.Background(), j, current); err != nil || j.ImportedAt.IsZero() {
					t.Fatalf("extra episode blocked automatic proof: %v", err)
				}
			} else if err := client.Lifecycle(context.Background(), j, current); err != nil {
				t.Fatal(err)
			}
		})
	}
}
