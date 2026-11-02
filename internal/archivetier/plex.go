package archivetier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Number accepts Tautulli's mixed numeric/string fields, failing on malformed data.
type Number string

func (n *Number) UnmarshalJSON(b []byte) error {
	if string(b) == "null" || string(b) == `""` {
		*n = ""
		return nil
	}
	var s string
	if len(b) > 0 && b[0] == '"' {
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
	} else {
		s = string(b)
	}
	if _, e := strconv.ParseInt(s, 10, 64); e != nil {
		return fmt.Errorf("invalid numeric field")
	}
	*n = Number(s)
	return nil
}

type Plex struct {
	Base, Key string
	HTTP      *http.Client
}

func NewPlex(base, key string) *Plex {
	return &Plex{strings.TrimRight(base, "/"), key, NewClient("", "", "", false).HTTP}
}
func (p *Plex) call(ctx context.Context, cmd string, q url.Values, out any) error {
	if !validBase(p.Base) || p.Key == "" {
		return fmt.Errorf("Tautulli not configured")
	}
	q.Set("apikey", p.Key)
	q.Set("cmd", cmd)
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, p.Base+"/api/v2?"+q.Encode(), nil)
	if e != nil {
		return fmt.Errorf("Tautulli request failed")
	}
	resp, e := p.HTTP.Do(req)
	if e != nil {
		return fmt.Errorf("Tautulli transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Tautulli HTTP %d", resp.StatusCode)
	}
	var env struct {
		Response struct {
			Result string          `json:"result"`
			Data   json.RawMessage `json:"data"`
		} `json:"response"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&env) != nil || env.Response.Result != "success" {
		return fmt.Errorf("Tautulli response failed")
	}
	if len(env.Response.Data) == 0 || string(env.Response.Data) == "null" {
		return fmt.Errorf("Tautulli response data missing")
	}
	if json.Unmarshal(env.Response.Data, out) != nil {
		return fmt.Errorf("Tautulli data decoding failed")
	}
	return nil
}
func (p *Plex) Active(ctx context.Context) (bool, error) {
	var v struct {
		Count    Number             `json:"stream_count"`
		Sessions *[]json.RawMessage `json:"sessions"`
	}
	if e := p.call(ctx, "get_activity", url.Values{}, &v); e != nil {
		return false, e
	}
	if v.Count == "" || v.Sessions == nil {
		return false, fmt.Errorf("missing Plex stream count or sessions")
	}
	n, _ := strconv.Atoi(string(v.Count))
	if n < 0 || n != len(*v.Sessions) {
		return false, fmt.Errorf("Plex stream count disagrees with sessions")
	}
	return n > 0, nil
}

func seasonKey(show string, season int) string { return show + ":s" + strconv.Itoa(season) }

// watched reports recent play of the target: a movie, or a show season (or
// the whole show when history lacked a season number).
func watched(recent map[string]bool, key string, t Target) bool {
	return recent[key] || t.Kind == "sonarr" && recent[seasonKey(key, t.Season)]
}

// Recent fetches the entire idle window. Unknown/truncated data blocks mutation.
func (p *Plex) Recent(ctx context.Context, now time.Time, days int) (map[string]bool, error) {
	out := map[string]bool{}
	since := now.AddDate(0, 0, -days)
	seen := map[string]bool{}
	total := -1
	for start := 0; start < 10000; start += 100 {
		var v struct {
			Total Number `json:"recordsFiltered"`
			Data  *[]struct {
				ID          Number `json:"id"`
				Date        Number `json:"date"`
				Stop        Number `json:"stopped"`
				Key         Number `json:"rating_key"`
				Grandparent Number `json:"grandparent_rating_key"`
				Season      Number `json:"parent_media_index"`
				Type        string `json:"media_type"`
			} `json:"data"`
		}
		q := url.Values{"after": {since.Format("2006-01-02")}, "start": {strconv.Itoa(start)}, "length": {"100"}, "order_column": {"date"}, "order_dir": {"desc"}}
		if e := p.call(ctx, "get_history", q, &v); e != nil {
			return nil, e
		}
		n, e := strconv.Atoi(string(v.Total))
		if e != nil || n < 0 || v.Data == nil || len(*v.Data) > 100 {
			return nil, fmt.Errorf("missing Plex history total or rows")
		}
		if total >= 0 && n != total {
			return nil, fmt.Errorf("Plex history changed during pagination")
		}
		total = n
		rows := *v.Data
		for _, r := range rows {
			if id, e := strconv.ParseInt(string(r.ID), 10, 64); e != nil || id <= 0 || seen[string(r.ID)] {
				return nil, fmt.Errorf("Plex history row identity missing or repeated")
			}
			seen[string(r.ID)] = true
			ts, e := strconv.ParseInt(string(r.Date), 10, 64)
			if e != nil {
				return nil, fmt.Errorf("missing playback timestamp")
			}
			if r.Stop != "" {
				stop, _ := strconv.ParseInt(string(r.Stop), 10, 64)
				if stop > ts {
					ts = stop
				}
			}
			if time.Unix(ts, 0).Before(since) {
				continue
			}
			key := r.Key
			if r.Type == "episode" {
				key = r.Grandparent
			}
			if key == "" {
				return nil, fmt.Errorf("history lacks stable Plex identity")
			}
			// Episodes count against their season only; without a season
			// number the whole show counts as watched.
			if n, e := strconv.Atoi(string(r.Season)); r.Type == "episode" && e == nil && n >= 0 {
				out[seasonKey(string(key), n)] = true
			} else {
				out[string(key)] = true
			}
		}
		if start+len(rows) == total {
			return out, nil
		}
		if start+len(rows) > total || len(rows) < 100 {
			return nil, fmt.Errorf("incomplete Plex history")
		}
	}
	return nil, fmt.Errorf("Plex history exceeds safety limit")
}
