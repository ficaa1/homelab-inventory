package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func (m *Media) TVOptions(ctx context.Context) (mediadata.Options, error) {
	out := mediadata.Options{Requesters: []mediadata.Choice{}, Servers: []mediadata.Server{}}
	s, ok := m.tvService("overseerr")
	if !ok {
		return out, errors.New("request source is not configured")
	}
	var users struct {
		Results []struct {
			ID                    int
			DisplayName, Username string
		}
	}
	if err := m.get(ctx, s, "/api/v1/user", nil, &users, true); err != nil {
		return out, err
	}
	for _, u := range users.Results {
		if u.ID > 0 {
			name := u.DisplayName
			if name == "" {
				name = u.Username
			}
			out.Requesters = append(out.Requesters, mediadata.Choice{ID: u.ID, Name: m.text(name)})
		}
	}
	for _, kind := range []string{"movie", "tv"} {
		app := "radarr"
		if kind == "tv" {
			app = "sonarr"
		}
		var servers []struct {
			ID   *int
			Name string
			Is4k bool
		}
		if err := m.get(ctx, s, "/api/v1/settings/"+app, nil, &servers, true); err != nil {
			return out, err
		}
		for _, v := range servers {
			if v.ID == nil || *v.ID < 0 || v.Is4k {
				continue
			}
			var detail struct {
				Profiles []struct {
					ID   int
					Name string
				}
				RootFolders []struct{ Path string }
			}
			if err := m.get(ctx, s, "/api/v1/settings/"+app+"/"+strconv.Itoa(*v.ID), nil, &detail, true); err != nil {
				return out, err
			}
			server := mediadata.Server{Choice: mediadata.Choice{ID: *v.ID, Name: m.text(v.Name)}, Kind: kind, Profiles: []mediadata.Choice{}, Roots: []string{}}
			for _, p := range detail.Profiles {
				if p.ID > 0 {
					server.Profiles = append(server.Profiles, mediadata.Choice{ID: p.ID, Name: m.text(p.Name)})
				}
			}
			for _, r := range detail.RootFolders {
				if r.Path != "" {
					server.Roots = append(server.Roots, r.Path)
				}
			}
			out.Servers = append(out.Servers, server)
		}
	}
	return out, nil
}

var duskOperation = regexp.MustCompile(`^[A-Za-z0-9-]{16,80}$`)

// TVRequest is the only write path. The lifecycle bridge serializes it; callers
// must durably reserve Operation before calling, and retain unknown outcomes.
func (m *Media) TVRequest(ctx context.Context, p mediadata.RequestPolicy, r mediadata.Request) (mediadata.RequestResult, error) {
	result := mediadata.RequestResult{State: "configuration"}
	if !validTVTitle(r.Kind, r.ID) || !r.Confirmed || !duskOperation.MatchString(r.Operation) || (r.Kind == "movie" && r.Season != 0) || (r.Kind == "tv" && (r.Season < 1 || r.Season > 1000)) {
		result.State = "rejected"
		return result, errors.New("invalid confirmed request")
	}
	if p.RequesterID <= 0 || (p.Approval != "manual" && p.Approval != "automatic") {
		return result, errors.New("request policy is incomplete")
	}
	selection := p.Movie
	if r.Kind == "tv" {
		selection = p.TV
	}
	options, err := m.TVOptions(ctx)
	if err != nil {
		return result, err
	}
	userOK, serverOK := false, false
	for _, u := range options.Requesters {
		if u.ID == p.RequesterID {
			userOK = true
		}
	}
	for _, s := range options.Servers {
		if s.ID == selection.ServerID && s.Kind == r.Kind {
			profile, root := false, false
			for _, v := range s.Profiles {
				profile = profile || v.ID == selection.ProfileID
			}
			for _, v := range s.Roots {
				root = root || v == selection.Root
			}
			serverOK = profile && root
		}
	}
	if !userOK || !serverOK {
		return result, errors.New("request policy choices are no longer available")
	}
	s, _ := m.tvService("overseerr")
	var user struct{ ID, Permissions int }
	if err := m.get(ctx, s, "/api/v1/user/"+strconv.Itoa(p.RequesterID), nil, &user, false); err != nil {
		return result, err
	}
	if user.ID != p.RequesterID {
		return result, errors.New("requester cannot be verified")
	}
	// Overseerr permissions: admin=2, auto approve=32, movie=64, tv=128.
	auto := user.Permissions&2 != 0 || user.Permissions&32 != 0 || r.Kind == "movie" && user.Permissions&64 != 0 || r.Kind == "tv" && user.Permissions&128 != 0
	if auto != (p.Approval == "automatic") {
		return result, errors.New("requester approval permissions do not match policy")
	}
	value, err := m.TVRead(ctx, "title", r.Kind, r.ID, "")
	if err != nil {
		return result, err
	}
	detail := value.(mediadata.Detail)
	can := detail.Title.CanRequest
	if r.Kind == "tv" {
		can = false
		for _, v := range detail.Title.Seasons {
			if v.Number == r.Season {
				can = v.CanRequest
			}
		}
	}
	if !can {
		result.State = "already_requested"
		if detail.Title.State == "available" {
			result.State = "available"
		}
		return result, nil
	}
	body := map[string]any{"mediaType": r.Kind, "mediaId": r.ID, "is4k": false, "serverId": selection.ServerID, "profileId": selection.ProfileID, "rootFolder": selection.Root, "userId": p.RequesterID}
	if r.Kind == "tv" {
		body["seasons"] = []int{r.Season}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/api/v1/request", bytes.NewReader(b))
	if err != nil {
		return result, errors.New("invalid request")
	}
	req.Header.Set("X-Api-Key", s.key)
	req.Header.Set("Content-Type", "application/json")
	result.State = "unknown"
	resp, err := m.host.HTTP().Do(req)
	if err != nil {
		return result, errors.New("request outcome unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 409 {
		result.State = "already_requested"
		return result, nil
	}
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		result.State = "rejected"
		return result, nil
	}
	if resp.StatusCode != 201 {
		return result, errors.New("request outcome unknown")
	}
	var out struct {
		ID, Status, ProfileID, ServerID int
		Is4k                            bool
		Seasons                         []struct{ SeasonNumber int }
		Type                            string
		Media                           struct{ TMDBID int }
	}
	if json.NewDecoder(io.LimitReader(resp.Body, plugin.MaxHTTPBody)).Decode(&out) != nil || out.ID <= 0 || out.Type != r.Kind || out.Media.TMDBID != r.ID || out.Is4k || out.ProfileID != selection.ProfileID || out.ServerID != selection.ServerID || (r.Kind == "tv" && (len(out.Seasons) != 1 || out.Seasons[0].SeasonNumber != r.Season)) {
		return result, errors.New("request outcome could not be verified")
	}
	if out.Status == 1 {
		result.State = "pending"
	} else if out.Status == 2 || out.Status == 5 {
		result.State = "approved"
	}
	m.cache = map[string]cached{}
	m.cacheBytes = 0
	return result, nil
}
