package tastedata

import (
	"context"
	"time"
)

type WatchTitle struct {
	Kind, Title, Key       string
	Year, Percent, Seconds int
	// Season and Episode identify a series play; zero for films.
	Season, Episode int
	At              time.Time
}

type SuggestedTitle struct {
	BrowseTitle
	Rating       float64
	Votes        int
	Adult        bool
	Date         string
	Availability string
}

type BrowseTitle struct {
	MatchNames []string `json:"-"`
	ID         int      `json:"id"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Year       string   `json:"year"`
	Overview   string   `json:"overview"`
	Poster     string   `json:"poster"`
}

type TastePerson struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// TitleFacts is the public TMDB metadata a taste model weighs: who made a
// title, what it is about, where it is from and how it was received.
type TitleFacts struct {
	SuggestedTitle
	People    []TastePerson `json:"people"`
	Keywords  []string      `json:"keywords"`
	Genres    []string      `json:"genres"`
	Countries []string      `json:"countries"`
	Language  string        `json:"language"`
	// Episodes counts a series' regular episodes aired so far.
	Episodes int `json:"episodes,omitempty"`
}

type TitleDetail struct{ BrowseTitle }

// Source is the narrow read-only recommendation seam; it exposes no service keys.
type Source interface {
	WatchHistory(context.Context, int) ([]WatchTitle, error)
	WatchIdentity(context.Context, WatchTitle) (BrowseTitle, error)
	Recommendations(context.Context, string, int) ([]SuggestedTitle, error)
	RecommendationsPage(context.Context, string, int, int) ([]SuggestedTitle, error)
	RecommendationAvailability(context.Context) (map[string]string, error)
	TitleFacts(context.Context, string, int) (TitleFacts, error)
	PersonWork(context.Context, int) ([]SuggestedTitle, error)
	Search(context.Context, string) ([]BrowseTitle, error)
	Details(context.Context, string, int) (TitleDetail, error)
}
