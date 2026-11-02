package media

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (m *Media) digestRead(ctx context.Context, path string, query url.Values) (any, error) {
	id, _, _ := strings.Cut(path, "/")
	var service service
	for _, s := range m.services {
		if s.id == id {
			service = s
			break
		}
	}
	var out any
	var err error
	if service.id == "" {
		err = fmt.Errorf("%s: not configured", id)
	} else {
		tau := &digestTautulli{media: m, service: service}
		arr := &digestArr{media: m, service: service}
		switch path {
		case "tautulli/activity":
			out, err = tau.Activity(ctx)
		case "tautulli/history":
			hours, e := strconv.Atoi(query.Get("hours"))
			if e != nil || hours < 1 || hours > 168 {
				hours = 24
			}
			out, err = tau.History(ctx, m.host.Now().Add(-time.Duration(hours)*time.Hour))
		case "tautulli/libraries":
			out, err = tau.Libraries(ctx)
		case "sonarr/summary":
			out, err = arr.sonarrSummary(ctx, m.host.Now())
		case "radarr/summary":
			out, err = arr.radarrSummary(ctx, m.host.Now())
		}
	}
	return out, err
}
