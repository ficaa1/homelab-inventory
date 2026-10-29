package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/store"
	"github.com/ficaa1/homelab-inventory/internal/trend"
)

func trendTestInv(at time.Time, pct float64) *inventory.Inventory {
	total := uint64(4 << 40)
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   at,
		Host: inventory.Host{Hostname: "host-a", Filesystems: []inventory.Filesystem{{
			Device: "/dev/sda1", MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal,
			TotalBytes: total, UsedBytes: uint64(float64(total) * pct / 100), UsedPct: pct,
		}}},
	}
}

func getTrend(t *testing.T, h http.Handler) trend.Report {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hosts/host-a/trend", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("trend: %d %s", rec.Code, rec.Body.String())
	}
	var rep trend.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	// The forecast is fitted against the wall clock, so two requests a few
	// milliseconds apart can legitimately differ in it. Its exact value is
	// pinned against a fixed clock in internal/trend.
	for i := range rep.Series {
		rep.Series[i].Forecast = nil
		rep.Series[i].Note = ""
	}
	return rep
}

func newTrendStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestHostTrendCachedMatchesFreshServer(t *testing.T) {
	st := newTrendStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 12; i++ {
		if _, err := st.Save(trendTestInv(now.Add(-time.Duration(12-i)*time.Hour), 30+float64(i))); err != nil {
			t.Fatal(err)
		}
	}
	quiet := log.New(io.Discard, "", 0)
	warm := (&server{store: st, log: quiet}).routes()

	first := getTrend(t, warm)
	second := getTrend(t, warm)
	fresh := getTrend(t, (&server{store: st, log: quiet}).routes())
	if first.Runs != 12 {
		t.Fatalf("Runs = %d, want 12", first.Runs)
	}
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, fresh) {
		t.Fatal("cached response differs from a fresh server's")
	}
}

func TestHostTrendPicksUpNewRunAndPrunedRuns(t *testing.T) {
	st := newTrendStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-8 * 24 * time.Hour)
	for i := 0; i < 40; i++ {
		if _, err := st.Save(trendTestInv(start.Add(time.Duration(i)*15*time.Minute), 40)); err != nil {
			t.Fatal(err)
		}
	}
	h := (&server{store: st, log: log.New(io.Discard, "", 0)}).routes()
	before := getTrend(t, h)
	if before.Runs != 40 {
		t.Fatalf("Runs before prune = %d, want 40", before.Runs)
	}

	res, err := st.Prune(store.DefaultPolicy(), now, false)
	if err != nil {
		t.Fatal(err)
	}
	records, err := st.History("host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) >= 40 || len(records) == 0 {
		t.Fatalf("prune kept %d of 40 runs (plan %+v)", len(records), res)
	}
	pruned := getTrend(t, h)
	if pruned.Runs != len(records) {
		t.Fatalf("Runs after prune = %d, want %d", pruned.Runs, len(records))
	}

	newest := now.Add(-time.Minute)
	if _, err := st.Save(trendTestInv(newest, 55)); err != nil {
		t.Fatal(err)
	}
	added := getTrend(t, h)
	if added.Runs != len(records)+1 {
		t.Fatalf("Runs after new run = %d, want %d", added.Runs, len(records)+1)
	}
	if !added.To.Equal(newest) {
		t.Fatalf("new run not picked up: To = %v, want %v", added.To, newest)
	}
}

func TestTrendCompressedAndReusedRunIDs(t *testing.T) {
	st := newTrendStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	at := now.Add(-10 * 24 * time.Hour)
	if _, err := st.Save(trendTestInv(at, 40)); err != nil {
		t.Fatal(err)
	}
	s := &server{store: st, log: log.New(io.Discard, "", 0)}
	h := s.routes()
	before := getTrend(t, h)
	// Compression changes the representation but verifies identical raw bytes.
	if _, err := st.Prune(store.DefaultPolicy(), now, false); err != nil {
		t.Fatal(err)
	}
	rs, err := st.History("host-a")
	if err != nil || !rs[0].Compressed {
		t.Fatal("not compressed", err)
	}
	if got := getTrend(t, h); !reflect.DeepEqual(got, before) {
		t.Fatal("compressed warm report changed")
	}
	if got := getTrend(t, (&server{store: st, log: s.log}).routes()); !reflect.DeepEqual(got, before) {
		t.Fatal("compressed cold report changed")
	}
	// DELETE must release the cache even if this host is never read again.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/storage/runs/host-a", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	// Reusing a compressed run's old ID must extract the newly saved contents.
	if _, err := st.Save(trendTestInv(at, 90)); err != nil {
		t.Fatal(err)
	}
	got := getTrend(t, h)
	if got.Series[0].Current != 90 {
		t.Fatalf("reused ID retained old percent: %v", got.Series[0].Current)
	}
}
