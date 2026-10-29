package trend

import (
	"errors"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// ErrNoRuns is returned when none of the sampled runs could be read.
var ErrNoRuns = errors.New("no readable runs")

// Run is one stored run reduced to what a trend draws from. A run is parsed
// once; after that its samples are all a report needs.
type Run struct {
	Host      string
	Collected bool
	Samples   []Sample
}

// Extract reduces one run to its trend samples. A nil run, or one without a
// collection time, yields a Run that contributes nothing.
func Extract(inv *inventory.Inventory) Run {
	if inv == nil || inv.CollectedAt.IsZero() {
		return Run{}
	}
	return Run{Host: inv.Host.Hostname, Collected: true, Samples: Samples(inv)}
}

// BuildRuns is Build over runs that have already been extracted.
func BuildRuns(runs []Run, now time.Time) *Report {
	var host string
	var samples []Sample
	for _, r := range runs {
		if !r.Collected {
			continue
		}
		if host == "" {
			host = r.Host
		}
		samples = append(samples, r.Samples...)
	}
	rep := Assemble(samples, now)
	rep.Host = host
	rep.Runs = len(runs)
	return rep
}

// Reader is the part of the store the cache reads through.
type Reader interface {
	RunEpoch(host string) uint64
	At(host, id string) (*inventory.Inventory, error)
}

// Cache keeps each host's extracted runs, so a request parses only the runs it
// has not seen. Entries leave when their run is pruned, and a host's entries
// are dropped whenever the store's run epoch moves, because a pruned run's ID
// can be reused by a later one.
type Cache struct {
	mu    sync.Mutex
	hosts map[string]*hostRuns
	bytes int
	clock uint64
}

type hostRuns struct {
	epoch uint64
	runs  map[string]Run
	bytes int
	used  uint64
}

// Report builds host's trend from the sampled run IDs, which are read oldest to
// newest in the order given. live is every stored run ID for host; cached runs
// missing from it are forgotten. onErr, when non-nil, is told about each sampled
// run that cannot be read; such a run is left out of the report and retried next time.
func (c *Cache) Report(r Reader, host string, live, sampled []string, now time.Time, onErr func(id string, err error)) (*Report, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hosts == nil {
		c.hosts = map[string]*hostRuns{}
	}
	c.clock++
	h := c.hosts[host]
	epoch := r.RunEpoch(host)
	if h == nil || h.epoch != epoch {
		c.drop(host)
		h = &hostRuns{epoch: epoch, runs: map[string]Run{}, bytes: 256 + len(host)}
		c.bytes += h.bytes
		c.hosts[host] = h
	}

	h.used = c.clock
	// Eviction affects only later requests, never the report being assembled.
	defer c.trim()
	stored := make(map[string]bool, len(live))
	for _, id := range live {
		stored[id] = true
	}
	for id := range h.runs {
		if !stored[id] {
			weight := runBytes(id, h.runs[id])
			h.bytes -= weight
			c.bytes -= weight
			delete(h.runs, id)
		}
	}

	var runs []Run
	for _, id := range sampled {
		run, ok := h.runs[id]
		if !ok {
			inv, err := r.At(host, id)
			if err != nil {
				if onErr != nil {
					onErr(id, err)
				}
				continue
			}
			run = Extract(inv)
			h.runs[id] = run
			weight := runBytes(id, run)
			h.bytes += weight
			c.bytes += weight
		}
		runs = append(runs, run)
	}
	if len(runs) == 0 {
		return nil, ErrNoRuns
	}
	return BuildRuns(runs, now), nil
}

// Budget includes sample backing arrays, strings and conservative map overhead.
// Large reports are still returned in full; only reuse is sacrificed.
const maxCacheBytes = 32 << 20
const maxCacheHosts = 64

func runBytes(id string, r Run) int {
	n := 256 + len(id) + len(r.Host) + cap(r.Samples)*128
	for _, s := range r.Samples {
		n += len(s.Kind) + len(s.Subject) + len(s.Type)
	}
	return n
}
func (c *Cache) drop(host string) {
	if h := c.hosts[host]; h != nil {
		c.bytes -= h.bytes
		delete(c.hosts, host)
	}
}
func (c *Cache) trim() {
	for c.bytes > maxCacheBytes || len(c.hosts) > maxCacheHosts {
		var oldest string
		var used uint64 = ^uint64(0)
		for host, h := range c.hosts {
			if h.used < used {
				oldest, used = host, h.used
			}
		}
		c.drop(oldest)
	}
}

// Reset releases entries after retention maintenance, including partial failures.
func (c *Cache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hosts = nil
	c.bytes = 0
}

// Forget releases a deleted host without waiting for another trend request.
func (c *Cache) Forget(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop(host)
}
