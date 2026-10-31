package plugin

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
)

type bridgeMedia struct {
	active      atomic.Int32
	overlap     atomic.Bool
	reads       atomic.Int32
	refreshes   atomic.Int32
	configured  atomic.Int32
	closed      atomic.Int32
	entered     chan struct{}
	release     chan struct{}
	failRefresh bool
}

func (p *bridgeMedia) enter(block bool) func() {
	if p.active.Add(1) != 1 {
		p.overlap.Store(true)
	}
	if block && p.entered != nil {
		close(p.entered)
		<-p.release
	}
	return func() { p.active.Add(-1) }
}
func (*bridgeMedia) Manifest() Manifest {
	return Manifest{ID: "media", Name: "Synthetic media", Settings: []Field{{Key: "label", Type: Text}}}
}
func (p *bridgeMedia) Configure(Config) error {
	defer p.enter(false)()
	p.configured.Add(1)
	return nil
}
func (*bridgeMedia) Test(context.Context) TestResult                        { return TestResult{OK: true} }
func (*bridgeMedia) View(context.Context, string, url.Values) (View, error) { return View{}, nil }
func (p *bridgeMedia) Close() error                                         { defer p.enter(false)(); p.closed.Add(1); return nil }
func (p *bridgeMedia) TVCapabilities() mediadata.Capabilities {
	defer p.enter(false)()
	return mediadata.Capabilities{Media: true, Discover: true, Requests: true}
}
func (p *bridgeMedia) TVRead(_ context.Context, op, kind string, id int, q string) (any, error) {
	defer p.enter(true)()
	p.reads.Add(1)
	if op == "refresh" {
		p.refreshes.Add(1)
		if p.failRefresh {
			return nil, errors.New("synthetic failure")
		}
	}
	return mediadata.Discovery{Status: "ok", Titles: []mediadata.Title{{ID: 42, Kind: "movie", Title: op}}}, nil
}
func (p *bridgeMedia) TVRequest(context.Context, mediadata.RequestPolicy, mediadata.Request) (mediadata.RequestResult, error) {
	defer p.enter(true)()
	return mediadata.RequestResult{State: "pending"}, nil
}
func (p *bridgeMedia) TVOptions(context.Context) (mediadata.Options, error) {
	defer p.enter(true)()
	return mediadata.Options{Requesters: []mediadata.Choice{{ID: 12, Name: "Synthetic requester"}}}, nil
}
func (p *bridgeMedia) TVProfiles(context.Context) ([]mediadata.Choice, error) {
	defer p.enter(true)()
	return []mediadata.Choice{{ID: 17, Name: "Synthetic viewer"}}, nil
}
func (p *bridgeMedia) TVHistory(context.Context, int) ([]mediadata.Title, error) {
	defer p.enter(true)()
	return []mediadata.Title{{ID: 42, Kind: "movie", Title: "Synthetic film"}}, nil
}

type bridgeDusk struct{}

