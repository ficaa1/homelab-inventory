package plugin

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/mediadata"
	"reflect"
	"time"
)

type mediaRefreshCache struct {
	at     time.Time
	config Config
	result any
}

// Media calls cross a typed seam while retaining the lifecycle lock and HTTP guard.
func (r *Registry) MediaCapabilities() mediadata.Capabilities {
	e := r.entries["media"]
	if e == nil {
		return mediadata.Capabilities{}
	}
	e.op.Lock()
	defer e.op.Unlock()
	if !e.stored.Enabled || !e.ready {
		return mediadata.Capabilities{}
	}
	if m, ok := e.plugin.(mediadata.Source); ok {
		return m.TVCapabilities()
	}
	return mediadata.Capabilities{}
}
func (r *Registry) MediaRead(ctx context.Context, op, kind string, id int, q string) (any, error) {
	e := r.entries["media"]
	if e == nil {
		return nil, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	m, ok := e.plugin.(mediadata.Source)
	if !ok || !e.stored.Enabled || !e.ready {
		return nil, ErrNotFound
	}
	if op == "refresh" && e.mediaRefresh != nil && e.host.Now().Sub(e.mediaRefresh.at) < 30*time.Second && reflect.DeepEqual(e.mediaRefresh.config, e.config) {
		return e.mediaRefresh.result, nil
	}
	value, err := m.TVRead(ctx, op, kind, id, q)
	if op == "refresh" && err == nil {
		e.mediaRefresh = &mediaRefreshCache{at: e.host.Now(), config: cloneConfig(e.config), result: value}
	}
	return value, err
}
func (r *Registry) MediaRequest(ctx context.Context, p mediadata.RequestPolicy, v mediadata.Request) (mediadata.RequestResult, error) {
	e := r.entries["media"]
	if e == nil {
		return mediadata.RequestResult{}, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	m, ok := e.plugin.(mediadata.Source)
	if !ok || !e.stored.Enabled || !e.ready {
		return mediadata.RequestResult{}, ErrNotFound
	}
	return m.TVRequest(ctx, p, v)
}
func (r *Registry) MediaOptions(ctx context.Context) (mediadata.Options, error) {
	e := r.entries["media"]
	if e == nil {
		return mediadata.Options{}, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	m, ok := e.plugin.(mediadata.Source)
	if !ok || !e.stored.Enabled || !e.ready {
		return mediadata.Options{}, ErrNotFound
	}
	return m.TVOptions(ctx)
}

// DuskSettings returns only the typed TV configuration under the lifecycle lock.
func (r *Registry) DuskSettings() (DuskConfiguration, bool) {
	e := r.entries["dusk"]
	if e == nil {
		return DuskConfiguration{}, false
	}
	e.op.Lock()
	defer e.op.Unlock()
	n, _ := e.config["server_name"].(string)
	return DuskConfiguration{ServerName: n, Discovery: e.config["discovery"] == true, Taste: e.config["taste"] == true, Requests: e.config["requests"] == true}, e.stored.Enabled && e.ready
}

func (r *Registry) MediaProfiles(ctx context.Context) ([]mediadata.Choice, error) {
	e := r.entries["media"]
	if e == nil {
		return []mediadata.Choice{}, nil
	}
	e.op.Lock()
	defer e.op.Unlock()
	m, ok := e.plugin.(mediadata.ProfileSource)
	if !ok || !e.stored.Enabled || !e.ready {
		return []mediadata.Choice{}, nil
	}
	return m.TVProfiles(ctx)
}
func (r *Registry) MediaHistory(ctx context.Context, id int) ([]mediadata.Title, error) {
	e := r.entries["media"]
	if e == nil {
		return nil, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	m, ok := e.plugin.(mediadata.ProfileSource)
	if !ok || !e.stored.Enabled || !e.ready {
		return nil, ErrNotFound
	}
	return m.TVHistory(ctx, id)
}

// DuskConfiguration contains no upstream addresses or credentials.
type DuskConfiguration struct {
	ServerName                 string
	Discovery, Taste, Requests bool
}
