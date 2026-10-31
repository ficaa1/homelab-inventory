package letterboxd

import (
	"context"
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
	"path/filepath"
	"testing"
	"time"
)

type syntheticRemote struct{ fail bool }

func (s syntheticRemote) Feed(context.Context) ([]Observation, error) { return []Observation{}, nil }
func (s syntheticRemote) Collection(_ context.Context, kind string) Scan {
	if s.fail {
		return Scan{Err: errors.New("offline")}
	}
	return Scan{Complete: true, Rows: []Observation{}, Total: 0}
}

type catalog struct{}

func (catalog) Search(context.Context, string) ([]tastedata.BrowseTitle, error) { return nil, nil }
func (catalog) Details(context.Context, string, int) (tastedata.TitleDetail, error) {
	return tastedata.TitleDetail{}, nil
}
func TestWatchlistRequiresCompleteScanAndOwnerBinding(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "profile.json")
	stars := 4.5
	p := Profile{Owner: 7, Version: 1, ImportedAt: now, Films: []Film{{Title: "Synthetic film", Year: "2020", Watched: true, Watchlist: true, Rating: &stars, Match: &tastedata.BrowseTitle{ID: 4, Kind: "movie", Title: "Synthetic film", Year: "2020"}}}}
	if e := Save(path, p); e != nil {
		t.Fatal(e)
	}
	s := Syncer{Path: path, Owner: 7, Username: "fixture", Catalog: catalog{}, Source: syntheticRemote{fail: true}}
	_ = s.Step(context.Background(), now)
	got, e := Load(path)
	if e != nil || !got.Films[0].Watchlist || got.Films[0].Rating == nil || *got.Films[0].Rating != stars {
		t.Fatalf("failed scan erased preferences: %#v %v", got, e)
	}
	s.Source = syntheticRemote{}
	if e = s.Step(context.Background(), now.Add(48*time.Hour)); e != nil {
		t.Fatal(e)
	}
	got, e = Load(path)
	if e != nil || got.Films[0].Watchlist {
		t.Fatalf("complete empty watchlist not applied: %#v %v", got, e)
	}
	s.Owner = 8
	if e = s.Step(context.Background(), now.Add(96*time.Hour)); e == nil {
		t.Fatal("cross-owner sync accepted")
	}
}