func (*bridgeDusk) Manifest() Manifest {
	return Manifest{ID: "dusk", Name: "Synthetic TV", Settings: []Field{{Key: "server_name", Type: Text}, {Key: "discovery", Type: Bool}, {Key: "taste", Type: Bool}, {Key: "requests", Type: Bool}, {Key: "private_key", Type: Secret}}}
}
func (*bridgeDusk) Configure(Config) error                                 { return nil }
func (*bridgeDusk) Test(context.Context) TestResult                        { return TestResult{OK: true} }
func (*bridgeDusk) View(context.Context, string, url.Values) (View, error) { return View{}, nil }
func (*bridgeDusk) Close() error                                           { return nil }
func newMediaBridge(t *testing.T) (*Registry, *bridgeMedia) {
	t.Helper()
	p := &bridgeMedia{}
	r, err := Open(t.TempDir(), []Builtin{{ID: "media", New: func(Host) Plugin { return p }}, {ID: "dusk", New: func(Host) Plugin { return &bridgeDusk{} }}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	on := true
	if _, err := r.Update("media", Update{Enabled: &on, Settings: Config{"label": "first"}}); err != nil {
		t.Fatal(err)
	}
	return r, p
}

func TestMediaBridgeSerializesLifecycle(t *testing.T) {
	calls := map[string]func(*Registry) error{
		"read": func(r *Registry) error { _, e := r.MediaRead(context.Background(), "discover", "", 0, ""); return e },
		"request": func(r *Registry) error {
			_, e := r.MediaRequest(context.Background(), mediadata.RequestPolicy{}, mediadata.Request{})
			return e
		},
		"options":  func(r *Registry) error { _, e := r.MediaOptions(context.Background()); return e },
		"profiles": func(r *Registry) error { _, e := r.MediaProfiles(context.Background()); return e },
		"history":  func(r *Registry) error { _, e := r.MediaHistory(context.Background(), 17); return e },
	}
	for name, call := range calls {
		for _, lifecycle := range []string{"configure", "disable", "close"} {
			t.Run(name+"/"+lifecycle, func(t *testing.T) {
				r, p := newMediaBridge(t)
				p.entered = make(chan struct{})
				p.release = make(chan struct{})
				done := make(chan error, 1)
				go func() { done <- call(r) }()
				<-p.entered
				if r.entries["media"].op.TryLock() {
					r.entries["media"].op.Unlock()
					t.Fatal("typed operation does not hold lifecycle lock")
				}
				started := make(chan struct{})
				finished := make(chan error, 1)
				go func() {
					close(started)
					switch lifecycle {
					case "configure":
						_, e := r.Update("media", Update{Settings: Config{"label": "second"}})
						finished <- e
					case "disable":
						off := false
						_, e := r.Update("media", Update{Enabled: &off})
						finished <- e
					default:
						r.Close()
						finished <- nil
					}
				}()
				<-started
				select {
				case <-finished:
					t.Fatal("lifecycle completed while Media call blocked")
				default:
				}
				close(p.release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if err := <-finished; err != nil {
					t.Fatal(err)
				}
				if p.overlap.Load() {
					t.Fatal("lifecycle overlapped typed call")
				}
			})
		}
	}
}
func TestMediaRefreshCoalescesAndInvalidates(t *testing.T) {
	r, p := newMediaBridge(t)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := r.MediaRead(context.Background(), "refresh", "", 0, "")
			errorsCh <- e
		}()
	}
	wg.Wait()
	close(errorsCh)
	for e := range errorsCh {
		if e != nil {
			t.Fatal(e)
		}
	}
	if p.refreshes.Load() != 1 {
		t.Fatal("concurrent refreshes were not coalesced")
	}
	// A cached result is served throughout the thirty-second window. Advancing
	// the cache timestamp avoids introducing wall-clock sleeps into this test.
	e := r.entries["media"]
	e.op.Lock()
	e.mediaRefresh.at = e.host.Now().Add(-29 * time.Second)
	e.op.Unlock()
	r.MediaRead(context.Background(), "refresh", "", 0, "")
	if p.refreshes.Load() != 1 {
		t.Fatal("refresh expired before thirty seconds")
	}
	e.op.Lock()
	e.mediaRefresh.at = e.host.Now().Add(-31 * time.Second)
	e.op.Unlock()
	r.MediaRead(context.Background(), "refresh", "", 0, "")
	if p.refreshes.Load() != 2 {
		t.Fatal("expired refresh retained")
	}
	if _, err := r.Update("media", Update{Settings: Config{"label": "changed"}}); err != nil {
		t.Fatal(err)
	}
	r.MediaRead(context.Background(), "refresh", "", 0, "")
	if p.refreshes.Load() != 3 {
		t.Fatal("reconfiguration retained stale refresh")
	}
	p.failRefresh = true
	e.op.Lock()
	e.mediaRefresh = nil
	e.op.Unlock()
	for i := 0; i < 2; i++ {
		if _, err := r.MediaRead(context.Background(), "refresh", "", 0, ""); err == nil {
			t.Fatal("failed refresh reported success")
		}
	}
	if p.refreshes.Load() != 5 {
		t.Fatal("failed refresh cached as success")
	}
}
func TestMediaBridgeDisabledSourceAndTypedDuskSettings(t *testing.T) {
	r, _ := newMediaBridge(t)
	on := true
	if _, err := r.Update("dusk", Update{Enabled: &on, Settings: Config{"server_name": "Household", "discovery": true, "taste": false, "requests": true, "private_key": "synthetic-secret"}}); err != nil {
		t.Fatal(err)
	}
	settings, enabled := r.DuskSettings()
	if !enabled || settings.ServerName != "Household" || !settings.Discovery || settings.Taste || !settings.Requests {
		t.Fatal("typed TV configuration mismatch")
	}
	off := false
	r.Update("media", Update{Enabled: &off})
	if r.MediaCapabilities().Media {
		t.Fatal("disabled source advertised capability")
	}
	if _, e := r.MediaRead(context.Background(), "refresh", "", 0, ""); !errors.Is(e, ErrNotFound) {
		t.Fatal("disabled reads accepted")
	}
	if _, e := r.MediaRequest(context.Background(), mediadata.RequestPolicy{}, mediadata.Request{}); !errors.Is(e, ErrNotFound) {
		t.Fatal("disabled requests accepted")
	}
	if _, e := r.MediaOptions(context.Background()); !errors.Is(e, ErrNotFound) {
		t.Fatal("disabled options accepted")
	}
	if _, e := r.MediaHistory(context.Background(), 17); !errors.Is(e, ErrNotFound) {
		t.Fatal("disabled history accepted")
	}
	if profiles, e := r.MediaProfiles(context.Background()); e != nil || len(profiles) != 0 {
		t.Fatal("disabled profile choices advertised")
	}
	r.Update("dusk", Update{Enabled: &off})
	if _, enabled := r.DuskSettings(); enabled {
		t.Fatal("disabled TV remains enabled")
	}
}
