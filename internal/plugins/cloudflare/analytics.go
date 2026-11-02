package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

type ZoneInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Plan   string `json:"plan"`
}

type Analytics struct {
	Zone       string          `json:"zone"`
	Since      time.Time       `json:"since"`
	Until      time.Time       `json:"until"`
	Requests   int64           `json:"requests"`
	Bytes      int64           `json:"bytes"` // bandwidth
	Threats    int64           `json:"threats"`
	Cached     int64           `json:"cached"`   // requests served from cache
	Uncached   int64           `json:"uncached"` //
	Uniques    int64           `json:"uniques"`
	Hours      []AnalyticsHour `json:"hours"` // one bucket per hour, ascending
	CacheRatio float64         `json:"cache_ratio"`
}

type AnalyticsHour struct {
	Time     time.Time `json:"time"`
	Requests int64     `json:"requests"`
	Bytes    int64     `json:"bytes"`
	Threats  int64     `json:"threats"`
}

func (p *Integration) zoneInfo(ctx context.Context) (*ZoneInfo, error) {
	if err := p.resolveZone(ctx); err != nil {
		return nil, err
	}
	var out struct {
		Success bool
		Result  struct {
			ID, Name, Status string
			Plan             struct{ Name string }
		}
	}
	if err := p.read(ctx, "/zones/"+p.zone, nil, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, errors.New("zone read refused")
	}
	return &ZoneInfo{ID: out.Result.ID, Name: out.Result.Name, Status: out.Result.Status, Plan: out.Result.Plan.Name}, nil
}
func (p *Integration) analytics(ctx context.Context, since, until time.Time) (*Analytics, error) {
	if err := p.resolveZone(ctx); err != nil {
		return nil, err
	}
	zoneID := p.zone
	sinceDay := since.Format("2006-01-02")
	untilDay := until.Format("2006-01-02")
	query := `query($zone: String!, $since: String!, $until: String!) {
  viewer {
    zones(filter: {zoneTag: $zone}) {
      httpRequests1hGroups(limit: 2000, filter: {date_geq: $since, date_lt: $until}) {
        dimensions { datetime }
        sum { requests bytes threats cachedRequests }
        uniq { uniques }
      }
    }
  }
}`
	var envelope struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			Viewer struct {
				Zones []struct {
					Groups []struct {
						Dimensions struct {
							Datetime string `json:"datetime"`
						} `json:"dimensions"`
						Sum struct {
							Requests       int64 `json:"requests"`
							Bytes          int64 `json:"bytes"`
							Threats        int64 `json:"threats"`
							CachedRequests int64 `json:"cachedRequests"`
						} `json:"sum"`
						Uniq struct {
							Uniques int64 `json:"uniques"`
						} `json:"uniq"`
					} `json:"httpRequests1hGroups"`
				} `json:"zones"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := p.read(ctx, "/graphql", map[string]any{"query": query, "variables": map[string]string{"zone": zoneID, "since": sinceDay, "until": untilDay}}, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Errors) > 0 || len(envelope.Data.Viewer.Zones) != 1 {
		return nil, errors.New("analytics unavailable; check zone analytics permission")
	}
	out := &Analytics{Zone: p.zoneLabel, Since: since, Until: until, Hours: []AnalyticsHour{}}
	for _, z := range envelope.Data.Viewer.Zones {
		for _, g := range z.Groups {
			t, err := time.Parse(time.RFC3339, g.Dimensions.Datetime)
			if err != nil {
				continue
			}
			out.Requests += g.Sum.Requests
			out.Bytes += g.Sum.Bytes
			out.Threats += g.Sum.Threats
			out.Cached += g.Sum.CachedRequests
			out.Uniques += g.Uniq.Uniques
			out.Hours = append(out.Hours, AnalyticsHour{
				Time: t, Requests: g.Sum.Requests, Bytes: g.Sum.Bytes, Threats: g.Sum.Threats,
			})
		}
	}
	out.Uncached = out.Requests - out.Cached
	if out.Cached+out.Uncached > 0 {
		out.CacheRatio = float64(out.Cached) / float64(out.Cached+out.Uncached)
	} else {
		out.CacheRatio = -1
	}
	sort.Slice(out.Hours, func(i, j int) bool { return out.Hours[i].Time.Before(out.Hours[j].Time) })
	return out, nil
}
