// Package plugin hosts compiled-in integrations and their declarative views.
package plugin

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

type FieldType string

const (
	URL    FieldType = "url"
	Secret FieldType = "secret"
	Text   FieldType = "text"
	Number FieldType = "number"
	Bool   FieldType = "bool"
	Select FieldType = "select"
)

type Field struct {
	Key         string    `json:"key"`
	Label       string    `json:"label"`
	Help        string    `json:"help,omitempty"`
	Type        FieldType `json:"type"`
	Required    bool      `json:"required,omitempty"`
	Default     any       `json:"default,omitempty"`
	Placeholder string    `json:"placeholder,omitempty"`
	Options     []Option  `json:"options,omitempty"`
	Group       string    `json:"group,omitempty"`
}
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
type ViewRef struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}
type Tab struct {
	Title string    `json:"title"`
	Icon  string    `json:"icon"`
	Views []ViewRef `json:"views"`
}
type Manifest struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Version     string  `json:"version"`
	Settings    []Field `json:"settings"`
	Tab         *Tab    `json:"tab"`
}
type Config map[string]any

type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}
type TestResult struct {
	OK      bool    `json:"ok"`
	Message string  `json:"message"`
	Checks  []Check `json:"checks"`
}
type Plugin interface {
	Manifest() Manifest
	Configure(Config) error
	Test(context.Context) TestResult
	View(context.Context, string, url.Values) (View, error)
	Close() error
}
type AlertSource interface {
	Alerts(context.Context) ([]Alert, error)
}
type ImageSource interface {
	Image(context.Context, string) (string, []byte, error)
}
type Alert struct{ Rule, Subject, Severity, Summary, Detail string }
type Host interface {
	HTTP() *http.Client
	CacheDir() string
	Logf(string, ...any)
	Now() time.Time
}

// Builtin binds a stable ID to a constructor; registration is explicit.
type Builtin struct {
	ID  string
	New func(Host) Plugin
}

// FieldError associates a rejected configuration with a form field.
type FieldError struct {
	Key     string
	Message string
}

func (e *FieldError) Error() string { return e.Message }

// ValidationError is safe to return to clients: values are never interpolated.
type ValidationError struct {
	Message string            `json:"error"`
	Fields  map[string]string `json:"fields"`
}

func (e *ValidationError) Error() string { return e.Message }
