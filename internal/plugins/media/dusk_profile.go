package media

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
)

var _ mediadata.ProfileSource = (*Media)(nil)

// TVProfiles returns only account IDs and display labels for admin selection.
func (m *Media) TVProfiles(ctx context.Context) ([]mediadata.Choice, error) {
	out := []mediadata.Choice{}
	s, ok := m.tvService("tautulli")
	if !ok {
		return out, nil
	}
	var rows []struct {
		UserID       number `json:"user_id"`
		FriendlyName string `json:"friendly_name"`
		Username     string `json:"username"`
	}
	if err := m.get(ctx, s, "/api/v2", url.Values{"cmd": {"get_users"}}, &rows, true); err != nil {
		return nil, err
	}
	if rows == nil || len(rows) > 1000 {
		return nil, errors.New("invalid playback profile collection")
	}
	seen := map[int]bool{}
	for _, r := range rows {
		id := int(r.UserID)
		if id <= 0 || number(id) != r.UserID || seen[id] {
			continue
		}
		seen[id] = true
		name := r.FriendlyName
		if name == "" {
			name = r.Username
		}
		out = append(out, mediadata.Choice{ID: id, Name: m.text(name)})
	}
	return out, nil
}

// TVHistory reads at most one page and resolves at most twenty distinct Plex
// titles to public TMDB identities. Paths, users, devices and stream details
// never enter the shared title contract. Unmatched metadata is skipped.
func (m *Media) TVHistory(ctx context.Context, userID int) ([]mediadata.Title, error) {
	out := []mediadata.Title{}
	if userID <= 0 {
		return nil, errors.New("invalid playback profile")
	}
	profiles, err := m.TVProfiles(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, p := range profiles {
		found = found || p.ID == userID
	}
	if !found {
		return nil, errors.New("playback profile unavailable")
	}
	s, _ := m.tvService("tautulli")
	var history struct {
		Data []struct {
			UserID               number `json:"user_id"`
			MediaType            string `json:"media_type"`
			RatingKey            number `json:"rating_key"`
			GrandparentRatingKey number `json:"grandparent_rating_key"`
		}
	}
	if err := m.get(ctx, s, "/api/v2", url.Values{"cmd": {"get_history"}, "user_id": {strconv.Itoa(userID)}, "length": {"100"}, "start": {"0"}, "order_column": {"date"}, "order_dir": {"desc"}}, &history, true); err != nil {
		return nil, err
	}
	if history.Data == nil || len(history.Data) > 100 {
		return nil, errors.New("invalid playback history")
	}
	keys := map[int]bool{}
	titles := map[string]bool{}
	resolved := 0
	for _, r := range history.Data {
		if int(r.UserID) != userID || number(userID) != r.UserID {
			return nil, errors.New("playback history profile mismatch")
		}
		kind, key := "movie", int(r.RatingKey)
		if r.MediaType == "episode" {
			kind, key = "tv", int(r.GrandparentRatingKey)
		} else if r.MediaType != "movie" {
			continue
		}
		if key <= 0 || keys[key] {
			continue
		}
		keys[key] = true
		if resolved >= 20 {
			break
		}
		resolved++
		var metadata struct {
			MediaType      string `json:"media_type"`
			Title, Summary string
			Year           number
			GUIDs          []string `json:"guids"`
		}
		if err := m.get(ctx, s, "/api/v2", url.Values{"cmd": {"get_metadata"}, "rating_key": {strconv.Itoa(key)}}, &metadata, true); err != nil {
			return nil, err
		}
		if (kind == "tv" && metadata.MediaType != "show") || (kind == "movie" && metadata.MediaType != "movie") {
			continue
		}
		id := 0
		for _, g := range metadata.GUIDs {
			raw, ok := strings.CutPrefix(g, "tmdb://")
			if ok {
				id, _ = strconv.Atoi(raw)
				break
			}
		}
		if !validTVTitle(kind, id) {
			continue
		}
		identity := kind + ":" + strconv.Itoa(id)
		if titles[identity] {
			continue
		}
		titles[identity] = true
		t := mediadata.Title{ID: id, Kind: kind, Title: m.text(metadata.Title), Overview: m.text(metadata.Summary), Availability: "available"}
		year := int(metadata.Year)
		if year >= 1800 && year <= 3000 {
			t.Year = strconv.Itoa(year)
		}
		out = append(out, t)
	}
	return out, nil
}
