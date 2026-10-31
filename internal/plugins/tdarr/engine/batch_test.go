package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPreviewRechecksLeaseAndConsumesToken(t *testing.T) {
	ctx := context.Background()
	lease := ""
	zero := 0
	inv := Inventory{Items: []LibraryItem{{Kind: "radarr", ID: 7, Title: "Example", Path: "/arr/films/example"}}, Leases: map[string]string{}}
	e := &Engine{Inventory: func(context.Context, bool) (Inventory, error) {
		inv.Leases[TitleKey("radarr", 7)] = lease
		return inv, nil
	}, Resolve: func(context.Context, Selection) (LibraryTitle, []LibraryFile, error) {
		return LibraryTitle{ID: 7, Path: "/arr/films/example"}, []LibraryFile{{ID: 9, Path: "/arr/films/example/movie.mkv", Size: 2 << 30}}, nil
	}, Candidates: func(context.Context) ([]ShrinkCandidate, error) {
		return []ShrinkCandidate{{Path: "/worker/films/example/movie.mkv", Kind: "video"}}, nil
	}, Status: func(context.Context) TdarrStatus {
		return TdarrStatus{Status: "ok", Queue: &TdarrQueueStatus{Queued: &zero, Processing: &zero}}
	}, Idle: func(context.Context) error { return nil }, Blocked: func(LibraryTitle) bool { return false }, MapPath: func(s, kind string) (string, error) { return strings.Replace(s, "/arr/", "/worker/", 1), nil }, Now: time.Now}
	preview, err := e.Preview(ctx, "operator", Request{Selection: []Selection{{Kind: "radarr", ID: 7}}})
	if err != nil || preview.Token == "" {
		t.Fatalf("preview %v", err)
	}
	lease = "archive"
	req := ExecuteRequest{Plan: preview.Plan, Hash: preview.Hash, Token: preview.Token, Confirm: true}
	if _, err = e.Recheck(ctx, "operator", req); !errors.Is(err, ErrConflict) {
		t.Fatalf("lease recheck not refused: %v", err)
	}
	lease = ""
	if _, err = e.Recheck(ctx, "operator", req); !errors.Is(err, ErrConflict) {
		t.Fatal("preview token reused")
	}
}
