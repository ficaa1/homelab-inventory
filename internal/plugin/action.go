package plugin

import (
	"context"
	"errors"
	"time"
)

// ActionSpec is compiled into a plugin; callers cannot select an upstream path.
// Operator actions require the separate operator credential before dispatch.
type ActionSpec struct {
	TimeoutSeconds int     `json:"timeout_seconds,omitempty"`
	ID             string  `json:"id"`
	Label          string  `json:"label"`
	Fields         []Field `json:"fields,omitempty"`
	Operator       bool    `json:"operator,omitempty"`
}
type ActionSource interface {
	Action(context.Context, string, Config) (View, error)
}

func (r *Registry) ActionSpec(id, action string) (ActionSpec, error) {
	e := r.entries[id]
	if e == nil {
		return ActionSpec{}, ErrNotFound
	}
	for _, a := range e.manifest.Actions {
		if a.ID == action {
			return a, nil
		}
	}
	return ActionSpec{}, ErrNotFound
}

// Action never retries. Unknown outcomes must be reconciled in the upstream service.
func (r *Registry) Action(ctx context.Context, id, action string, confirmed bool, values Config) ([]byte, error) {
	spec, err := r.ActionSpec(id, action)
	if err != nil {
		return nil, err
	}
	if !confirmed {
		return nil, &ValidationError{Message: "explicit confirmation required"}
	}
	for _, field := range spec.Fields {
		if _, present := values[field.Key]; !present && field.Required && field.Default == nil {
			return nil, &ValidationError{Message: "missing action field", Fields: map[string]string{field.Key: "is required"}}
		}
	}
	input := cloneConfig(values)
	for _, field := range spec.Fields {
		if _, present := input[field.Key]; !present {
			input[field.Key] = field.Default
		}
	}
	clean, err := resolve(Manifest{Settings: spec.Fields}, input, true)
	if err != nil {
		return nil, err
	}
	e := r.entries[id]
	e.op.Lock()
	defer e.op.Unlock()
	src, ok := e.plugin.(ActionSource)
	if !ok || !e.stored.Enabled {
		return nil, ErrNotFound
	}
	if !e.ready {
		return nil, errors.New("plugin is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, actionTimeout(spec))
	defer cancel()
	v, err := src.Action(ctx, action, clean)
	if err != nil {
		return nil, errors.New(e.host.safe(err.Error()))
	}
	return ValidateView(v, e.manifest)
}

func actionTimeout(spec ActionSpec) time.Duration {
	if spec.TimeoutSeconds >= 1 && spec.TimeoutSeconds <= 360 {
		return time.Duration(spec.TimeoutSeconds) * time.Second
	}
	return 10 * time.Second
}
