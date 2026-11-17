package cve

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (p *Integration) enrichment(ctx context.Context, cfg config, vulns []Vulnerability) error {
	p.mu.RLock()
	b, _ := json.Marshal(p.enrich)
	p.mu.RUnlock()
	var e enrichment
	_ = json.Unmarshal(b, &e)
	now := p.host.Now().UTC()
	e.Sources.KEV.Enabled = cfg.KEV != ""
	e.Sources.EPSS.Enabled = cfg.EPSS != ""
	publish := func() error {
		if err := p.save("enrichment.json", e); err != nil {
			return errors.New("enrichment cache unavailable")
		}
		// Publish independent maps: the worker continues filling its local copy.
		b, _ := json.Marshal(e)
		var snapshot enrichment
		_ = json.Unmarshal(b, &snapshot)
		p.mu.Lock()
		p.enrich = snapshot
		p.status.Sources.KEV = snapshot.Sources.KEV
		p.status.Sources.EPSS = snapshot.Sources.EPSS
		p.mu.Unlock()
		return nil
	}
	due := func(t *time.Time) bool { return t == nil || now.Sub(*t) >= 24*time.Hour }
	if cfg.KEV != "" && due(e.KEVAttempt) {
		e.KEVAttempt = &now
		if err := publish(); err != nil {
			return err
		}
		var feed struct {
			Vulnerabilities []struct {
				CVEID      string `json:"cveID"`
				Name       string `json:"vulnerabilityName"`
				DateAdded  string `json:"dateAdded"`
				DueDate    string `json:"dueDate"`
				Ransomware string `json:"knownRansomwareCampaignUse"`
			} `json:"vulnerabilities"`
		}
		err := p.request(ctx, "GET", cfg.KEV, nil, &feed)
		if err == nil && feed.Vulnerabilities == nil {
			err = errors.New("invalid KEV feed")
		}
		if err != nil {
			e.Sources.KEV.OK = false
			e.Sources.KEV.Error = ptr(err.Error())
		} else {
			entries := map[string]KEV{}
			for _, v := range feed.Vulnerabilities {
				if strings.HasPrefix(v.CVEID, "CVE-") {
					entries[v.CVEID] = KEV{DateAdded: v.DateAdded, DueDate: v.DueDate, Ransomware: strings.EqualFold(v.Ransomware, "Known"), Name: v.Name}
				}
			}
			e.KEV = entries
			e.Sources.KEV.SourceStatus = SourceStatus{OK: true, UpdatedAt: &now}
			e.Sources.KEV.Count = len(entries)
		}
		if err := publish(); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cfg.EPSS != "" && due(e.EPSSAttempt) {
		e.EPSSAttempt = &now
		if err := publish(); err != nil {
			return err
		}
		ids := []string{}
		for _, v := range vulns {
			if strings.HasPrefix(v.ID, "CVE-") {
				ids = append(ids, v.ID)
			}
		}
		sort.Strings(ids)
		complete := true
		for start := 0; start < len(ids); start += 100 {
			batch := ids[start:min(start+100, len(ids))]
			q := url.Values{"cve": {strings.Join(batch, ",")}, "limit": {"100"}}
			var response struct {
				Data []struct {
					CVE        string `json:"cve"`
					EPSS       string `json:"epss"`
					Percentile string `json:"percentile"`
				} `json:"data"`
			}
			err := p.request(ctx, "GET", cfg.EPSS+"?"+q.Encode(), nil, &response)
			updates := map[string]EPSS{}
			wanted := map[string]bool{}
			for _, id := range batch {
				wanted[id] = true
			}
			if err == nil && response.Data == nil {
				err = errors.New("invalid EPSS response")
			}
			if err == nil {
				for _, v := range response.Data {
					score, e1 := strconv.ParseFloat(v.EPSS, 64)
					percentile, e2 := strconv.ParseFloat(v.Percentile, 64)
					if !wanted[v.CVE] || e1 != nil || e2 != nil || math.IsNaN(score) || math.IsNaN(percentile) || score < 0 || score > 1 || percentile < 0 || percentile > 1 {
						err = errors.New("invalid EPSS score")
						break
					}
					updates[v.CVE] = EPSS{Score: score, Percentile: percentile}
				}
			}
			if err != nil {
				complete = false
				e.Sources.EPSS.OK = false
				e.Sources.EPSS.Error = ptr(err.Error())
			} else {
				for _, id := range batch {
					e.EPSSTimes[id] = now
					if score, ok := updates[id]; ok {
						e.EPSS[id] = score
					} else {
						delete(e.EPSS, id)
					}
				}
			}
			if err := publish(); err != nil {
				return err
			}
			p.phase("enrichment", min(start+100, len(ids)), len(ids))
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if complete {
			e.Sources.EPSS.SourceStatus = SourceStatus{OK: true, UpdatedAt: &now}
		}
	}
	return publish()
}
