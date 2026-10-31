package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaintenanceDrainsRequestsAndWorkers(t *testing.T) {
	var generations atomic.Int32
	var running atomic.Int32
	closed := make(chan struct{}, 8)
	l, err := newApplicationLifecycle(context.Background(), func() (*applicationRuntime, error) {
		generations.Add(1)
		rt := &applicationRuntime{server: testServer(t, "fixture-admin")}
		rt.workers = func(ctx context.Context) {
			// Count synchronously so maintenance cannot win the worker start race.
			running.Add(1)
			rt.launch(ctx, func(ctx context.Context) { defer running.Add(-1); <-ctx.Done() })
		}
		rt.close = func() { closed <- struct{}{} }
		return rt, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.shutdown()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	old := l.active
	old.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(204) })
	go func() {
		defer close(done)
		l.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/blocked", nil))
	}()
	<-entered
	result := make(chan error, 1)
	action := make(chan struct{})
	go func() {
		result <- l.maintenance(context.Background(), func() error {
			if running.Load() != 0 {
				return errors.New("workers still running")
			}
			select {
			case <-closed:
			default:
				return errors.New("resources not closed")
			}
			close(action)
			return nil
		})
	}()
	deadline := time.After(time.Second)
	for {
		l.mu.Lock()
		paused := l.paused
		l.mu.Unlock()
		if paused {
			break
		}
		select {
		case <-deadline:
			t.Fatal("maintenance did not pause")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	rec := httptest.NewRecorder()
	l.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Fatalf("new request admitted during drain: %d", rec.Code)
	}
	select {
	case <-action:
		t.Fatal("snapshot ran before request finished")
	default:
	}
	close(release)
	<-done
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if l.active == old || generations.Load() != 2 || running.Load() != 1 {
		t.Fatal("did not reopen fresh runtime")
	}
	if l.active.server.backup != old.server.backup {
		t.Fatal("staged plan owner lost")
	}
	rec = httptest.NewRecorder()
	l.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("reopened health=%d", rec.Code)
	}
}

func TestMaintenanceReloadAndFailure(t *testing.T) {
	fail := false
	opens := 0
	l, err := newApplicationLifecycle(context.Background(), func() (*applicationRuntime, error) {
		opens++
		if fail {
			return nil, errors.New("bad installed state")
		}
		return &applicationRuntime{server: testServer(t, "fixture-admin")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.shutdown()
	old := l.active
	err = l.maintenance(context.Background(), func() error {
		fail = true
		if e := l.reload(); e == nil {
			return errors.New("invalid state accepted")
		}
		if l.active != old {
			return errors.New("invalid runtime published")
		}
		// The transaction rolls back files before reopening the original state.
		fail = false
		return l.reload()
	})
	if err != nil || opens != 3 || l.active == old || l.paused {
		t.Fatalf("rollback reopen: err=%v opens=%d", err, opens)
	}
	fail = true
	err = l.maintenance(context.Background(), func() error { return nil })
	if err == nil || !l.paused {
		t.Fatal("failed reopen exposed stale state")
	}
}

func TestCancelledMaintenanceLeavesActiveRuntime(t *testing.T) {
	l, err := newApplicationLifecycle(context.Background(), func() (*applicationRuntime, error) {
		return &applicationRuntime{server: testServer(t, "fixture-admin")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.shutdown()
	old := l.active
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = l.maintenance(ctx, func() error { t.Error("cancelled action executed"); return nil })
	if !errors.Is(err, context.Canceled) || l.paused || l.active != old || old.stopped {
		t.Fatal("cancelled operation changed running state")
	}
}
