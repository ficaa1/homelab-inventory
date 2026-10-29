package overview

import (
	"context"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

func runFixture(host string, count int, newest time.Time) []*inventory.Inventory {
	var vs []*inventory.Inventory
	for i := 0; i < count; i++ {
		v := inv(host)
		v.CollectedAt = newest.Add(-time.Duration(count-1-i) * 15 * time.Minute)
		step := uint64(i)
		v.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{{
			Name: "tank", Health: "ONLINE", SizeBytes: 1 << 40,
			AllocBytes: 1<<39 + step<<26, CapacityPct: 50 + i%7,
		}}}
		v.Proxmox = &inventory.Proxmox{Available: true, Storages: []inventory.Storage{
			{Name: "local-lvm", Type: "lvmthin", Active: true, Enabled: true, TotalBytes: 1 << 40, UsedBytes: 1<<38 + step<<24},
			{Name: "nfs", Type: "nfs", Active: i%5 != 0, Enabled: true, TotalBytes: 1 << 41, UsedBytes: 1<<39 + step<<23},
		}}
		v.Host.Filesystems = []inventory.Filesystem{{
			Kind: inventory.FSKindReal, MountPoint: "/", FSType: "ext4", TotalBytes: 1 << 36,
			UsedBytes: 1<<35 + step<<20, UsedPct: float64(50+i%9) / 100,
		}}
		vs = append(vs, v)
	}
	return vs
}

// memoPolicy keeps the last day whole and thins older runs to hourly, so the
// 24-hour prior window still holds observations while the fixture gzips.
var memoPolicy = store.Policy{KeepAllDays: 1, HourlyDays: 4, DailyDays: 365, CompressAfterDays: 1}

func memoKeys(m *RunMemo, host string) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for k := range m.entries {
		if k.host == host {
			out[k.id] = true
		}
	}
	return out
}

func storageRows(t *testing.T, r Report) []Storage {
	t.Helper()
	rows, ok := r.Storage.([]Storage)
	if !ok {
		t.Fatalf("storage unavailable: %+v", r.Storage)
	}
	return rows
}

func TestRunMemoMatchesFreshParseWithCompressedRuns(t *testing.T) {
	s := saved(t, runFixture("host-b", 300, now)...)
	if _, err := s.Prune(memoPolicy, now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	records, err := s.History("host-b")
	if err != nil {
		t.Fatal(err)
	}
	gz := 0
	for _, r := range records {
		if r.Compressed {
			gz++
		}
	}
	if gz == 0 {
		t.Fatal("fixture has no gzip runs")
	}
	ctx := context.Background()
	fresh := storageRows(t, (Builder{Store: s}).Build(ctx, now))
	memo := &RunMemo{}
	cold := storageRows(t, (Builder{Store: s, Runs: memo}).Build(ctx, now))
	warm := storageRows(t, (Builder{Store: s, Runs: memo}).Build(ctx, now))
	if !reflect.DeepEqual(fresh, cold) || !reflect.DeepEqual(fresh, warm) {
		t.Fatalf("memoized storage differs from a fresh parse\nfresh: %+v\ncold:  %+v\nwarm:  %+v", fresh, cold, warm)
	}
	var deltas, sparks int
	for _, r := range fresh {
		if r.Delta24hBytes != nil {
			deltas++
		}
		if len(r.Spark) > 0 {
			sparks++
		}
	}
	if deltas == 0 || sparks == 0 {
		t.Fatalf("comUTCon is vacuous: %d deltas, %d sparklines", deltas, sparks)
	}
}

func TestRunMemoForgetsPrunedAndForgottenRuns(t *testing.T) {
	s := saved(t, runFixture("host-b", 300, now)...)
	for _, v := range runFixture("nas", 20, now.AddDate(0, 0, -30)) {
		if _, err := s.Save(v); err != nil {
			t.Fatal(err)
		}
	}
	memo := &RunMemo{}
	b := Builder{Store: s, Runs: memo}
	b.Build(context.Background(), now)
	before := memoKeys(memo, "host-b")
	if len(before) == 0 || len(memoKeys(memo, "nas")) == 0 {
		t.Fatal("memo was not populated")
	}
	if _, err := s.Prune(memoPolicy, now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	records, err := s.History("host-b")
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, r := range records {
		live[r.ID] = true
	}
	b.Build(context.Background(), now)
	for id := range memoKeys(memo, "host-b") {
		if !live[id] {
			t.Fatalf("pruned run %s still memoized", id)
		}
	}
	if len(memoKeys(memo, "host-b")) >= len(before) {
		t.Fatal("pruning evicted nothing")
	}
	if err := s.ForgetHost("nas", now); err != nil {
		t.Fatal(err)
	}
	b.Build(context.Background(), now)
	if n := len(memoKeys(memo, "nas")); n != 0 {
		t.Fatalf("forgotten host still has %d memoized runs", n)
	}
}

func TestRunMemoParsesEachRunOnceUnderConcurrency(t *testing.T) {
	cs := &countedStore{Store: saved(t, runFixture("host-b", 260, now)...)}
	memo := &RunMemo{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			(Builder{Store: cs, Runs: memo}).Build(context.Background(), now)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := memo.Warm(cs); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	memo.mu.Lock()
	entries := len(memo.entries)
	memo.mu.Unlock()
	if entries == 0 || int(cs.reads.Load()) != entries {
		t.Fatalf("parses %d for %d memoized runs; each run must be parsed exactly once", cs.reads.Load(), entries)
	}
}

func TestRunMemoErrorsAreNotCached(t *testing.T) {
	s := &failingHistoryStore{countedStore: &countedStore{Store: saved(t, runFixture("host-b", 40, now)...)}, failRead: 3}
	memo := &RunMemo{}
	b := Builder{Store: s, Runs: memo}
	if _, ok := b.Build(context.Background(), now).Storage.(Unavailable); !ok {
		t.Fatal("failed sample reported as storage")
	}
	memo.mu.Lock()
	for k, e := range memo.entries {
		if e.sum == nil {
			memo.mu.Unlock()
			t.Fatalf("failed run %s left in memo", k.id)
		}
	}
	memo.mu.Unlock()
	s.failRead = 0
	got := storageRows(t, b.Build(context.Background(), now))
	want := storageRows(t, (Builder{Store: s.countedStore}).Build(context.Background(), now))
	if !reflect.DeepEqual(got, want) {
		t.Fatal("storage did not recover to the fresh result")
	}
}

func TestRunMemoBoundsItsEntries(t *testing.T) {
	s := saved(t, runFixture("host-b", 1, now)...)
	records, err := s.History("host-b")
	if err != nil {
		t.Fatal(err)
	}
	memo := &RunMemo{entries: map[runKey]*runEntry{}}
	done := make(chan struct{})
	close(done)
	for i := 0; i < maxRunEntries; i++ {
		memo.entries[runKey{host: "filler", id: strconv.Itoa(i)}] = &runEntry{ready: done, sum: &runSummary{}, lastUse: uint64(i + 10)}
	}
	if _, err := memo.summary(context.Background(), s, "host-b", records[0].ID); err != nil {
		t.Fatal(err)
	}
	if len(memo.entries) != maxRunEntries {
		t.Fatalf("memo holds %d entries, want %d", len(memo.entries), maxRunEntries)
	}
	if _, ok := memo.entries[runKey{host: "filler", id: "0"}]; ok {
		t.Fatal("least recently used entry was not evicted")
	}
}

func BenchmarkStorageOverRuns(b *testing.B) {
	s, err := store.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for _, v := range runFixture("host-b", 600, now) {
		if _, err := s.Save(v); err != nil {
			b.Fatal(err)
		}
	}
	latest, err := s.Latest("host-b")
	if err != nil {
		b.Fatal(err)
	}
	invs := []*inventory.Inventory{latest}
	ctx := context.Background()
	b.Run("fresh", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			(Builder{Store: s}).storage(ctx, invs, now)
		}
	})
	b.Run("cold-memo", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			(Builder{Store: s, Runs: &RunMemo{}}).storage(ctx, invs, now)
		}
	})
	b.Run("warm-memo", func(b *testing.B) {
		memo := &RunMemo{}
		(Builder{Store: s, Runs: memo}).storage(ctx, invs, now)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			(Builder{Store: s, Runs: memo}).storage(ctx, invs, now)
		}
	})
}

