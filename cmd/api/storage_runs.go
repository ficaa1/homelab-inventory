package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/store"
)

func retentionEnv() (store.Policy, error) {
	p := store.DefaultPolicy()
	for key, dst := range map[string]*int{
		"RUN_KEEP_ALL_DAYS": &p.KeepAllDays, "RUN_HOURLY_DAYS": &p.HourlyDays,
		"RUN_DAILY_DAYS": &p.DailyDays, "RUN_COMPRESS_AFTER_DAYS": &p.CompressAfterDays,
	} {
		if v, ok := os.LookupEnv(key); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return p, fmt.Errorf("%s must be an integer", key)
			}
			*dst = n
		}
	}
	return p, p.Validate()
}

type pruneState struct {
	Last  *store.PruneResult `json:"lastPrune,omitempty"`
	Error string             `json:"error,omitempty"`
}

func (s *server) handleRunsStorage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.store.Usage(time.Now())
	if err != nil {
		writeError(w, 500, "reading run storage")
		return
	}
	var state pruneState
	if err := s.store.ReadState("run-prune", &state); err != nil {
		writeError(w, 500, "reading prune state")
		return
	}
	s.scheduleMu.Lock()
	next := s.nextPrune
	s.scheduleMu.Unlock()
	writeJSON(w, 200, struct {
		store.Usage
		Policy store.Policy `json:"policy"`
		pruneState
		Next *time.Time `json:"nextPrune,omitempty"`
	}{usage, s.effective().RunRetention, state, timePointer(next)})
}
func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
func (s *server) handlePruneRuns(w http.ResponseWriter, r *http.Request) {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		writeError(w, 415, "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var in struct {
		DryRun *bool `json:"dryRun"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.DryRun == nil {
		writeError(w, 400, "expected {\"dryRun\":true} or {\"dryRun\":false}")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "expected one JSON object")
		return
	}
	// A first pass over years of history can exceed the API's usual 30s deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Hour))
	result, err := s.pruneRuns(r.Context(), *in.DryRun)
	if err != nil {
		writeError(w, 500, "pruning runs: "+err.Error())
		return
	}
	writeJSON(w, 200, result)
}
func (s *server) pruneRuns(ctx context.Context, dryRun bool) (store.PruneResult, error) {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	result, err := s.store.PruneContext(ctx, s.effective().RunRetention, time.Now(), dryRun)
	if !dryRun {
		s.trends.Reset()
		// Persist a compact summary; the potentially large action list is returned
		// only to the requesting caller, rather than on every dashboard refresh.
		summary := result
		summary.Actions = nil
		state := pruneState{Last: &summary}
		if err != nil {
			state.Error = err.Error()
		}
		if saveErr := s.store.WriteState("run-prune", state); saveErr != nil && err == nil {
			err = saveErr
		}
		s.log.Printf("run prune: deleted=%d compressed=%d saved=%d bytes error=%v", result.FilesDeleted, result.FilesCompressed, result.BytesSaved, err)
	}
	return result, err
}
func (s *server) handleForgetHost(w http.ResponseWriter, r *http.Request) {
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	err := s.store.ForgetHost(r.PathValue("host"), time.Now())
	switch {
	case errors.Is(err, store.ErrActiveHost):
		writeError(w, 409, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "host not found")
	case err != nil:
		writeError(w, 400, err.Error())
	default:
		s.trends.Forget(r.PathValue("host"))
		writeJSON(w, 200, map[string]bool{"forgotten": true})
	}
}
func (s *server) pruneLoop(ctx context.Context) {
	next := time.Now().Add(10 * time.Minute)
	for {
		s.scheduleMu.Lock()
		s.nextPrune = next
		s.scheduleMu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		_, _ = s.pruneRuns(ctx, false)
		next = time.Now().Add(24 * time.Hour)
	}
}
