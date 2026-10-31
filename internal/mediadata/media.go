// Package mediadata defines the shared, credential-free TV media contract.
package mediadata

import "context"

type Capabilities struct{ Media, Upcoming, Browse, Discover, Search, Requests bool }
type Source interface {
	TVCapabilities() Capabilities
	TVRead(context.Context, string, string, int, string) (any, error)
	TVRequest(context.Context, RequestPolicy, Request) (RequestResult, error)
	TVOptions(context.Context) (Options, error)
}
type RequestPolicy struct {
	Label       string    `json:"label"`
	Approval    string    `json:"approval"`
	RequesterID int       `json:"requester_id"`
	Movie       Selection `json:"movie"`
	TV          Selection `json:"tv"`
}
type Selection struct {
	ServerID  int    `json:"server_id"`
	ProfileID int    `json:"profile_id"`
	Root      string `json:"root"`
}
type Request struct {
	Kind      string `json:"kind"`
	ID        int    `json:"id"`
	Season    int    `json:"season"`
	Operation string `json:"operation"`
	Confirmed bool   `json:"confirmed"`
}
type RequestResult struct {
	State string `json:"state"`
}
type Choice struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type Server struct {
	Choice
	Kind     string   `json:"kind"`
	Profiles []Choice `json:"profiles"`
	Roots    []string `json:"roots"`
}
type Options struct {
	Requesters []Choice `json:"requesters"`
	Servers    []Server `json:"servers"`
}
type Title struct {
	ID           int      `json:"id"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	Year         string   `json:"year"`
	Overview     string   `json:"overview"`
	Poster       string   `json:"poster"`
	State        string   `json:"state,omitempty"`
	CanRequest   bool     `json:"can_request"`
	Seasons      []Season `json:"seasons,omitempty"`
	Runtime      int      `json:"runtime,omitempty"`
	Directors    []string `json:"directors,omitempty"`
	Creators     []string `json:"creators,omitempty"`
	Cast         []string `json:"cast,omitempty"`
	Availability string   `json:"availability,omitempty"`
	Rating       *float64 `json:"rating,omitempty"`
	Because      []string `json:"because,omitempty"`
	Seeds        []string `json:"seeds,omitempty"`
}
type Season struct {
	Number     int    `json:"number"`
	Episodes   int    `json:"episodes"`
	State      string `json:"state"`
	CanRequest bool   `json:"can_request"`
}
type Titles struct {
	Titles []Title  `json:"titles"`
	Hidden []string `json:"hidden"`
}
type Detail struct {
	Title           Title `json:"title"`
	RequestsEnabled bool  `json:"requests_enabled"`
}
type Seed struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
}
type Discovery struct {
	Status        string   `json:"status"`
	CheckedAt     string   `json:"checked_at"`
	Partial       bool     `json:"partial"`
	LibraryStatus string   `json:"library_status"`
	Titles        []Title  `json:"titles"`
	Recent        []Title  `json:"recent"`
	Watchlist     []Title  `json:"watchlist"`
	Seeds         []Seed   `json:"seeds"`
	Hidden        []string `json:"hidden"`
}
