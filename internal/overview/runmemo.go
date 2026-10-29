package overview

import (
	"context"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/capacity"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/store"
	"github.com/ficaa1/homelab-inventory/internal/trend"
)

// maxRunEntries bounds the memo. The dashboard's working set is about 200 runs
// per host plus one day-earlier window, so this covers a fleet of some twenty
// hosts before the least recently used runs start being parsed again.
const maxRunEntries = 8192

// RunMemo keeps the small per-run results storage() needs, keyed by host and
// run ID. Save refuses content changes under an existing ID; compression keeps
// the content. A store epoch distinguishes IDs reused after deletion.
// Concurrent readers of one run share a single parse. The zero value is ready
// to use.
type RunMemo struct {
	mu      sync.Mutex
	entries map[runKey]*runEntry
	clock   uint64
}

type runKey struct {
	host, id string
	epoch    uint64
}

type runEntry struct {
	ready   chan struct{}
	sum     *runSummary
	err     error
	lastUse uint64
}

// runSummary holds what one run contributes: its trend samples, and its capacity
// rows with whether each PVE row could serve as the prior side of a 24-hour delta.
type runSummary struct {
	collectedAt time.Time
	samples     []trend.Sample
	capacity    []runCapacity
}

type runCapacity struct {
	kind     capacity.Kind
	name     string
	total    int64
	used     int64
	readable bool
}

// summarize reads a run's summary from its inventory. Trend samples see only
// active Proxmox storages, as the capacity history always has; the delta rows
// see the full inventory, as they always have.
func summarize(v *inventory.Inventory) *runSummary {
	s := &runSummary{collectedAt: v.CollectedAt, samples: trend.Samples(trendView(v))}
	for _, r := range capacity.Build([]*inventory.Inventory{v}, nil).Rows {
		s.capacity = append(s.capacity, runCapacity{kind: r.Kind, name: r.Name, total: r.TotalBytes, used: r.UsedBytes, readable: readablePVE(v, r)})
	}
	return s
}

func trendView(v *inventory.Inventory) *inventory.Inventory {
	if v.Proxmox == nil {
		return v
	}
	c := *v
	p := *v.Proxmox
	p.Storages = nil
	for _, st := range v.Proxmox.Storages {
		if st.Active {
			p.Storages = append(p.Storages, st)
		}
	}
	c.Proxmox = &p
	return &c
}

// summary returns the memoized summary for one run, parsing it at most once
// while it stays cached. Errors are returned to the waiters but not kept.
func (m *RunMemo) summary(ctx context.Context, src InventoryStore, host, id string) (*runSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := runKey{host: host, id: id, epoch: runEpoch(src, host)}
	m.mu.Lock()
	if m.entries == nil {
		m.entries = map[runKey]*runEntry{}
	}
	m.clock++
	if e, ok := m.entries[key]; ok {
		e.lastUse = m.clock
		m.mu.Unlock()
		select {
		case <-e.ready:
			return e.sum, e.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.evictLocked()
	// Pending parses cannot be evicted. At capacity, parse without retaining
	// another entry rather than growing the memo under concurrent cold reads.
	if len(m.entries) >= maxRunEntries {
		m.mu.Unlock()
		v, err := src.At(host, id)
		if err != nil {
			return nil, err
		}
		return summarize(v), nil
	}
	e := &runEntry{ready: make(chan struct{}), lastUse: m.clock}
	m.entries[key] = e
	m.mu.Unlock()

	v, err := src.At(host, id)
	m.mu.Lock()
	if err != nil {
		e.err = err
		if m.entries[key] == e {
			delete(m.entries, key)
		}
	} else {
		e.sum = summarize(v)
	}
	m.mu.Unlock()
	close(e.ready)
	return e.sum, e.err
}

// evictLocked makes room for one entry by dropping finished LRU entries.
func (m *RunMemo) evictLocked() {
	for len(m.entries) >= maxRunEntries {
		var oldest runKey
		var oldestUse uint64
		found := false
		for k, e := range m.entries {
			select {
			case <-e.ready:
			default:
				continue
			}
			if !found || e.lastUse < oldestUse {
				oldest, oldestUse, found = k, e.lastUse, true
			}
		}
		if !found {
			return
		}
		delete(m.entries, oldest)
	}
}

// retain forgets a host's runs that are no longer stored, so pruned runs leave
// the memo the next time the host's history is read.
func (m *RunMemo) retain(host string, records []store.Record) {
	keep := make(map[string]bool, len(records))
	for _, r := range records {
		keep[r.ID] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.entries {
		if k.host == host && !keep[k.id] {
			delete(m.entries, k)
		}
	}
}

// retainHosts forgets every run of a host that is no longer listed.
func (m *RunMemo) retainHosts(hosts []string) {
	keep := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		keep[h] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.entries {
		if !keep[k.host] {
			delete(m.entries, k)
		}
	}
}

// Warm summarizes the runs a dashboard read would sample, so the first request
// after a restart does not pay for every parse. It returns how many runs it read.
func (m *RunMemo) Warm(src InventoryStore) (int, error) {
	hosts, err := src.Hosts()
	if err != nil {
		return 0, err
	}
	n := 0
	ctx := context.Background()
	for _, host := range hosts {
		records, err := src.History(host)
		if err != nil || len(records) == 0 {
			continue
		}
		var refs []store.Record
		for _, idx := range sampledIndexes(len(records)) {
			refs = append(refs, records[idx])
		}
		refs = append(refs, windowRecords(records, records[0].CollectedAt.Add(-24*time.Hour))...)
		for _, r := range refs {
			if _, err := m.summary(ctx, src, host, r.ID); err == nil {
				n++
			}
		}
	}
	return n, nil
}

// sampledIndexes lists the history positions a trend reads: every run up to 200,
// otherwise 200 evenly spaced positions across the whole history.
func sampledIndexes(n int) []int {
	k := n
	if k > 200 {
		k = 200
	}
	out := make([]int, k)
	for i := range out {
		out[i] = i
		if n > k {
			out[i] = int(float64(n-1)*float64(i)/float64(k-1) + .5)
		}
	}
	return out
}

// windowRecords returns the runs collected within an hour of target. History is
// newest first, so the scan stops at the first run older than the window.
func windowRecords(records []store.Record, target time.Time) []store.Record {
	var out []store.Record
	for _, rec := range records {
		if rec.CollectedAt.After(target.Add(time.Hour)) {
			continue
		}
		if rec.CollectedAt.Before(target.Add(-time.Hour)) {
			break
		}
		out = append(out, rec)
	}
	return out
}

func runEpoch(src InventoryStore, host string) uint64 {
	if s, ok := src.(interface{ RunEpoch(string) uint64 }); ok {
		return s.RunEpoch(host)
	}
	return 0
}
