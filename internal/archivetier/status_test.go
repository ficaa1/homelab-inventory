package archivetier

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusAuthHealthAndStaleness(t *testing.T) {
	r, _, _, now := runner(t)
	handler := StatusHandler(r.Store.Dir, "token", 15*time.Minute, func() time.Time { return now })
	req := func(path, token string) *httptest.ResponseRecorder {
		q := httptest.NewRequest(http.MethodGet, path, nil)
		q.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, q)
		return w
	}
	if w := req("/status", "wrong"); w.Code != 401 {
		t.Fatal("private report exposed")
	}
	if w := req("/healthz", "token"); w.Code != 503 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("missing state healthy")
	}
	if e := r.Store.Save(State{Checked: now}); e != nil {
		t.Fatal(e)
	}
	if w := req("/healthz", "token"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("fresh state unhealthy")
	}
	now = now.Add(16 * time.Minute)
	if req("/healthz", "token").Code != 503 {
		t.Fatal("stale state healthy")
	}
	if e := r.Store.Save(State{Checked: now, Error: "archive failed"}); e != nil {
		t.Fatal(e)
	}
	if req("/healthz", "token").Code != 503 {
		t.Fatal("failed state healthy")
	}
	if req("/status", "token").Code != 200 {
		t.Fatal("failed report inaccessible")
	}
}

func TestFailedPhaseHealthWithoutTransientError(t *testing.T) {
	r, _, _, now := runner(t)
	if e := r.Store.Save(State{Checked: now, Job: &Job{Phase: Failed, Message: "unresolved download lifecycle"}}); e != nil {
		t.Fatal(e)
	}
	h := StatusHandler(r.Store.Dir, "token", time.Hour, func() time.Time { return now })
	q := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	q.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, q)
	if w.Code != 503 {
		t.Fatal("persisted failure healthy", w.Code)
	}
}
