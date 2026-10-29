package trend

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

type fakeReader struct {
	mu    sync.Mutex
	epoch uint64
	runs  map[string]*inventory.Inventory
	reads map[string]int
	base  time.Time
	step  time.Duration
	n     int
}

func newFakeReader(n int, step time.Duration) *fakeReader {
	f := &fakeReader{runs: map[string]*inventory.Inventory{}, reads: map[string]int{}, base: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: step}
	for i := 0; i < n; i++ {
		f.add(i)
	}
	return f
}

func (f *fakeReader) at(i int) time.Time { return f.base.Add(time.Duration(i) * f.step) }

func (f *fakeReader) add(i int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[runID(i)] = fakeInventory(f.at(i), 20+float64(i%7)+float64(i)/3)
	f.n = i + 1
}

func runID(i int) string { return fmt.Sprintf("r%03d", i) }

func fakeInventory(at time.Time, pct float64) *inventory.Inventory {
	total := uint64(4 << 40)
	return &inventory.Inventory{
		CollectedAt: at,
		Host: inventory.Host{Hostname: "host-a", Filesystems: []inventory.Filesystem{{
			Device: "/dev/sda1", MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal,
			TotalBytes: total, UsedBytes: uint64(float64(total) * pct / 100), UsedPct: pct,
		}}},
	}
}

// records returns every stored run, newest first, as the store lists them.
func (f *fakeReader) records() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for i := f.n - 1; i >= 0; i-- {
		out = append(out, runID(i))
	}
	return out
}

func (f *fakeReader) RunEpoch(string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch
}

func (f *fakeReader) At(_, id string) (*inventory.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[id]++
	inv, ok := f.runs[id]
	if !ok {
		return nil, errors.New("gone")
	}
	return inv, nil
}

func (f *fakeReader) readCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[id]
}

func (f *fakeReader) invs(recs []string) []*inventory.Inventory {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*inventory.Inventory
	for _, r := range recs {
		if inv, ok := f.runs[r]; ok {
			out = append(out, inv)
		}
	}
	return out
}

