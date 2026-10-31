package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
)

// legacyRequest consults the exact global title/season hash barrier before a
// modern household operation can reserve or send any upstream request.
func (d *duskState) legacyRequest(v mediadata.Request) (string, bool) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", v.Kind, v.ID, v.Season))))
	b, e := os.ReadFile(filepath.Join(filepath.Dir(d.path), "legacy-requests", hash+".json"))
	if os.IsNotExist(e) {
		return "", false
	}
	if e != nil {
		return "unknown", true
	}
	var receipt struct {
		Operation string `json:"operation"`
		State     string `json:"state"`
	}
	if len(b) > 4096 || json.Unmarshal(b, &receipt) != nil || receipt.State == "" {
		return "unknown", true
	}
	if receipt.State == "rejected" || receipt.State == "configuration" {
		return receipt.State, receipt.Operation == v.Operation
	}
	return receipt.State, true
}

func (d *duskState) decorateLegacy(title *mediadata.Title) {
	apply := func(season int, state *string, can *bool) {
		if !*can {
			return
		}
		receipt, ok := d.legacyRequest(mediadata.Request{Kind: title.Kind, ID: title.ID, Season: season})
		if !ok {
			return
		}
		*can = false
		switch receipt {
		case "approved", "already_requested":
			*state = "requested"
		case "pending":
			*state = "pending"
		default:
			*state = "unknown_request"
		}
	}
	if title.Kind == "movie" {
		apply(0, &title.State, &title.CanRequest)
	} else {
		for i := range title.Seasons {
			apply(title.Seasons[i].Number, &title.Seasons[i].State, &title.Seasons[i].CanRequest)
		}
	}
}
