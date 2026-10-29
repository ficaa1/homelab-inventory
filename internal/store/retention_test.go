package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/composelog"
	"github.com/ficaa1/homelab-inventory/internal/drift"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/trend"
)

func retentionRun(t *testing.T, s *Store, at time.Time, hash string) Record {
	t.Helper()
	inv := &inventory.Inventory{CollectedAt: at, Host: inventory.Host{Hostname: "host-a"}, Docker: &inventory.Docker{Available: true}}
	if hash != "" {
		inv.Docker.ComposeProjects = []inventory.ComposeProject{{Name: "app", ConfigFiles: []inventory.ComposeFile{{Path: "/compose.yml", Exists: true, SHA256: hash, Content: "services: " + hash}}}}
	}
	r, err := s.Save(inv)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestPruneThinningBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s, _ := Open(t.TempDir())
	// Use UTC-aligned boundaries; within each sampled bucket the oldest wins.
	ages := []time.Duration{400 * 24 * time.Hour, 365*24*time.Hour + time.Second, 365 * 24 * time.Hour, 364 * 24 * time.Hour, 364*24*time.Hour - time.Minute, 30*24*time.Hour + time.Second, 30 * 24 * time.Hour, 30*24*time.Hour - time.Minute, 7*24*time.Hour + time.Minute, 7 * 24 * time.Hour, 7*24*time.Hour - time.Minute, time.Hour}
	rs := make([]Record, len(ages))
	for i, a := range ages {
		rs[i] = retentionRun(t, s, now.Add(-a), "")
	}
	plan, err := s.Prune(DefaultPolicy(), now, true)
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, a := range plan.Actions {
		if a.Action == "delete" {
			removed[a.ID] = true
		}
	}
	// Latest is always protected. At exactly 365d the
	// daily sample stays; just outside expiry goes. Exactly 30d is hourly,
	// exactly 7d and newer are all retained.
	for i, want := range []bool{true, true, false, false, true, false, false, true, false, false, false, false} {
		if removed[rs[i].ID] != want {
			t.Errorf("age=%s deleted=%v want=%v", ages[i], removed[rs[i].ID], want)
		}
	}
	before, _ := s.History("host-a")
	after, _ := s.History("host-a")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("dry run changed records")
	}
	actual, err := s.Prune(DefaultPolicy(), now, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, func() PruneResult { actual.DryRun = true; return actual }()) {
		t.Fatal("apply differed from dry plan")
	}
	again, err := s.Prune(DefaultPolicy(), now, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Actions) != 0 {
		t.Fatalf("not idempotent: %+v", again.Actions)
	}
}
func TestPruneComposeTransitionsAndOnlyRun(t *testing.T) {
	now := time.Now().UTC()
	s, _ := Open(t.TempDir())
	// Transitions outside even the daily expiry remain, including a revert and
	// file disappearance. An unchanged run in the expired tier may be removed.
	hashes := []string{"a", "a", "b", "a", "", "", "c"}
	rs := make([]Record, len(hashes))
	for i, h := range hashes {
		rs[i] = retentionRun(t, s, now.Add(-time.Duration(500*24-i)*time.Hour), h)
	}
	p := DefaultPolicy()
	p.DailyDays = 366
	_, err := s.Prune(p, now, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rs {
		_, err := s.At("host-a", r.ID)
		wantMissing := i == 1 || i == 5
		if errors.Is(err, ErrNotFound) != wantMissing {
			t.Errorf("run %d missing=%v want=%v", i, err, wantMissing)
		}
	}
	single := &inventory.Inventory{Host: inventory.Host{Hostname: "only"}, CollectedAt: now.AddDate(-2, 0, 0)}
	r, err := s.Save(single)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Prune(p, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.At("only", r.ID); err != nil {
		t.Fatal("only run lost", err)
	}
}
func TestGzipReadTransparency(t *testing.T) {
	now := time.Now().UTC()
	s, _ := Open(t.TempDir())
	r1 := retentionRun(t, s, now.AddDate(0, 0, -10), "old")
	r2 := retentionRun(t, s, now.AddDate(0, 0, -9), "new")
	before1, _ := s.At("host-a", r1.ID)
	before2, _ := s.At("host-a", r2.ID)
	composeBefore := composelog.Build([]*inventory.Inventory{before1, before2}, []string{r1.ID, r2.ID}, true)
	driftBefore := drift.Compare(before1, before2, r1.ID, r2.ID)
	trendBefore := trend.Build([]*inventory.Inventory{before1, before2}, now)
	_, err := s.Prune(DefaultPolicy(), now, false)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := s.History("host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || !hist[0].Compressed || !hist[1].Compressed {
		t.Fatalf("history=%+v", hist)
	}
	for _, r := range hist {
		fi, err := os.Stat(filepath.Join(s.dir, "host-a", r.ID+".json.gz"))
		if err != nil || fi.Size() != r.SizeBytes {
			t.Fatal("wrong physical size")
		}
	}
	a, _ := s.At("host-a", r1.ID)
	b, _ := s.At("host-a", r2.ID)
	latest, err := s.Latest("host-a")
	if err != nil || !reflect.DeepEqual(latest, before2) {
		t.Fatal("latest mismatch", err)
	}
	all, err := s.LatestAll()
	if err != nil || len(all) != 1 || !reflect.DeepEqual(all[0], before2) {
		t.Fatal("latestAll mismatch", err)
	}
	if !reflect.DeepEqual(a, before1) || !reflect.DeepEqual(b, before2) {
		t.Fatal("At mismatch")
	}
	if !reflect.DeepEqual(composeBefore, composelog.Build([]*inventory.Inventory{a, b}, []string{r1.ID, r2.ID}, true)) {
		t.Fatal("compose changed")
	}
	if !reflect.DeepEqual(driftBefore, drift.Compare(a, b, r1.ID, r2.ID)) {
		t.Fatal("drift changed")
	}
	if !reflect.DeepEqual(trendBefore, trend.Build([]*inventory.Inventory{a, b}, now)) {
		t.Fatal("trend changed")
	}
	if _, err = s.Save(before2); err == nil {
		t.Fatal("Save clobbered compressed ID")
	}
}
func TestCompressionVerificationFailureAndCrashRecovery(t *testing.T) {
	s, _ := Open(t.TempDir())
	r := retentionRun(t, s, time.Now().AddDate(0, 0, -10), "a")
	path := filepath.Join(s.dir, "host-a", r.ID+".json")
	original, _ := os.ReadFile(path)
	_, err := compressRun(path, false, func(p string) ([]byte, error) {
		// Simulate corruption after the atomic rename, before verification.
		if err := os.WriteFile(p, []byte("broken gzip"), 0600); err != nil {
			t.Fatal(err)
		}
		return readGzip(p)
	})
	if err == nil {
		t.Fatal("expected verification failure")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatal("original removed or changed")
	}
	if _, err = s.At("host-a", r.ID); err != nil {
		t.Fatal("original unreadable", err)
	}
	hist, _ := s.History("host-a")
	if len(hist) != 1 || hist[0].Compressed || hist[0].SizeBytes != int64(len(original)+len("broken gzip")) {
		t.Fatalf("duplicate accounting %+v", hist)
	}
	_, err = compressRun(path, false, readGzip)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("original remains after verified recovery")
	}
	if _, err = s.At("host-a", r.ID); err != nil {
		t.Fatal(err)
	}
	// A mismatched but readable verification must also retain the original.
	r = retentionRun(t, s, time.Now().AddDate(0, 0, -11), "a")
	path = filepath.Join(s.dir, "host-a", r.ID+".json")
	_, err = compressRun(path, false, func(string) ([]byte, error) { return []byte("wrong"), nil })
	if err == nil {
		t.Fatal("accepted mismatch")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("original missing")
	}
}
func TestPruneDryRunAndCorruptHistory(t *testing.T) {
	now := time.Now().UTC()
	s, _ := Open(t.TempDir())
	r := retentionRun(t, s, now.AddDate(0, 0, -40), "a")
	retentionRun(t, s, now.AddDate(0, 0, -40).Add(time.Minute), "a")
	retentionRun(t, s, now.AddDate(0, 0, -1), "a")
	snapshot := func() map[string]string {
		out := map[string]string{}
		filepath.Walk(s.dir, func(p string, i os.FileInfo, e error) error {
			if e != nil {
				return e
			}
			if !i.IsDir() {
				b, _ := os.ReadFile(p)
				out[p] = string(b)
			}
			return nil
		})
		return out
	}
	before := snapshot()
	plan, err := s.Prune(DefaultPolicy(), now, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FilesDeleted != 1 || plan.FilesCompressed != 1 || !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("dry run mutation or wrong plan", plan)
	}
	os.WriteFile(filepath.Join(s.dir, "host-a", r.ID+".json"), []byte("{broken"), 0600)
	before = snapshot()
	failed, err := s.Prune(DefaultPolicy(), now, false)
	if err == nil {
		t.Fatal("ignored corrupt compose history")
	}
	if len(failed.Hosts) != 1 || failed.Hosts[0].RunsAfter != 3 || failed.Hosts[0].BytesAfter != failed.Hosts[0].BytesBefore {
		t.Fatal("failure summary lost the unchanged host", failed)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("mutated host before validating all its history")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.PruneContext(ctx, DefaultPolicy(), now, false); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
}

func TestRetentionUses24HourDaysAcrossDST(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, zone)
	boundary := now.Add(-7 * 24 * time.Hour)
	if older(boundary, now, 7) || !older(boundary.Add(-time.Second), now, 7) {
		t.Fatal("retention age changed with daylight saving time")
	}
}

func TestPruneDailyForeverAndCompressionBoundary(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s, _ := Open(t.TempDir())
	ancient := retentionRun(t, s, now.AddDate(-5, 0, 0), "")
	duplicate := retentionRun(t, s, ancient.CollectedAt.Add(time.Minute), "")
	boundary := retentionRun(t, s, now.Add(-2*24*time.Hour), "")
	olderRun := retentionRun(t, s, boundary.CollectedAt.Add(-time.Second), "")
	retentionRun(t, s, now, "")
	p := DefaultPolicy()
	p.DailyDays = 0
	_, err := s.Prune(p, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.At("host-a", ancient.ID); err != nil {
		t.Fatal("daily forever lost an ancient sample", err)
	}
	if _, err := s.At("host-a", duplicate.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("daily forever retained a duplicate", err)
	}
	rs, _ := s.History("host-a")
	for _, r := range rs {
		if r.ID == boundary.ID && r.Compressed || r.ID == olderRun.ID && !r.Compressed {
			t.Fatal("wrong compression age boundary", r)
		}
	}
}
func TestForgetHostFreshnessAndTraversal(t *testing.T) {
	now := time.Now().UTC()
	s, _ := Open(t.TempDir())
	r := retentionRun(t, s, now.AddDate(0, 0, -7), "")
	if err := s.ForgetHost("host-a", now); !errors.Is(err, ErrActiveHost) {
		t.Fatal("accepted boundary", err)
	}
	if _, err := s.At("host-a", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetHost("../escape", now); err == nil {
		t.Fatal("accepted path traversal")
	}
	usage, err := s.Usage(now.Add(time.Second))
	if err != nil || !usage.Hosts[0].Stale {
		t.Fatal("stale detection", err)
	}
	if err := s.ForgetHost("host-a", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.History("host-a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("not forgotten")
	}
}
func TestComposeSignatureOrderingAndSet(t *testing.T) {
	f := func(path, hash string) inventory.ComposeFile {
		return inventory.ComposeFile{Path: path, Exists: true, SHA256: hash}
	}
	inv := &inventory.Inventory{Docker: &inventory.Docker{ComposeProjects: []inventory.ComposeProject{{Name: "a", ConfigFiles: []inventory.ComposeFile{f("a", "1"), f("b", "2")}}}}}
	sig := composeSignature(inv)
	inv.Docker.ComposeProjects[0].ConfigFiles = []inventory.ComposeFile{f("b", "2"), f("a", "1")}
	if sig != composeSignature(inv) {
		t.Fatal("ordering changed signature")
	}
	inv.Docker.ComposeProjects[0].ConfigFiles = []inventory.ComposeFile{f("a", "1")}
	if sig == composeSignature(inv) {
		t.Fatal("removal not detected")
	}
	if strings.Contains(sig, "services:") {
		t.Fatal("signature includes contents")
	}
}