func every(recs []string, k int) []string {
	var out []string
	for i := 0; i < len(recs); i += k {
		out = append(out, recs[i])
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReportMatchesBuild(t *testing.T) {
	f := newFakeReader(40, 6*time.Hour)
	f.runs[runID(3)] = &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}}
	live := f.records()
	sampled := every(live, 3)
	now := f.at(39).Add(time.Hour)

	var c Cache
	got, err := c.Report(f, "host-a", live, sampled, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Build(f.invs(sampled), now)
	if g, w := mustJSON(t, got), mustJSON(t, want); g != w {
		t.Fatalf("cached report differs from Build:\n got %s\nwant %s", g, w)
	}
	if got.Runs != len(sampled) {
		t.Fatalf("Runs = %d, want %d", got.Runs, len(sampled))
	}
	if len(got.Series) == 0 || got.Series[0].Forecast == nil {
		t.Fatalf("test history should produce a forecast: %+v", got.Series)
	}

	again, err := c.Report(f, "host-a", live, sampled, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, again) != mustJSON(t, want) {
		t.Fatal("warm report differs from Build")
	}
	if n := f.readCount(runID(39)); n != 1 {
		t.Fatalf("sampled run read %d times, want once", n)
	}
}

func TestReportReadsOnlyNewRuns(t *testing.T) {
	f := newFakeReader(30, time.Hour)
	var c Cache
	live := f.records()
	if _, err := c.Report(f, "host-a", live, live[:10], f.at(29), nil); err != nil {
		t.Fatal(err)
	}

	f.add(30)
	live = f.records()
	rep, err := c.Report(f, "host-a", live, live[:10], f.at(30), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.To != f.at(30) {
		t.Fatalf("new run not picked up: To = %v, want %v", rep.To, f.at(30))
	}
	if n := f.readCount(runID(30)); n != 1 {
		t.Fatalf("new run read %d times, want once", n)
	}
	for _, r := range live[1:10] {
		if n := f.readCount(r); n != 1 {
			t.Fatalf("cached run %s read %d times, want once", r, n)
		}
	}
}

func TestReportEvictsPrunedRuns(t *testing.T) {
	f := newFakeReader(30, time.Hour)
	var c Cache
	live := f.records()
	if _, err := c.Report(f, "host-a", live, live, f.at(30), nil); err != nil {
		t.Fatal(err)
	}

	kept := live[:12]
	if _, err := c.Report(f, "host-a", kept, every(kept, 2), f.at(30), nil); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, r := range kept {
		keep[r] = true
	}
	if got := len(c.hosts["host-a"].runs); got != len(kept) {
		t.Fatalf("cache holds %d runs after prune, want %d", got, len(kept))
	}
	for id := range c.hosts["host-a"].runs {
		if !keep[id] {
			t.Fatalf("pruned run %s still cached", id)
		}
	}
}

func TestReportResetsOnEpochChange(t *testing.T) {
	f := newFakeReader(10, time.Hour)
	var c Cache
	live := f.records()
	if _, err := c.Report(f, "host-a", live, live, f.at(10), nil); err != nil {
		t.Fatal(err)
	}

	// A pruned run's ID can be reused by a later save; the epoch is what says
	// the cached copy is no longer the same run.
	f.runs[runID(9)] = fakeInventory(f.at(9), 90)
	f.epoch++
	rep, err := c.Report(f, "host-a", live, live, f.at(10), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.readCount(runID(9)); n != 2 {
		t.Fatalf("run re-read %d times after epoch change, want 2", n)
	}
	if last := rep.Series[0].Points[len(rep.Series[0].Points)-1]; last.Pct != 90 {
		t.Fatalf("reused ID served stale content: pct %v", last.Pct)
	}
}

func TestReportRetriesUnreadableRun(t *testing.T) {
	f := newFakeReader(6, time.Hour)
	var c Cache
	live := f.records()
	missing := runID(2)
	delete(f.runs, missing)

	var failed []string
	onErr := func(id string, _ error) { failed = append(failed, id) }
	rep, err := c.Report(f, "host-a", live, live, f.at(6), onErr)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0] != missing {
		t.Fatalf("onErr got %v, want [%s]", failed, missing)
	}
	if rep.Runs != len(live)-1 {
		t.Fatalf("Runs = %d, want %d", rep.Runs, len(live)-1)
	}

	f.runs[missing] = fakeInventory(f.at(2), 25)
	rep, err = c.Report(f, "host-a", live, live, f.at(6), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runs != len(live) {
		t.Fatalf("restored run not retried: Runs = %d, want %d", rep.Runs, len(live))
	}
}

func TestReportNoReadableRuns(t *testing.T) {
	f := newFakeReader(3, time.Hour)
	var c Cache
	live := f.records()
	for id := range f.runs {
		delete(f.runs, id)
	}
	if _, err := c.Report(f, "host-a", live, live, f.at(3), nil); !errors.Is(err, ErrNoRuns) {
		t.Fatalf("err = %v, want ErrNoRuns", err)
	}
	if _, err := c.Report(f, "host-a", nil, nil, f.at(3), nil); !errors.Is(err, ErrNoRuns) {
		t.Fatalf("empty history err = %v, want ErrNoRuns", err)
	}
}

func TestReportConcurrent(t *testing.T) {
	f := newFakeReader(60, time.Hour)
	var c Cache
	live := f.records()
	sampled := every(live, 4)
	now := f.at(60)
	want := mustJSON(t, Build(f.invs(sampled), now))

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := c.Report(f, "host-a", live, sampled, now, nil)
			if err != nil {
				errs <- err
				return
			}
			b, _ := json.Marshal(rep)
			if string(b) != want {
				errs <- errors.New("concurrent report differs from Build")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestCacheBoundsHostsAndBytes(t *testing.T) {
	f := newFakeReader(1, time.Hour)
	var c Cache
	for i := 0; i < maxCacheHosts+10; i++ {
		if _, err := c.Report(f, fmt.Sprintf("host-%d", i), []string{runID(0)}, []string{runID(0)}, f.at(1), nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.hosts) != maxCacheHosts {
		t.Fatalf("hosts = %d", len(c.hosts))
	}
	if c.hosts["host-0"] != nil {
		t.Fatal("oldest disappeared host retained")
	}
	// A valid large report must remain complete even when it cannot be cached.
	inv := fakeInventory(f.at(0), 50)
	inv.Host.Filesystems[0].MountPoint = strings.Repeat("x", maxCacheBytes+1)
	f.runs[runID(0)] = inv
	got, err := c.Report(f, "large", []string{runID(0)}, []string{runID(0)}, f.at(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, got) != mustJSON(t, Build([]*inventory.Inventory{inv}, f.at(1))) {
		t.Fatal("budget changed report")
	}
	if c.bytes > maxCacheBytes || c.hosts["large"] != nil {
		t.Fatal("oversized run retained")
	}
}

func TestCacheForgetResetAndConcurrentMaintenance(t *testing.T) {
	f := newFakeReader(10, time.Hour)
	live := f.records()
	var c Cache
	if _, err := c.Report(f, "host-a", live, live, f.at(10), nil); err != nil {
		t.Fatal(err)
	}
	c.Forget("host-a")
	if len(c.hosts) != 0 || c.bytes != 0 {
		t.Fatal("forget retained entries")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := c.Report(f, "host-a", live, live, f.at(10), nil); err != nil {
					t.Error(err)
				}
				c.Forget("host-a")
				c.Reset()
			}
		}()
	}
	wg.Wait()
	c.Reset()
	if len(c.hosts) != 0 || c.bytes != 0 {
		t.Fatal("reset retained entries")
	}
}
