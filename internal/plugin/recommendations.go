package plugin

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

// RecommendationSource retains Media's lifecycle guard around every typed read.
func (r *Registry) RecommendationSource() tastedata.Source { return mediaTaste{r: r} }

type mediaTaste struct{ r *Registry }

func mediaTasteCall[T any](r *Registry, fn func(tastedata.Source) (T, error)) (T, error) {
	var zero T
	e := r.entries["media"]
	if e == nil {
		return zero, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	m, ok := e.plugin.(tastedata.Source)
	if !ok || !e.stored.Enabled || !e.ready {
		return zero, ErrNotFound
	}
	return fn(m)
}
func (m mediaTaste) WatchHistory(ctx context.Context, id int) ([]tastedata.WatchTitle, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) ([]tastedata.WatchTitle, error) { return s.WatchHistory(ctx, id) })
}
func (m mediaTaste) WatchIdentity(ctx context.Context, v tastedata.WatchTitle) (tastedata.BrowseTitle, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) (tastedata.BrowseTitle, error) { return s.WatchIdentity(ctx, v) })
}
func (m mediaTaste) Recommendations(ctx context.Context, k string, id int) ([]tastedata.SuggestedTitle, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) ([]tastedata.SuggestedTitle, error) { return s.Recommendations(ctx, k, id) })
}
func (m mediaTaste) RecommendationsPage(ctx context.Context, k string, id, page int) ([]tastedata.SuggestedTitle, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) ([]tastedata.SuggestedTitle, error) {
		return s.RecommendationsPage(ctx, k, id, page)
	})
}
func (m mediaTaste) RecommendationAvailability(ctx context.Context) (map[string]string, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) (map[string]string, error) { return s.RecommendationAvailability(ctx) })
}
func (m mediaTaste) TitleFacts(ctx context.Context, k string, id int) (tastedata.TitleFacts, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) (tastedata.TitleFacts, error) { return s.TitleFacts(ctx, k, id) })
}
func (m mediaTaste) PersonWork(ctx context.Context, id int) ([]tastedata.SuggestedTitle, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) ([]tastedata.SuggestedTitle, error) { return s.PersonWork(ctx, id) })
}
func (m mediaTaste) Search(ctx context.Context, q string) ([]tastedata.BrowseTitle, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) ([]tastedata.BrowseTitle, error) { return s.Search(ctx, q) })
}
func (m mediaTaste) Details(ctx context.Context, k string, id int) (tastedata.TitleDetail, error) {
	return mediaTasteCall(m.r, func(s tastedata.Source) (tastedata.TitleDetail, error) { return s.Details(ctx, k, id) })
}
