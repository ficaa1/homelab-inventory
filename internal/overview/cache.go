package overview

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

const storageCacheTTL = 30 * time.Second

// StorageCache holds one immutable storage aggregation for one builder/store.
// The zero value is ready to use. Refreshes run in the requesting goroutine;
// canceling it stops its reads, and surviving waiters can retry the refresh.
type StorageCache struct {
	mu         sync.Mutex
	key        string
	value      any
	expires    time.Time
	pending    chan struct{}
	generation uint64
}

// Invalidate prevents even an in-flight refresh from publishing pre-ingest data.
func (c *StorageCache) Invalidate() {
	c.mu.Lock()
	c.generation++
	c.value = nil
	c.mu.Unlock()
}

func (b Builder) cachedStorage(ctx context.Context, invs []*inventory.Inventory, now time.Time) any {
	if b.Cache == nil {
		return b.storage(ctx, invs, now)
	}
	var key strings.Builder
	for _, inv := range invs {
		key.WriteString(strconv.FormatUint(runEpoch(b.Store, inv.Host.Hostname), 10))
		key.WriteByte(0)
		key.WriteString(inv.Host.Hostname)
		key.WriteByte(0)
		key.WriteString(inv.CollectedAt.UTC().Format(time.RFC3339Nano))
		key.WriteByte(0)
	}
	return b.Cache.load(ctx, key.String(), now, func() any { return b.storage(ctx, invs, now) })
}

func (c *StorageCache) load(ctx context.Context, key string, now time.Time, refresh func() any) any {
	for {
		if ctx.Err() != nil {
			return unavailable("request canceled")
		}
		c.mu.Lock()
		if c.value != nil && c.key == key && now.Before(c.expires) {
			value := cloneStorage(c.value)
			c.mu.Unlock()
			return value
		}
		if c.pending != nil {
			done := c.pending
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return unavailable("request canceled")
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		c.pending = done
		generation := c.generation
		c.mu.Unlock()

		value := refresh()
		c.mu.Lock()
		// Source failures and canceled partial results must not become cache hits.
		if _, ok := value.([]Storage); ok && ctx.Err() == nil && generation == c.generation {
			c.value, c.key, c.expires = cloneStorage(value), key, now.Add(storageCacheTTL)
		}
		c.pending = nil
		close(done)
		c.mu.Unlock()
		return value
	}
}

func cloneStorage(value any) any {
	rows, ok := value.([]Storage)
	if !ok {
		return value
	}
	out := make([]Storage, len(rows))
	for i, row := range rows {
		out[i] = row
		out[i].UsedBytes = clonePointer(row.UsedBytes)
		out[i].SizeBytes = clonePointer(row.SizeBytes)
		out[i].Pct = clonePointer(row.Pct)
		out[i].Delta24hBytes = clonePointer(row.Delta24hBytes)
		out[i].Forecast.FullAt = clonePointer(row.Forecast.FullAt)
		if row.Spark != nil {
			out[i].Spark = append([]float64{}, row.Spark...)
		}
	}
	return out
}
func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	value := *p
	return &value
}
