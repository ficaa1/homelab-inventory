package archive

import (
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"net/url"
	"strconv"
	"strings"
)

func (p *Integration) collection(name string, data any, q url.Values) plugin.View {
	object, ok := data.(map[string]any)
	if !ok {
		return p.render(name, data)
	}
	items, ok := object["items"].([]any)
	if !ok {
		return p.render(name, data)
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 0 || page > 100000 {
		page = 0
	}
	start := page * 100
	if start > len(items) {
		start = len(items)
	}
	end := start + 100
	if end > len(items) {
		end = len(items)
	}
	columns := []plugin.Column{}
	keys := []string{"kind", "id", "title", "year", "sizeBytes", "quality", "videoCodec", "archiveStatus", "tags"}
	if name == "seeding" {
		keys = []string{"name", "hash", "mode", "size", "NVMeBytesFreed", "NASBytesCost", "refusal", "planHash"}
	}
	for _, key := range keys {
		format := ""
		if key == "sizeBytes" || key == "size" || key == "NVMeBytesFreed" || key == "NASBytesCost" {
			format = "bytes"
		}
		columns = append(columns, plugin.Column{Key: key, Label: key, Format: format})
	}
	rows := []map[string]any{}
	for _, raw := range items[start:end] {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		row := map[string]any{}
		for _, key := range keys {
			value := item[key]
			switch x := value.(type) {
			case string:
				if p.token != "" {
					x = strings.ReplaceAll(x, p.token, "[redacted]")
				}
				row[key] = strings.NewReplacer("<", " ", ">", " ").Replace(x)
			case map[string]any:
				if key == "archiveStatus" {
					row[key] = fmt.Sprint(x["status"])
				} else {
					row[key] = fmt.Sprintf("Archive: %v · 4K: %v", x["archive"], x["fourK"])
				}
			default:
				row[key] = value
			}
		}
		rows = append(rows, row)
	}
	v := plugin.View{Title: name, UpdatedAt: p.host.Now(), RefreshSeconds: 30, Actions: actions(), Blocks: []plugin.Block{{Type: "table", Title: fmt.Sprintf("%s · %d–%d of %d", name, start+1, end, len(items)), Empty: "No entries", Columns: columns, Rows: rows}}}
	links := []plugin.Item{}
	if page > 0 {
		links = append(links, plugin.Item{Title: "Previous page", Link: &plugin.Link{View: name, Params: map[string]string{"page": strconv.Itoa(page - 1)}}})
	}
	if end < len(items) {
		links = append(links, plugin.Item{Title: "Next page", Link: &plugin.Link{View: name, Params: map[string]string{"page": strconv.Itoa(page + 1)}}})
	}
	if len(links) > 0 {
		v.Blocks = append(v.Blocks, plugin.Block{Type: "list", Items: links})
	}
	if name == "seeding" {
		state := map[string]any{"job": object["job"], "queue": object["queue"], "queueResults": object["queueResults"]}
		details := p.render("Queue and current job", state)
		v.Blocks = append(v.Blocks, details.Blocks...)
	}
	return v
}
