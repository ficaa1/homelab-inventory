package plugin

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/engine"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

// BindTasteSource supplies the typed Media bridge once the registry is constructed.
func (r *Registry) BindTasteSource() {
	if e := r.entries["taste"]; e != nil {
		if p, ok := e.plugin.(interface{ SetRecommendationSource(tastedata.Source) }); ok {
			p.SetRecommendationSource(r.RecommendationSource())
		}
	}
}

// TasteDiscovery accepts only a household identity already authenticated by core.
func (r *Registry) TasteDiscovery(ctx context.Context, profile string, refresh bool) (engine.Report, error) {
	e := r.entries["taste"]
	if e == nil {
		return engine.Report{}, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	p, ok := e.plugin.(interface {
		Discovery(context.Context, string, bool) (engine.Report, error)
	})
	if !ok || !e.stored.Enabled || !e.ready {
		return engine.Report{}, ErrNotFound
	}
	return p.Discovery(ctx, profile, refresh)
}
func (r *Registry) TasteFeedback(ctx context.Context, profile, kind string, id int, verdict, title, year string) error {
	e := r.entries["taste"]
	if e == nil {
		return ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	p, ok := e.plugin.(interface {
		Feedback(context.Context, string, string, int, string, string, string) error
	})
	if !ok || !e.stored.Enabled || !e.ready {
		return ErrNotFound
	}
	return p.Feedback(ctx, profile, kind, id, verdict, title, year)
}
func (r *Registry) TasteHidden(ctx context.Context, profile, kind string, id int, hidden bool) error {
	e := r.entries["taste"]
	if e == nil {
		return ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	p, ok := e.plugin.(interface {
		Hidden(context.Context, string, string, int, bool) error
	})
	if !ok || !e.stored.Enabled || !e.ready {
		return ErrNotFound
	}
	return p.Hidden(ctx, profile, kind, id, hidden)
}

func (r *Registry) TasteBound(profile string) bool {
	e := r.entries["taste"]
	if e == nil {
		return false
	}
	e.op.Lock()
	defer e.op.Unlock()
	p, ok := e.plugin.(interface{ Bound(string) bool })
	return ok && e.stored.Enabled && e.ready && p.Bound(profile)
}
func (r *Registry) TastePicks(ctx context.Context, profile, kind string, familiar bool) (any, error) {
	e := r.entries["taste"]
	if e == nil {
		return nil, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	p, ok := e.plugin.(interface {
		Picks(context.Context, string, string, bool) (any, error)
	})
	if !ok || !e.stored.Enabled || !e.ready {
		return nil, ErrNotFound
	}
	return p.Picks(ctx, profile, kind, familiar)
}

func (r *Registry) TasteHiddenKeys(profile string) ([]string, error) {
	e := r.entries["taste"]
	if e == nil {
		return nil, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	p, ok := e.plugin.(interface {
		HiddenKeys(string) ([]string, error)
	})
	if !ok || !e.stored.Enabled || !e.ready {
		return nil, ErrNotFound
	}
	return p.HiddenKeys(profile)
}
func (r *Registry) TasteSimilar(ctx context.Context, profile, kind string, id int) (engine.Report, error) {
	e := r.entries["taste"]
	if e == nil {
		return engine.Report{}, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	p, ok := e.plugin.(interface {
		Similar(context.Context, string, string, int) (engine.Report, error)
	})
	if !ok || !e.stored.Enabled || !e.ready {
		return engine.Report{}, ErrNotFound
	}
	return p.Similar(ctx, profile, kind, id)
}
