package archivetier

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestProbeFixtures(t *testing.T) {
	b, err := os.ReadFile("testdata/probe-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]json.RawMessage{}
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile("testdata/probe-surround.json")
	if err != nil {
		t.Fatal(err)
	}
	cases["surround"] = b
	for _, tc := range []struct{ name, mode, why string }{
		{"surround", Mode1080p, ""}, {"ddp51", Mode1080p, ""}, {"ac3-truehd", Mode1080p, ""},
		{"stereo", Mode1080p, "stereo audio"}, {"truehd", Mode1080p, "lossless audio"}, {"dtshd-core", Mode1080p, ""},
		{"pq", Mode2160p, ""}, {"dv5", Mode2160p, "not HDR10"}, {"sdr", Mode2160p, "not HDR10"}, {"dv8-pq", Mode2160p, ""},
		{"dtshd", Mode1080p, "lossless audio"}, {"flac", Mode1080p, "lossless audio"}, {"pcm", Mode1080p, "lossless audio"},
		{"vorbis", Mode1080p, "audio codec unknown"}, {"avc", Mode1080p, ""}, {"av1", Mode1080p, ""}, {"mpeg2", Mode1080p, "video codec unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseProbe(cases[tc.name])
			if err != nil {
				t.Fatal(err)
			}
			if got := mediaReject(File{MediaInfo: m}, tc.mode); got != tc.why {
				t.Fatalf("got %q, want %q: %+v", got, tc.why, m)
			}
		})
	}
	for _, b := range []string{`null`, `{}`, `{"streams":null}`, `{"streams":[]}`, `{"streams":[{"channels":"six"}]}`, `{"streams":[{}]}`, `{"streams":[{"codec_type":"audio","codec_name":"aac"}]}`} {
		if _, err := parseProbe([]byte(b)); err == nil {
			t.Fatalf("invalid probe accepted: %s", b)
		}
	}
}

func TestUnknownAudioSelection(t *testing.T) {
	c := testConfig()
	tg := selectionTarget("radarr")
	r := fixture[[]Release](t, "example-movie-releases.json")[0]
	r.Guid, r.Title, r.Quality, r.Size, r.Seeders, r.Rejections = "unknown", "Title 1080p BluRay x264-GROUP", qual(1080), 12*GB, 100, nil
	if why := Reject(r, tg, c); why != "" {
		t.Fatal(why)
	}
	known := r
	known.Guid, known.Title = "known", r.Title+" AAC5.1"
	x, y := score(r, tg, c), score(known, tg, c)
	if x.AudioPenalty != -5 || math.Abs(y.Total-x.Total-5) > .001 {
		t.Fatalf("scores %+v %+v", x, y)
	}
	d := Select(tg, []Release{r}, c)
	if d.Release == nil || !d.AudioUnverified || !d.Candidates[0].AudioUnverified {
		t.Fatalf("missing unverified flag: %+v", d)
	}
	if d = Select(tg, []Release{r, known}, c); d.Release == nil || d.Release.Guid != known.Guid {
		t.Fatal("known surround did not win")
	}
}

func TestProbeUnavailablePauses(t *testing.T) {
	for _, err := range []error{exec.ErrNotFound, errors.New("bad output"), context.DeadlineExceeded} {
		r, a, _, now := runner(t)
		check(t, r, now)
		a.queue = []QueueItem{{ID: 1, SeriesID: 1, DownloadID: "hash", State: "importPending"}}
		r.Probe = func(context.Context, File) (*MediaInfo, error) { return nil, err }
		observeQuiet(t, r, now.Add(time.Hour))
		st := check(t, r, now.Add(time.Hour))
		if st.Job.Phase != NeedsApproval || !strings.Contains(st.Job.Message, "probe unavailable") || a.imports != 0 || !Healthy(st) {
			t.Fatalf("probe did not pause: %+v", st)
		}
		st, failure := r.Check(context.Background(), now.Add(48*time.Hour))
		if failure != nil || st.Job.Phase != NeedsApproval || !Healthy(st) || a.imports != 0 {
			t.Fatal("unavailable probe bypassed the job timeout")
		}
	}
}

