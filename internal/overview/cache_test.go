package overview

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/seen"
	"github.com/ficaa1/homelab-inventory/internal/silence"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

type countedStore struct {
	*store.Store
	hosts        atomic.Int32
	histories    atomic.Int32
	reads        atomic.Int32
	afterHistory func()
	afterRead    func()
}

func (s *countedStore) Hosts() ([]string, error) {
	s.hosts.Add(1)
	return s.Store.Hosts()
}
func (s *countedStore) History(host string) ([]store.Record, error) {
	s.histories.Add(1)
	records, err := s.Store.History(host)
	if s.afterHistory != nil {
		s.afterHistory()
	}
	return records, err
}
func (s *countedStore) At(host, id string) (*inventory.Inventory, error) {
	s.reads.Add(1)
	v, err := s.Store.At(host, id)
	if s.afterRead != nil {
		s.afterRead()
	}
	return v, err
}
func historyStore(t *testing.T) *countedStore {
	t.Helper()
	var vs []*inventory.Inventory
	for i := 0; i < 40; i++ {
		v := inv("disk")
		v.CollectedAt = now.Add(-time.Duration(39-i) * time.Hour)
		v.Host.Filesystems = []inventory.Filesystem{{Kind: inventory.FSKindReal, MountPoint: "/", FSType: "ext4", TotalBytes: 10000, UsedBytes: uint64(100 + i), UsedPct: float64(100+i) / 100}}
		vs = append(vs, v)
	}
	return &countedStore{Store: saved(t, vs...)}
}
func TestLocalCancellationStopsReads(t *testing.T) {
	for _, stage := range []string{"before", "history", "sample"} {
		t.Run(stage, func(t *testing.T) {
			s := historyStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "before":
				cancel()
			case "history":
				s.afterHistory = cancel
			case "sample":
				s.afterRead = cancel
			}
			r := (Builder{Store: s}).Build(ctx, now)
			if _, ok := r.Storage.(Unavailable); !ok {
				t.Fatalf("canceled storage: %+v", r.Storage)
			}
			want := int32(0)
			if stage == "sample" {
				want = 1
			}
			if s.reads.Load() != want {
				t.Fatalf("history reads after cancellation: %d, want %d", s.reads.Load(), want)
			}
			if stage == "before" && s.hosts.Load() != 0 {
				t.Fatal("canceled request accessed store")
			}
		})
	}
}
func TestStorageCacheHitsExpiryAndInventoryChange(t *testing.T) {
	s := historyStore(t)
	b := Builder{Store: s, Cache: &StorageCache{}}
	rows := b.Build(context.Background(), now).Storage.([]Storage)
	reads := s.reads.Load()
	if reads == 0 {
		t.Fatal("cold request did not read history")
	}
	// Caller mutation cannot corrupt subsequent responses or cause data races.
	*rows[0].Pct = 999
	rows[0].Spark[0] = 999
	rows = b.Build(context.Background(), now.Add(time.Second)).Storage.([]Storage)
	if s.reads.Load() != reads || *rows[0].Pct == 999 || rows[0].Spark[0] == 999 {
		t.Fatal("cache missed or response shares mutable data")
	}
	b.Build(context.Background(), now.Add(storageCacheTTL))
	if s.reads.Load() <= reads {
		t.Fatal("expired cache did not refresh")
	}
	reads = s.reads.Load()
	v := inv("disk")
	v.CollectedAt = now.Add(time.Second)
	v.Host.Filesystems = []inventory.Filesystem{{Kind: inventory.FSKindReal, MountPoint: "/", FSType: "ext4", TotalBytes: 10000, UsedBytes: 500, UsedPct: 5}}
	if _, err := s.Save(v); err != nil {
		t.Fatal(err)
	}
	rows = b.Build(context.Background(), now.Add(storageCacheTTL+time.Second)).Storage.([]Storage)
	if s.reads.Load() <= reads || *rows[0].Pct != 5 {
		t.Fatal("new inventory reused stale cache")
	}
	reads = s.reads.Load()
	b.Cache.Invalidate()
	b.Build(context.Background(), now.Add(storageCacheTTL+2*time.Second))
	if s.reads.Load() <= reads {
		t.Fatal("invalidated cache reused history")
	}
}
func TestStorageCacheCoalescesRefresh(t *testing.T) {
	s := historyStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.afterHistory = func() { once.Do(func() { close(entered); <-release }) }
	b := Builder{Store: s, Cache: &StorageCache{}}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); b.Build(context.Background(), now) }()
	}
	<-entered
	close(release)
	wg.Wait()
	if s.histories.Load() != 1 {
		t.Fatalf("duplicate refreshes: %d", s.histories.Load())
	}
}
func TestStorageCacheCanceledWaiterAndLeader(t *testing.T) {
	s := historyStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.afterHistory = func() { once.Do(func() { close(entered); <-release }) }
	b := Builder{Store: s, Cache: &StorageCache{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Report, 1)
	go func() { done <- b.Build(ctx, now) }()
	<-entered
	waiterCtx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	waiter := make(chan Report, 1)
	go func() { waiter <- b.Build(waiterCtx, now) }()
	select {
	case r := <-waiter:
		if _, ok := r.Storage.(Unavailable); !ok {
			t.Fatal("canceled waiter returned cached success")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ignored cancellation")
	}
	cancel()
	close(release)
	<-done
	if s.reads.Load() != 0 {
		t.Fatal("canceled leader kept reading history")
	}
	r := b.Build(context.Background(), now)
	if _, ok := r.Storage.([]Storage); !ok || s.histories.Load() != 2 {
		t.Fatal("canceled refresh poisoned cache")
	}
}
func TestStorageCacheInvalidationDuringRefresh(t *testing.T) {
	var c StorageCache
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan any, 1)
	go func() {
		done <- c.load(context.Background(), "key", now, func() any { close(entered); <-release; return []Storage{} })
	}()
	<-entered
	c.Invalidate()
	close(release)
	<-done
	refreshed := false
	c.load(context.Background(), "key", now, func() any { refreshed = true; return []Storage{} })
	if !refreshed {
		t.Fatal("pre-ingest refresh repopulated invalidated cache")
	}
}

// failingHistoryStore injects failures into listing, trend samples or delta reads.
type failingHistoryStore struct {
	*countedStore
	failListing  bool
	failRead     int32
	refreshReads int32
}

func (s *failingHistoryStore) History(host string) ([]store.Record, error) {
	s.refreshReads = 0
	records, err := s.countedStore.History(host)
	if s.failListing {
		return nil, errors.New("listing failed")
	}
	return records, err
}
func (s *failingHistoryStore) At(host, id string) (*inventory.Inventory, error) {
	v, err := s.countedStore.At(host, id)
	s.refreshReads++
	if s.failRead > 0 && s.refreshReads == s.failRead {
		return nil, errors.New("sample failed")
	}
	return v, err
}
func TestStorageHistoryErrorsAreUnavailableAndNotCached(t *testing.T) {
	for _, stage := range []string{"listing", "sample", "delta"} {
		t.Run(stage, func(t *testing.T) {
			s := &failingHistoryStore{countedStore: historyStore(t)}
			switch stage {
			case "listing":
				s.failListing = true
			case "sample":
				s.failRead = 2
			case "delta":
				s.failRead = 41 // Forty trend samples precede the first delta read.
			}
			b := Builder{Store: s, Cache: &StorageCache{}}
			for i := 0; i < 2; i++ {
				r := b.Build(context.Background(), now.Add(time.Duration(i)*time.Second))
				u, ok := r.Storage.(Unavailable)
				if !ok || u.Status != "unavailable" || u.Error == "" {
					t.Fatalf("failed history reported as successful storage: %+v", r.Storage)
				}
				if _, ok := r.Fleet.(Fleet); !ok {
					t.Fatal("history error suppressed live fleet")
				}
				if s.histories.Load() != int32(i+1) {
					t.Fatal("history error was cached")
				}
			}
			s.failListing, s.failRead = false, 0
			r := b.Build(context.Background(), now.Add(2*time.Second))
			rows, ok := r.Storage.([]Storage)
			if !ok || len(rows) != 1 || len(rows[0].Spark) == 0 || rows[0].Delta24hBytes == nil {
				t.Fatalf("history did not recover immediately: %+v", r.Storage)
			}
			reads := s.reads.Load()
			b.Build(context.Background(), now.Add(3*time.Second))
			if s.histories.Load() != 3 || s.reads.Load() != reads {
				t.Fatal("recovered storage was not cached")
			}
		})
	}
}

type cancelStateStore struct {
	*countedStore
	cancel context.CancelFunc
	stage  string
	states []string
}

func (s *cancelStateStore) ReadState(name string, dst any) error {
	s.states = append(s.states, name)
	err := s.Store.ReadState(name, dst)
	if name == s.stage {
		s.cancel()
	}
	return err
}
func TestStateCancellationStopsSubsequentLocalReads(t *testing.T) {
	for _, stage := range []string{silence.StateName, seen.StateName} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &cancelStateStore{countedStore: historyStore(t), cancel: cancel, stage: stage}
			r := (Builder{Store: s, Cache: &StorageCache{}}).Build(ctx, now)
			want := 1
			if stage != silence.StateName {
				want = 2
			}
			if len(s.states) != want {
				t.Fatalf("state reads: %v, want %d", s.states, want)
			}
			if s.histories.Load() != 0 || s.reads.Load() != 0 {
				t.Fatal("canceled state read continued into history")
			}
			for _, section := range []any{r.Fleet, r.Attention, r.Storage, r.ZFS, r.Backups, r.UnitsFailed} {
				u, ok := section.(Unavailable)
				if !ok || u.Error != "request canceled" {
					t.Fatalf("canceled local section: %+v", section)
				}
			}
		})
	}
}
