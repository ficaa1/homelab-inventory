package archivetier

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

const plexRefresh = 7 * 24 * time.Hour

// PlexLibrary is a complete external-ID index, never a partial library scan.
// It and the per-title outcomes survive restarts in the supervisor state.
type PlexLibrary struct {
	At     time.Time           `json:"at"`
	Source string              `json:"source"`
	IDs    map[string][]string `json:"ids"`
}
type PlexIdentity struct {
	At     time.Time `json:"at"`
	Key    string    `json:"key,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

func (t Target) plexKey(c Config) string {
	if key := c.PlexKeys[t.Key()]; key != "" {
		return key
	}
	return t.PlexKey
}
func (t Target) externalIDs() []string {
	ids := []string{}
	if t.Kind == "radarr" && t.TMDBID > 0 {
		ids = append(ids, "tmdb://"+strconv.Itoa(t.TMDBID))
	}
	if t.Kind == "sonarr" && t.TVDBID > 0 {
		ids = append(ids, "tvdb://"+strconv.Itoa(t.TVDBID))
	}
	if t.IMDBID != "" {
		ids = append(ids, "imdb://"+t.IMDBID)
	}
	return ids
}

// ResolveTargets uses only GETs. Listings are read once per refresh, and each
// movie/show metadata record once. Unknown, incomplete or ambiguous identity
// never becomes an unwatched title. Configuration overrides are applied by the
// runner and never enter this automatic cache.
func (p *Plex) ResolveTargets(ctx context.Context, targets []Target, st *State, now time.Time) error {
	kinds := map[string]bool{}
	for _, t := range targets {
		kinds[t.Kind] = true
	}
	if st.PlexLibraries == nil {
		st.PlexLibraries = map[string]PlexLibrary{}
	}
	if st.PlexIdentities == nil {
		st.PlexIdentities = map[string]PlexIdentity{}
	}
	for _, kind := range []string{"radarr", "sonarr"} {
		if !kinds[kind] {
			continue
		}
		cache, ok := st.PlexLibraries[kind]
		if !ok || cache.IDs == nil || cache.Source != p.Base || cache.At.After(now) || now.Sub(cache.At) >= plexRefresh {
			ids, err := p.libraryIDs(ctx, kind)
			if err != nil {
				// Retain old evidence for inspection, but never use an expired index.
				for _, t := range targets {
					if t.Kind == kind {
						st.PlexIdentities[t.Key()] = PlexIdentity{At: now, Reason: "Plex identity lookup failed: " + err.Error()}
					}
				}
				return err
			}
			cache = PlexLibrary{At: now, Source: p.Base, IDs: ids}
			st.PlexLibraries[kind] = cache
		}
		for _, t := range targets {
			if t.Kind != kind {
				continue
			}
			matches := map[string]bool{}
			for _, id := range t.externalIDs() {
				for _, key := range cache.IDs[id] {
					matches[key] = true
				}
			}
			identity := PlexIdentity{At: cache.At, Reason: "not found in Plex"}
			if len(matches) == 1 {
				for key := range matches {
					identity.Key = key
				}
				identity.Reason = ""
			}
			if len(matches) > 1 {
				identity.Reason = "ambiguous Plex identity (more than one match)"
			}
			st.PlexIdentities[t.Key()] = identity
		}
	}
	return nil
}

func (p *Plex) libraryIDs(ctx context.Context, kind string) (map[string][]string, error) {
	mediaType := "movie"
	if kind == "sonarr" {
		mediaType = "show"
	}
	var libraries *[]struct {
		ID   Number `json:"section_id"`
		Type string `json:"section_type"`
	}
	if err := p.call(ctx, "get_library_names", url.Values{}, &libraries); err != nil {
		return nil, err
	}
	if libraries == nil {
		return nil, fmt.Errorf("missing Plex libraries")
	}
	ids := map[string][]string{}
	sections := map[string]bool{}
	keys := map[string]bool{}
	for _, lib := range *libraries {
		if lib.Type != mediaType {
			continue
		}
		if lib.ID == "" || sections[string(lib.ID)] {
			return nil, fmt.Errorf("invalid Plex section identity")
		}
		sections[string(lib.ID)] = true
		total := -1
		for start := 0; start < 10000; start += 100 {
			var page struct {
				Total Number `json:"recordsFiltered"`
				Data  *[]struct {
					Key  Number `json:"rating_key"`
					Type string `json:"media_type"`
				} `json:"data"`
			}
			q := url.Values{"section_id": {string(lib.ID)}, "start": {strconv.Itoa(start)}, "length": {"100"}, "order_column": {"rating_key"}, "order_dir": {"asc"}}
			if start == 0 {
				q.Set("refresh", "true")
			}
			if err := p.call(ctx, "get_library_media_info", q, &page); err != nil {
				return nil, err
			}
			n, err := strconv.Atoi(string(page.Total))
			if err != nil || n < 0 || n > 10000 || page.Data == nil || len(*page.Data) > 100 || total >= 0 && n != total {
				return nil, fmt.Errorf("incomplete Plex library listing")
			}
			total = n
			for _, row := range *page.Data {
				key := string(row.Key)
				num, err := strconv.Atoi(key)
				if err != nil || num <= 0 || row.Type != mediaType || keys[key] {
					return nil, fmt.Errorf("invalid or repeated Plex library identity")
				}
				keys[key] = true
				var md struct {
					Key   Number    `json:"rating_key"`
					Type  string    `json:"media_type"`
					GUIDs *[]string `json:"guids"`
				}
				if err := p.call(ctx, "get_metadata", url.Values{"rating_key": {key}}, &md); err != nil {
					return nil, err
				}
				if string(md.Key) != key || md.Type != mediaType || md.GUIDs == nil {
					return nil, fmt.Errorf("incomplete Plex metadata")
				}
				for _, guid := range *md.GUIDs {
					found := false
					for _, previous := range ids[guid] {
						if previous == key {
							found = true
						}
					}
					if !found {
						ids[guid] = append(ids[guid], key)
					}
				}
			}
			count := start + len(*page.Data)
			if count == total {
				break
			}
			if count > total || len(*page.Data) < 100 || count >= 10000 {
				return nil, fmt.Errorf("incomplete Plex library listing")
			}
		}
	}
	for _, values := range ids {
		sort.Strings(values)
	}
	return ids, nil
}

func (r *Runner) resolvePlex(ctx context.Context, targets []Target, st *State, now time.Time) {
	pending := []Target{}
	for _, t := range targets {
		if r.Config.PlexKeys[t.Key()] == "" {
			pending = append(pending, t)
		}
	}
	if resolver, ok := r.Plex.(interface {
		ResolveTargets(context.Context, []Target, *State, time.Time) error
	}); ok && len(pending) > 0 {
		if err := resolver.ResolveTargets(ctx, pending, st, now); err != nil {
			// Fail the whole batch closed, including titles from another section.
			for _, t := range pending {
				st.PlexIdentities[t.Key()] = PlexIdentity{At: now, Reason: "Plex identity lookup failed: " + err.Error()}
			}
		}
	}
	for i := range targets {
		targets[i].PlexKey, targets[i].PlexReason = "", "not found in Plex"
		if key := r.Config.PlexKeys[targets[i].Key()]; key != "" {
			targets[i].PlexKey, targets[i].PlexReason = key, ""
			continue
		}
		if id, ok := st.PlexIdentities[targets[i].Key()]; ok {
			targets[i].PlexKey, targets[i].PlexReason = id.Key, id.Reason
		}
	}
}
