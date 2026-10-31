package taste

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
)

// Upload targets bind to configured household identities rather than caller IDs.
func (t *Integration) UploadTargets() []plugin.UploadTarget {
	ids := []string{}
	for id := range t.bindings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []plugin.UploadTarget{}
	for _, id := range ids {
		out = append(out, plugin.UploadTarget{ID: "letterboxd-" + id, Label: "Letterboxd · " + id, Help: "Private export ZIP; only ratings, watched, likes and watchlist are read", Accept: []string{".zip"}, MaxBytes: 16 << 20})
	}
	return out
}
func (t *Integration) Upload(ctx context.Context, target, name string, size int64, r io.Reader) (plugin.UploadResult, error) {
	id, ok := strings.CutPrefix(target, "letterboxd-")
	if !ok || size > 16<<20 {
		return plugin.UploadResult{}, errors.New("invalid preference upload")
	}
	h, e := t.household(id)
	if e != nil {
		return plugin.UploadResult{}, e
	}
	b, e := io.ReadAll(io.LimitReader(r, (16<<20)+1))
	if e != nil || len(b) > 16<<20 {
		return plugin.UploadResult{}, errors.New("preference upload too large")
	}
	p, e := letterboxd.Parse(b, t.host.Now())
	if e != nil {
		return plugin.UploadResult{}, e
	}
	p.Owner = h.binding.Owner
	p, e = letterboxd.Resolve(ctx, p, guardedCatalog{Source: t.source, host: t.host, endpoint: t.endpoint})
	if e != nil {
		return plugin.UploadResult{}, e
	}
	if e = letterboxd.Save(h.model.ProfilePath, p); e != nil {
		return plugin.UploadResult{}, e
	}
	h.model.Refresh()
	return plugin.UploadResult{Name: "Letterboxd export", Message: "Preferences imported; model refreshes in the background"}, nil
}
