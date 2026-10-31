package archive

import (
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"strconv"
	"strings"
)

func number(key, label string, required bool) plugin.Field {
	return plugin.Field{Key: key, Label: label, Type: plugin.Number, Required: required}
}
func boolean(key, label string) plugin.Field {
	return plugin.Field{Key: key, Label: label, Type: plugin.Bool, Required: true}
}
func kind() plugin.Field {
	return plugin.Field{Key: "kind", Label: "Service", Type: plugin.Select, Required: true, Options: []plugin.Option{{Value: "radarr", Label: "Radarr"}, {Value: "sonarr", Label: "Sonarr"}}}
}
func extraActions() []plugin.ActionSpec {
	return []plugin.ActionSpec{
		action("search", "Search one archive target", kind(), number("id", "Service title ID", true), number("season", "Season (0 for movie)", true)),
		action("approve", "Approve a cached release", kind(), number("id", "Service title ID", true), number("season", "Season (0 for movie)", true), text("release_id", "Worker-cached release ID"), number("indexer_id", "Indexer ID", true)),
		action("tag", "Set library policy tags", kind(), number("id", "Service title ID", true), boolean("archive", "Archive eligible"), boolean("four_k", "Keep 4K"), plugin.Field{Key: "seasons", Label: "Eligible seasons (comma separated)", Type: plugin.Text}),
		action("purge-all", "Purge all reviewed originals", text("hash", "Reviewed plan hash"), text("confirm", "Service confirmation phrase")),
		action("clear", "Clear current policy report"),
		action("seed-queue", "Queue reviewed seeding plans", text("items", "One torrent-hash:reviewed-plan-hash per line"), boolean("allow_move", "Allow storage move")),
		action("seed-cancel", "Cancel seeding queue"),
		action("selection-reset", "Reset selection overrides"),
		action("selection-save", "Update selection scoring", number("floor_1080", "1080p size floor", false), number("target_1080", "1080p size target", false), number("cap_1080", "1080p size cap", false), number("floor_2160", "2160p size floor", false), number("target_2160", "2160p size target", false), number("cap_2160", "2160p size cap", false), number("min_score", "Minimum score", false), number("hevc", "HEVC factor", false), number("avc", "AVC factor", false), number("av1", "AV1 factor", false), plugin.Field{Key: "trusted_groups", Label: "Trusted groups (comma separated)", Type: plugin.Text}, plugin.Field{Key: "denied_groups", Label: "Denied groups (comma separated)", Type: plugin.Text}),
	}
}
func integer(c plugin.Config, key string, min int) (int, error) {
	v, ok := c[key].(float64)
	if !ok || v < float64(min) || v > 1e9 || v != float64(int(v)) {
		return 0, errors.New("title, season and indexer IDs must be valid integers")
	}
	return int(v), nil
}
func extraPayload(id string, c plugin.Config) (string, string, any, error) {
	body := plugin.Config{"apply": true}
	switch id {
	case "search", "approve":
		kind, _ := c["kind"].(string)
		if kind != "sonarr" && kind != "radarr" {
			return "", "", nil, errors.New("invalid service kind")
		}
		title, e := integer(c, "id", 1)
		if e != nil {
			return "", "", nil, e
		}
		season, e := integer(c, "season", 0)
		if e != nil {
			return "", "", nil, e
		}
		if kind == "radarr" && season != 0 || kind == "sonarr" && season < 1 {
			return "", "", nil, errors.New("select one regular season for Sonarr")
		}
		target := plugin.Config{"kind": kind, "id": title, "season": season}
		if id == "approve" {
			indexer, e := integer(c, "indexer_id", 1)
			if e != nil {
				return "", "", nil, e
			}
			release, _ := c["release_id"].(string)
			if len(release) > 256 || release == "" {
				return "", "", nil, errors.New("invalid cached release ID")
			}
			target["releaseId"] = release
			target["indexerId"] = indexer
			body["approval"] = target
		} else {
			body["title"] = target
		}
		return "/archive/" + id, "POST", body, nil
	case "tag":
		title, e := integer(c, "id", 1)
		if e != nil {
			return "", "", nil, e
		}
		seasons := []int{}
		raw, _ := c["seasons"].(string)
		if len(raw) > 1000 {
			return "", "", nil, errors.New("too many seasons")
		}
		for _, part := range strings.Split(raw, ",") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			n, e := strconv.Atoi(strings.TrimSpace(part))
			if e != nil || n < 1 || n > 10000 {
				return "", "", nil, errors.New("invalid eligible season")
			}
			seasons = append(seasons, n)
		}
		return "/archive/tag", "POST", plugin.Config{"kind": c["kind"], "id": title, "archive": c["archive"], "fourK": c["four_k"], "seasons": seasons}, nil
	case "purge-all":
		return "/archive/purge-all", "POST", plugin.Config{"apply": true, "hash": c["hash"], "confirm": c["confirm"]}, nil
	case "clear":
		return "/archive/clear", "POST", body, nil
	case "seed-cancel":
		return "/seeding/queue/cancel", "POST", body, nil
	case "seed-queue":
		raw, _ := c["items"].(string)
		lines := strings.Split(strings.TrimSpace(raw), "\n")
		if len(lines) < 1 || len(lines) > 50 {
			return "", "", nil, errors.New("select 1 to 50 reviewed plans")
		}
		items := []plugin.Config{}
		for _, line := range lines {
			hash, plan, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || len(hash) < 32 || len(hash) > 64 || len(plan) < 16 || len(plan) > 128 || strings.ContainsAny(hash+plan, " /\\\r\t") {
				return "", "", nil, errors.New("invalid reviewed seeding plan")
			}
			items = append(items, plugin.Config{"hash": hash, "planHash": plan})
		}
		return "/seeding/queue", "POST", plugin.Config{"apply": true, "items": items, "allowMove": c["allow_move"]}, nil
	case "selection-reset":
		return "/archive/selection", "PUT", plugin.Config{"reset": true}, nil
	case "selection-save":
		overrides := plugin.Config{}
		sizes := plugin.Config{}
		for _, mode := range []string{"1080", "2160"} {
			band := plugin.Config{}
			for _, field := range []string{"floor", "target", "cap"} {
				if value, ok := c[field+"_"+mode]; ok {
					n, _ := value.(float64)
					if n < 0 || n > 10000 {
						return "", "", nil, errors.New("invalid size band")
					}
					band[field] = n
				}
			}
			if len(band) > 0 {
				sizes[mode+"p"] = band
			}
		}
		if len(sizes) > 0 {
			overrides["sizes"] = sizes
		}
		if v, ok := c["min_score"]; ok {
			overrides["minScore"] = v
		}
		factors := plugin.Config{}
		for _, key := range []string{"hevc", "avc", "av1"} {
			if v, ok := c[key]; ok {
				n, _ := v.(float64)
				if n < 0 || n > 100 {
					return "", "", nil, errors.New("invalid codec factor")
				}
				factors[key] = v
			}
		}
		if len(factors) > 0 {
			overrides["codecFactors"] = factors
		}
		for _, key := range []string{"trusted_groups", "denied_groups"} {
			if value, ok := c[key].(string); ok {
				list := []string{}
				for _, item := range strings.Split(value, ",") {
					item = strings.TrimSpace(item)
					if item != "" {
						if len(item) > 100 {
							return "", "", nil, errors.New("invalid group name")
						}
						list = append(list, item)
					}
				}
				target := "trustedGroups"
				if key == "denied_groups" {
					target = "deniedGroups"
				}
				overrides[target] = list
			}
		}
		return "/archive/selection", "PUT", plugin.Config{"overrides": overrides}, nil
	}
	return "", "", nil, plugin.ErrNotFound
}
