package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// applicationRuntime owns all caches and background producers for one dataset.
// Constructing a runtime must be inert: workers start only after installation
// has committed and the maintenance gate can reopen.
type applicationRuntime struct {
	server   *server
	handler  http.Handler
	workers  func(context.Context)
	validate func() error
	close    func()
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopped  bool
}

func (rt *applicationRuntime) launch(ctx context.Context, work func(context.Context)) {
	rt.wg.Add(1)
	go func() { defer rt.wg.Done(); work(ctx) }()
}
func (rt *applicationRuntime) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	rt.cancel = cancel
	if rt.workers != nil {
		rt.workers(ctx)
	}
}
func (rt *applicationRuntime) stop() {
	if rt.stopped {
		return
	}
	rt.stopped = true
	if rt.cancel != nil {
		rt.cancel()
	}
	rt.wg.Wait()
	if rt.close != nil {
		rt.close()
	}
}

// The listener survives restore. Only /admin/backup/ bypasses admission so a
// restore request can drain all other handlers without waiting on itself.
// Backup handlers serialize their operations with the shared backupAdmin.
type applicationLifecycle struct {
	operation      sync.Mutex
	backupRequests sync.Mutex
	mu             sync.Mutex
	active         *applicationRuntime
	paused         bool
	inflight       int
	drained        chan struct{}
	factory        func() (*applicationRuntime, error)
	parent         context.Context
}

func newApplicationLifecycle(ctx context.Context, factory func() (*applicationRuntime, error)) (*applicationLifecycle, error) {
	l := &applicationLifecycle{parent: ctx, factory: factory, paused: true}
	if err := l.open(false); err != nil {
		return nil, err
	}
	l.active.start(ctx)
	l.paused = false
	return l, nil
}
func (l *applicationLifecycle) reload() error { return l.open(true) }
func (l *applicationLifecycle) open(validate bool) error {
	rt, err := l.factory()
	if err != nil {
		return fmt.Errorf("reopen application state: %w", err)
	}
	if validate && rt.validate != nil {
		if err := rt.validate(); err != nil {
			rt.stop()
			return fmt.Errorf("validate restored state: %w", err)
		}
	}
	l.mu.Lock()
	previous := l.active
	if l.active != nil {
		rt.server.backup = l.active.server.backup
	}
	rt.server.backupMaintenance = l.maintenance
	rt.server.backupReload = l.reload
	rt.handler = rt.server.routes()
	l.active = rt
	l.mu.Unlock()
	if previous != nil {
		previous.stop()
	}
	return nil
}
func (l *applicationLifecycle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bypass := strings.HasPrefix(r.URL.Path, "/admin/backup/")
	if bypass {
		l.backupRequests.Lock()
		defer l.backupRequests.Unlock()
	}
	l.mu.Lock()
	rt := l.active
	if l.paused && !bypass {
		l.mu.Unlock()
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Dashboard maintenance is in progress. Retry shortly.", http.StatusServiceUnavailable)
		return
	}
	if !bypass {
		if l.inflight == 0 {
			l.drained = make(chan struct{})
		}
		l.inflight++
	}
	l.mu.Unlock()
	if !bypass {
		defer func() {
			l.mu.Lock()
			l.inflight--
			if l.inflight == 0 {
				close(l.drained)
			}
			l.mu.Unlock()
		}()
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(10 * time.Second))
		writeWindow := 30 * time.Second
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/plugins/tdarr/actions/") {
			writeWindow = 370 * time.Second
		}
		_ = rc.SetWriteDeadline(time.Now().Add(writeWindow))
		defer func() { _ = rc.SetReadDeadline(time.Time{}); _ = rc.SetWriteDeadline(time.Time{}) }()
	}
	rt.handler.ServeHTTP(w, r)
}
func (l *applicationLifecycle) maintenance(ctx context.Context, work func() error) error {
	l.operation.Lock()
	defer l.operation.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	l.paused = true
	drain, n, old := l.drained, l.inflight, l.active
	l.mu.Unlock()
	if n > 0 {
		select {
		case <-drain:
		case <-ctx.Done():
			l.mu.Lock()
			l.paused = false
			l.mu.Unlock()
			return ctx.Err()
		}
	}
	// No new requests can start writes. Join loops and independently started
	// plugin/model work before the snapshot or transaction touches files.
	old.stop()
	var err error
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		err = work()
	}
	l.mu.Lock()
	unchanged := l.active == old
	l.mu.Unlock()
	// Export also needs fresh managers because Close is final for plugins.
	if unchanged {
		err = errors.Join(err, l.open(false))
	}
	l.mu.Lock()
	fresh := l.active
	if fresh != old {
		fresh.start(l.parent)
		l.paused = false
	}
	l.mu.Unlock()
	// If reopening failed, remain unavailable instead of exposing stale caches.
	return err
}
func (l *applicationLifecycle) shutdown() {
	l.operation.Lock()
	defer l.operation.Unlock()
	l.mu.Lock()
	l.paused = true
	rt := l.active
	drain, n := l.drained, l.inflight
	l.mu.Unlock()
	if n > 0 {
		<-drain
	}
	if rt != nil {
		rt.stop()
	}
}
