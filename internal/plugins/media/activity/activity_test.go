package activity

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestReadRejectsPartialHistoryAndJoinsSeasonImports(t *testing.T) {
	fail := false
	reads := 0
	fetch := func(_ context.Context, path string, q url.Values, out any) error {
		reads++
		var raw string
		switch path {
		case "/queue":
			raw = `{"totalRecords":1,"records":[{"id":1,"seriesId":7,"episodeId":71,"downloadId":"synthetic","status":"completed","trackedDownloadState":"importPending","size":1000,"sizeleft":0,"series":{"title":"Example"}}]}`
		case "/history":
			if fail {
				return errors.New("offline")
			}
			if q.Get("downloadId") != "synthetic" {
				t.Error("incorrect history join")
			}
			raw = `{"totalRecords":1,"records":[{"id":2,"seriesId":7,"episodeId":70,"downloadId":"synthetic","eventType":"downloadFolderImported"}]}`
		case "/episode":
			raw = `[{"id":70,"seasonNumber":1},{"id":71,"seasonNumber":1}]`
		default:
			t.Error("unexpected route")
		}
		return json.Unmarshal([]byte(raw), out)
	}
	a, e := Read(context.Background(), "Sonarr", true, fetch, func(s string) string { return s })
	if e != nil || reads != 3 {
		t.Fatalf("read failed %v", e)
	}
	v := Build(time.Now(), Feed{Status: StatusOK, Activity: a}, Feed{Status: StatusUnconfigured})
	if len(v.Titles) != 1 || len(v.Titles[0].Seasons) != 1 || *v.Titles[0].Seasons[0].Imported != 1 {
		t.Fatal("imports not joined")
	}
	fail = true
	if _, e = Read(context.Background(), "Sonarr", true, fetch, strings.TrimSpace); e == nil {
		t.Fatal("partial history accepted")
	}
}
