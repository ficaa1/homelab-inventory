package plugin

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// ActionSpec is compiled into a plugin; callers cannot select an upstream path.
// Operator actions require the separate operator credential before dispatch.
type ActionSpec struct {
	Destructive    bool           `json:"destructive,omitempty"`
	Confirm        string         `json:"confirm,omitempty"`
	Values         map[string]any `json:"values,omitempty"`
	Result         string         `json:"result,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	ID             string         `json:"id"`
	Label          string         `json:"label"`
	Fields         []Field        `json:"fields,omitempty"`
	Operator       bool           `json:"operator,omitempty"`
}

func (a ActionSpec) validatePresentation() error {
	if strings.TrimSpace(a.Label) == "" || strings.ContainsAny(a.Label+a.Confirm, "<>") || utf8.RuneCountInString(a.Confirm) > 300 {
		return errors.New("invalid action label or confirmation")
	}
	if a.Result != "" && a.Result != "view" && a.Result != "toast" {
		return errors.New("invalid action result")
	}
	presets := 0
	for _, f := range a.Fields {
		if f.Default != nil && !validActionValue(f, f.Default) {
			return errors.New("invalid action default")
		}
		if value, ok := a.Values[f.Key]; ok {
			presets++
			if !validActionValue(f, value) {
				return errors.New("invalid action preset")
			}
		}
	}
	if presets != len(a.Values) {
		return errors.New("unknown action preset")
	}
	return nil
}

func validActionValue(f Field, v any) bool {
	if f.Type == Secret || (f.Required && isString(v) && strings.TrimSpace(v.(string)) == "") {
		return false
	}
	_, msg := fieldValue(f, v)
	return msg == ""
}

func (a ActionSpec) matches(registered ActionSpec) bool {
	return a.Operator == registered.Operator && a.Destructive == registered.Destructive &&
		a.TimeoutSeconds == registered.TimeoutSeconds && a.Result == registered.Result &&
		slices.EqualFunc(a.Fields, registered.Fields, func(a, b Field) bool {
			a.Default, b.Default = nil, nil
			return reflect.DeepEqual(a, b)
		})
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
		message := err.Error()
		for _, f := range spec.Fields {
			if value, _ := clean[f.Key].(string); f.Type == Secret && value != "" {
				message = strings.ReplaceAll(message, value, "[redacted]")
			}
		}
		message = e.host.safe(message)
		var field *FieldError
		if errors.As(err, &field) && slices.ContainsFunc(spec.Fields, func(f Field) bool { return f.Key == field.Key }) {
			return nil, &ValidationError{Message: message, Fields: map[string]string{field.Key: message}}
		}
		return nil, errors.New(message)
	}
	return ValidateView(v, e.manifest)
}

func actionTimeout(spec ActionSpec) time.Duration {
	if spec.TimeoutSeconds >= 1 && spec.TimeoutSeconds <= 360 {
		return time.Duration(spec.TimeoutSeconds) * time.Second
	}
	return 10 * time.Second
}