func TestProbeEveryEpisodeAndAbandon(t *testing.T) {
	r, a, _, now := runner(t)
	r.Notify = func(context.Context, string) error { return nil }
	st := check(t, r, now)
	first := st.Job.ReleaseID
	old := a.current
	a.queue = []QueueItem{{ID: 5, SeriesID: 1, DownloadID: "hash", State: "importPending", Size: st.Job.Release.Size}}
	calls := 0
	r.Probe = func(context.Context, File) (*MediaInfo, error) {
		calls++
		channels := 6.0
		if calls == len(a.items) {
			channels = 2
		}
		return &MediaInfo{AudioChannels: channels, AudioCodec: "AAC", VideoCodec: "HEVC"}, nil
	}
	observeQuiet(t, r, now.Add(time.Hour))
	st = check(t, r, now.Add(time.Hour))
	if st.Job.Phase != NeedsApproval || !st.Job.HoldsLease() || len(st.History) != 0 {
		t.Fatal("missing owner wait")
	}
	st, err := r.DecideJob(context.Background(), now.Add(time.Hour), JobID(*st.Job), false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != len(a.items) || st.Job.Phase != Abandoned || a.imports != 0 || !SameFiles(old, a.current) || !Healthy(st) || len(st.History) != 1 || st.History[0].Status != HistoryEnded {
		t.Fatalf("bad abandon: calls %d %+v", calls, st)
	}
	if st.BadReleases[targetKey(old)][first] != "stereo audio" || len(st.AbandonedDownloads) != 1 || len(st.Attempted) != 0 {
		t.Fatalf("abandon did not release work: %+v", st)
	}
	r.queueReports(&st, now.Add(time.Hour))
	if len(st.Notifications) == 0 || !strings.Contains(st.Notifications[0].Body, "abandoned") {
		t.Fatal("missing morning summary")
	}
	marked := false
	for _, c := range st.Report[0].Candidates {
		if c.ID == first {
			marked = strings.Contains(c.Rejection, "rejected after download: stereo audio")
		}
	}
	if !marked {
		t.Fatal("bad release not marked in release list")
	}
	// Restart from persisted state; the old queue row must not block the title.
	observeQuiet(t, r, now.Add(2*time.Hour))
	st = check(t, r, now.Add(2*time.Hour))
	if st.Job == nil || st.Job.Phase != Downloading || st.Job.ReleaseID == first || a.grabs != 2 {
		t.Fatalf("next release blocked: %s %+v", st.Paused, st.Job)
	}
	for _, rel := range a.releases {
		if releaseID(rel) == first {
			if d := selectReleases(old, []Release{rel}, r.Config, st.BadReleases[targetKey(old)]); d.Release != nil {
				t.Fatal("bad release selected after fresh search")
			}
		}
	}
	b, err := os.ReadFile(r.Store.Dir + "/audit.jsonl")
	if err != nil || !strings.Contains(string(b), `"action":"abandoned"`) {
		t.Fatal("missing abandoned audit")
	}
}

func TestManualSkipsProbe(t *testing.T) {
	r, a, _, now := runner(t)
	st := check(t, r, now)
	st.Job.ManuallyApproved = true
	r.Store.Save(st)
	r.Probe = func(context.Context, File) (*MediaInfo, error) { t.Fatal("manual job probed"); return nil, nil }
	a.queue = []QueueItem{{ID: 1, SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	observeQuiet(t, r, now.Add(time.Hour))
	if st = check(t, r, now.Add(time.Hour)); st.Job.Phase != Verifying || a.imports != 1 {
		t.Fatal("manual import blocked")
	}
	a.current = target()
	for i := range a.current.Files {
		a.current.Files[i].ID += 100
		a.current.Files[i].Size = GB
		a.current.Episodes[i].FileID = a.current.Files[i].ID
	}
	observeQuiet(t, r, now.Add(2*time.Hour))
	if st = check(t, r, now.Add(2*time.Hour)); st.Job.Phase != Complete {
		t.Fatal("manual verification blocked")
	}
}

func TestManualApprovalReusesAbandonedDownload(t *testing.T) {
	r, a, _, now := runner(t)
	st := check(t, r, now)
	relID := st.Job.ReleaseID
	a.queue = []QueueItem{{ID: 5, SeriesID: 1, DownloadID: "hash", State: "importPending", Size: st.Job.Release.Size}}
	r.Probe = func(context.Context, File) (*MediaInfo, error) {
		return &MediaInfo{AudioChannels: 2, AudioCodec: "AAC", VideoCodec: "HEVC"}, nil
	}
	observeQuiet(t, r, now.Add(time.Hour))
	st = check(t, r, now.Add(time.Hour))
	if st.Job.Phase != NeedsApproval {
		t.Fatal("fixture not abandoned")
	}
	st, err := r.DecideJob(context.Background(), now.Add(time.Hour), JobID(*st.Job), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Approve(context.Background(), now.Add(time.Hour), ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3, ReleaseID: relID, IndexerID: 10})
	if err != nil {
		t.Fatal(err)
	}
	r.Probe = func(context.Context, File) (*MediaInfo, error) { t.Fatal("manual override probed"); return nil, nil }
	observeQuiet(t, r, now.Add(2*time.Hour))
	st = check(t, r, now.Add(2*time.Hour))
	if st.Job.Phase != Downloading || !st.Job.ManuallyApproved || st.Job.DownloadID != "hash" || len(st.AbandonedDownloads) != 0 || a.grabs != 1 {
		t.Fatalf("retained download not reused: %+v", st)
	}
	observeQuiet(t, r, now.Add(3*time.Hour))
	if st = check(t, r, now.Add(3*time.Hour)); st.Job.Phase != Verifying || a.imports != 1 {
		t.Fatal("override not imported")
	}
}
