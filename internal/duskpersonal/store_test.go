package duskpersonal

import (
	"context"
	"errors"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/mediadata"
	"path/filepath"
	"strings"
	"testing"
)

func title(id int, name string) mediadata.Title {
	return mediadata.Title{ID: id, Kind: "movie", Title: name, Year: "2020"}
}
func TestFeedbackIsolationRestart(t *testing.T) {
	dir := t.TempDir()
	s, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetHidden("a", "movie", 2, true); e != nil {
		t.Fatal(e)
	}
	if e = s.Verdict("a", "movie", 1, "loved", "Seed", "2020"); e != nil {
		t.Fatal(e)
	}
	s, e = New(dir)
	if e != nil {
		t.Fatal(e)
	}
	r := mediadata.Discovery{Titles: []mediadata.Title{title(1, "Seed"), title(2, "Hidden"), title(3, "Other")}}
	calls := 0
	sim := func(ctx context.Context, kind string, id int) (mediadata.Discovery, error) {
		calls++
		if id != 1 || kind != "movie" {
			t.Fatal("wrong seed")
		}
		return mediadata.Discovery{Titles: []mediadata.Title{title(4, "Similar"), title(2, "Hidden"), title(4, "Duplicate")}}, nil
	}
	out, e := s.Apply(context.Background(), "a", r, sim)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 1 || len(out.Titles) != 2 || out.Titles[0].ID != 4 {
		t.Fatalf("unexpected output %+v", out)
	}
	out, e = s.Apply(context.Background(), "b", r, sim)
	if e != nil || len(out.Titles) != 3 || calls != 1 {
		t.Fatalf("profile leak %+v %v", out, e)
	}
	if e = s.Verdict("a", "movie", 1, "undo", "", ""); e != nil {
		t.Fatal(e)
	}
	if e = s.SetHidden("a", "movie", 2, false); e != nil {
		t.Fatal(e)
	}
	out, e = s.Apply(context.Background(), "a", r, nil)
	if e != nil || len(out.Titles) != 3 {
		t.Fatal(out, e)
	}
}
func TestImportAndRecommendation(t *testing.T) {
	s, _ := New(t.TempDir())
	result, e := s.Import("p", "Date,Name,Year,Letterboxd URI,Rating\n2026-01-01,Seed,2020,https://example.invalid,5\n")
	if e != nil || result["imported"] != 1 {
		t.Fatal(result, e)
	}
	seed := title(1, "Seed")
	seed.Directors = []string{"Synthetic Director"}
	related := title(2, "Related")
	related.Directors = seed.Directors
	r := mediadata.Discovery{Titles: []mediadata.Title{title(3, "Unrelated"), related, seed}}
	out, e := s.Apply(context.Background(), "p", r, nil)
	if e != nil || len(out.Titles) != 2 || out.Titles[0].ID != 2 || len(out.Seeds) != 1 {
		t.Fatal(out, e)
	}
	got := s.Taste("p", "movie", r, false).(map[string]any)
	if got["judged"].(int) != 1 || len(got["picks"].([]mediadata.Title)) != 2 {
		t.Fatal(got)
	}
	s, e = New(filepath.Dir(s.path))
	if e != nil {
		t.Fatal(e)
	}
	got = s.Taste("p", "movie", r, true).(map[string]any)
	if got["judged"].(int) != 1 {
		t.Fatal(got)
	}
}
func TestValidationBoundsAndFailures(t *testing.T) {
	s, _ := New(t.TempDir())
	for _, input := range []string{"Name,Year,Rating\nX,2020,NaN", "Name,Year,Rating\nX,2020,6", "Other\nX", strings.Repeat("x", 65537), "Name,Year\n" + strings.Repeat("Same,2020\n", 501)} {
		if _, e := s.Import("p", input); !errors.Is(e, ErrInvalid) {
			t.Fatalf("accepted invalid CSV %v", e)
		}
	}
	if e := s.Verdict("p", "movie", 1, "bad", "Title", "2020"); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	if e := s.SetHidden("", "movie", 1, true); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	for i := 1; i <= 5; i++ {
		if e := s.Verdict("p", "movie", i, "liked", fmt.Sprint(i), "2020"); e != nil {
			t.Fatal(e)
		}
	}
	calls := 0
	out, e := s.Apply(context.Background(), "p", mediadata.Discovery{}, func(context.Context, string, int) (mediadata.Discovery, error) {
		calls++
		return mediadata.Discovery{}, errors.New("offline")
	})
	if e != nil || calls != 3 || !out.Partial {
		t.Fatal(calls, out, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.Apply(ctx, "p", mediadata.Discovery{}, func(context.Context, string, int) (mediadata.Discovery, error) {
		t.Fatal("canceled callback")
		return mediadata.Discovery{}, nil
	}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestConcurrentWrites(t *testing.T) {
	s, _ := New(t.TempDir())
	done := make(chan error, 20)
	for i := 1; i <= 20; i++ {
		go func(id int) { done <- s.SetHidden("p", "movie", id, true) }(i)
	}
	for i := 0; i < 20; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	s.mu.Lock()
	n := len(s.data["p"].Hidden)
	s.mu.Unlock()
	if n != 20 {
		t.Fatal(n)
	}
}

func TestWriteFailureRollsBack(t *testing.T) {
	s, _ := New(t.TempDir())
	if err := s.SetHidden("p", "movie", 1, true); err != nil {
		t.Fatal(err)
	}
	original := s.path
	s.path = filepath.Join(t.TempDir(), "missing", "personal.json")
	if err := s.SetHidden("p", "movie", 2, true); err == nil {
		t.Fatal("expected write error")
	}
	s.path = original
	s.mu.Lock()
	n := len(s.data["p"].Hidden)
	s.mu.Unlock()
	if n != 1 {
		t.Fatal("failed write changed memory")
	}
}