func TestRunMemoBoundsPendingParses(t *testing.T) {
	s := saved(t, runFixture("host-b", 1, now)...)
	records, _ := s.History("host-b")
	memo := &RunMemo{entries: map[runKey]*runEntry{}}
	for i := 0; i < maxRunEntries; i++ {
		memo.entries[runKey{host: "pending", id: strconv.Itoa(i)}] = &runEntry{ready: make(chan struct{})}
	}
	if _, err := memo.summary(context.Background(), s, "host-b", records[0].ID); err != nil {
		t.Fatal(err)
	}
	if len(memo.entries) > maxRunEntries {
		t.Fatalf("pending reads exceeded cap: %d", len(memo.entries))
	}
}

func TestRunMemoReusedIDAfterForget(t *testing.T) {
	old := runFixture("nas", 1, now.Add(-30*24*time.Hour))[0]
	s := saved(t, old)
	memo := &RunMemo{}
	cache := &StorageCache{}
	b := Builder{Store: s, Runs: memo, Cache: cache}
	first := storageRows(t, b.Build(context.Background(), now))
	if err := s.ForgetHost("nas", now); err != nil {
		t.Fatal(err)
	}
	old.Host.Filesystems[0].UsedBytes++
	old.Host.Filesystems[0].UsedPct++
	old.ZFS.Pools[0].AllocBytes++
	if _, err := s.Save(old); err != nil {
		t.Fatal(err)
	}
	// No intervening overview request observed the absent host. Both memo and
	// aggregation must distinguish the newly saved run under the same ID.
	got := storageRows(t, b.Build(context.Background(), now))
	want := storageRows(t, (Builder{Store: s}).Build(context.Background(), now))
	if reflect.DeepEqual(first, got) || !reflect.DeepEqual(got, want) {
		t.Fatal("reused ID returned the forgotten run")
	}
}

func TestStorageCacheInvalidatedByPrune(t *testing.T) {
	s := saved(t, runFixture("host-b", 300, now)...)
	b := Builder{Store: s, Runs: &RunMemo{}, Cache: &StorageCache{}}
	first := storageRows(t, b.Build(context.Background(), now))
	if _, err := s.Prune(memoPolicy, now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	got := storageRows(t, b.Build(context.Background(), now))
	want := storageRows(t, (Builder{Store: s}).Build(context.Background(), now))
	if reflect.DeepEqual(first, want) {
		t.Fatal("fixture did not alter storage history")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("prune left cached history")
	}
}
