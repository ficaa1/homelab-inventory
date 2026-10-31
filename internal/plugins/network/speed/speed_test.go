package speed

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestCloseCancelsNativeWorkerAndPersistsCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "history.jsonl")
	worker, e := NewSpeedtester(server.URL, server.URL+"/primary", server.URL+"/fallback", 1<<30, 1<<30, file, server.Client())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = worker.Start(time.Now()); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() { worker.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel requests")
	}
	if _, e = worker.Start(time.Now()); e == nil {
		t.Fatal("closed worker restarted")
	}
	history, e := openSpeedHistory(file)
	if e != nil || history.LatestStart().IsZero() {
		t.Fatalf("cooldown not persisted %v", e)
	}
}
