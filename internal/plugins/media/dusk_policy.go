package media

import (
	"context"
	"errors"
	"strconv"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
)

var _ mediadata.PolicySource = (*Media)(nil)

// TVValidatePolicy validates every selected media kind rather than assuming
// that movie and series auto-approval permissions are identical. The requester
// detail is always read fresh: a recently revoked permission cannot be masked
// by the ordinary twenty-second metadata cache.
func (m *Media) TVValidatePolicy(ctx context.Context, p mediadata.RequestPolicy) error {
	if p.RequesterID <= 0 || (p.Approval != "manual" && p.Approval != "automatic") {
		return errors.New("select a requester and manual or automatic approval")
	}
	options, err := m.TVOptions(ctx)
	if err != nil {
		return err
	}
	requester := false
	for _, u := range options.Requesters {
		requester = requester || u.ID == p.RequesterID
	}
	if !requester {
		return errors.New("selected requester is unavailable")
	}
	kinds := []string{}
	for _, kind := range []string{"movie", "tv"} {
		selection := p.Movie
		if kind == "tv" {
			selection = p.TV
		}
		if selection.ProfileID == 0 && selection.Root == "" && selection.ServerID == 0 {
			continue
		}
		matched := false
		for _, server := range options.Servers {
			if server.ID != selection.ServerID || server.Kind != kind {
				continue
			}
			profile, root := false, false
			for _, v := range server.Profiles {
				profile = profile || v.ID == selection.ProfileID
			}
			for _, v := range server.Roots {
				root = root || v == selection.Root
			}
			matched = matched || profile && root
		}
		if !matched {
			return errors.New("select configured servers, quality profiles and root folders")
		}
		kinds = append(kinds, kind)
	}
	if len(kinds) == 0 {
		return errors.New("select at least one media request destination")
	}
	s, ok := m.tvService("overseerr")
	if !ok {
		return errors.New("request source is not configured")
	}
	var user struct {
		ID          int
		Permissions *int
	}
	if err := m.get(ctx, s, "/api/v1/user/"+strconv.Itoa(p.RequesterID), nil, &user, false); err != nil {
		return err
	}
	if user.ID != p.RequesterID || user.Permissions == nil || *user.Permissions < 0 {
		return errors.New("requester permissions cannot be verified")
	}
	permissions := *user.Permissions
	for _, kind := range kinds {
		automatic := permissions&2 != 0 || permissions&32 != 0 || kind == "movie" && permissions&64 != 0 || kind == "tv" && permissions&128 != 0
		if automatic != (p.Approval == "automatic") {
			return errors.New("requester approval permissions do not match policy")
		}
	}
	return nil
}
