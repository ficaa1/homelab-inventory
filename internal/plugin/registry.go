package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

var (
	ErrNotFound = errors.New("plugin or view not found")
	ErrBusy     = errors.New("a plugin test is already running")
)

type storedPlugin struct {
	Enabled   bool      `json:"enabled"`
	Settings  Config    `json:"settings"`
	UpdatedAt time.Time `json:"updated_at"`
}
type stateFile struct {
	Plugins map[string]json.RawMessage `json:"plugins"`
}
type entry struct {
	op             sync.Mutex // serializes calls on a plugin instance
	mu             sync.Mutex // protects cached status independently of live calls
	test           chan struct{}
	plugin         Plugin
	manifest       Manifest
	host           *host
	stored         storedPlugin
	config         Config
	ready          bool
	state, message string
	checked        *time.Time
	failures       int
	alerts         []Alert
}
type Registry struct {
	entries map[string]*entry // immutable after Open
	mu      sync.Mutex        // protects file, including unknown entries
	file    stateFile
	path    string
	wake    chan struct{}
}
type ItemStatus struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	Version        string     `json:"version"`
	Enabled        bool       `json:"enabled"`
	State          string     `json:"state"`
	Message        string     `json:"message"`
	CheckedAt      *time.Time `json:"checked_at"`
	Tab            *Tab       `json:"tab"`
	SettingsSchema []Field    `json:"settings_schema"`
	Settings       Config     `json:"settings"`
}
type Capability struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Tab  *Tab   `json:"tab"`
}
type Update struct {
	Enabled  *bool  `json:"enabled,omitempty"`
	Settings Config `json:"settings,omitempty"`
}

// Open loads state without discarding integrations absent from this build.
// Built-ins start disabled; only persisted enablement activates them.
func Open(data string, builtins []Builtin, logger *log.Logger) (r *Registry, err error) {
	r = &Registry{entries: map[string]*entry{}, path: filepath.Join(data, "state-plugins.json"), file: stateFile{Plugins: map[string]json.RawMessage{}}, wake: make(chan struct{}, 1)}
	if err = os.MkdirAll(data, 0700); err != nil {
		return nil, errors.New("creating plugin data directory")
	}
	bytes, err := os.ReadFile(r.path)
	if err == nil {
		if json.Unmarshal(bytes, &r.file) != nil || r.file.Plugins == nil {
			return nil, errors.New("invalid plugin state file")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("reading plugin state file")
	}
	defer func(opened *Registry) {
		if err != nil {
			opened.Close()
		}
	}(r)
	for _, b := range builtins {
		if !idPattern.MatchString(b.ID) || b.New == nil || r.entries[b.ID] != nil {
			return nil, errors.New("invalid or duplicate builtin plugin")
		}
		h := newHost(data, b.ID, logger)
		if err = os.MkdirAll(h.dir, 0700); err != nil {
			return nil, errors.New("creating plugin cache directory")
		}
		if err = os.Chmod(h.dir, 0700); err != nil {
			return nil, errors.New("securing plugin cache directory")
		}
		p := b.New(h)
		if p == nil {
			return nil, errors.New("nil builtin plugin")
		}
		e := &entry{plugin: p, manifest: p.Manifest(), host: h, test: make(chan struct{}, 1), state: "disabled", stored: storedPlugin{Settings: Config{}}}
		r.entries[b.ID] = e
		if e.manifest.ID != b.ID {
			return nil, errors.New("builtin plugin ID mismatch")
		}
		if err = validateManifest(e.manifest); err != nil {
			return nil, err
		}
		if raw, ok := r.file.Plugins[b.ID]; ok {
			if json.Unmarshal(raw, &e.stored) != nil {
				return nil, errors.New("invalid stored plugin settings")
			}
		}
		e.config, err = resolve(e.manifest, e.stored.Settings, e.stored.Enabled)
		if err != nil {
			e.state = "error"
			var invalid *ValidationError
			if errors.As(err, &invalid) {
				for _, msg := range invalid.Fields {
					if msg == "is required" {
						e.state = "unconfigured"
					}
				}
			}
			e.message = "invalid or missing plugin settings"
			err = nil
			if !e.stored.Enabled {
				e.state = "disabled"
			}
			continue
		}
		if e.stored.Enabled {
			e.host.configure(e.manifest, e.config)
			if configureErr := p.Configure(cloneConfig(e.config)); configureErr != nil {
				e.state = "error"
				e.message = h.safe(configureErr.Error())
			} else {
				e.ready = true
				e.state = "ok"
			}
		}
	}
	return r, nil
}
func (r *Registry) ids() []string {
	ids := make([]string, 0, len(r.entries))
	for id := range r.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func (e *entry) status() ItemStatus {
	schema := e.manifest.Settings
	if schema == nil {
		schema = []Field{}
	}
	return ItemStatus{ID: e.manifest.ID, Name: e.manifest.Name, Description: e.manifest.Description, Version: e.manifest.Version, Enabled: e.stored.Enabled, State: e.state, Message: e.message, CheckedAt: e.checked, Tab: e.manifest.Tab, SettingsSchema: schema, Settings: masked(e.manifest, e.config)}
}
func (r *Registry) List() []ItemStatus {
	out := []ItemStatus{}
	for _, id := range r.ids() {
		e := r.entries[id]
		e.mu.Lock()
		out = append(out, e.status())
		e.mu.Unlock()
	}
	r.mu.Lock()
	for id, raw := range r.file.Plugins {
		if r.entries[id] != nil {
			continue
		}
		var s storedPlugin
		_ = json.Unmarshal(raw, &s)
		out = append(out, ItemStatus{ID: id, Name: id, Enabled: s.Enabled, State: "unavailable", Message: "plugin is not included in this build", SettingsSchema: []Field{}, Settings: Config{}})
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (r *Registry) Capabilities() []Capability {
	out := []Capability{}
	for _, id := range r.ids() {
		e := r.entries[id]
		e.mu.Lock()
		if e.stored.Enabled && e.manifest.Tab != nil {
			out = append(out, Capability{ID: id, Name: e.manifest.Name, Tab: e.manifest.Tab})
		}
		e.mu.Unlock()
	}
	return out
}
func (r *Registry) persist(id string, next storedPlugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, err := json.Marshal(next)
	if err != nil {
		return errors.New("encoding plugin settings")
	}
	file := stateFile{Plugins: map[string]json.RawMessage{}}
	for k, v := range r.file.Plugins {
		file.Plugins[k] = v
	}
	file.Plugins[id] = raw
	data, err := json.Marshal(file)
	if err != nil {
		return errors.New("encoding plugin state")
	}
	if err = fsx.WriteAtomic(r.path, data, 0600); err != nil {
		return errors.New("saving plugin settings")
	}
	r.file = file
	return nil
}
func (e *entry) configError(err error) error {
	fields := map[string]string{}
	var field *FieldError
	if errors.As(err, &field) {
		for _, f := range e.manifest.Settings {
			if f.Key == field.Key {
				fields[field.Key] = e.host.safe(field.Message)
			}
		}
	}
	return &ValidationError{Message: e.host.safe(err.Error()), Fields: fields}
}
func (e *entry) restore() {
	e.host.configure(e.manifest, e.config)
	if e.ready {
		if err := e.plugin.Configure(cloneConfig(e.config)); err != nil {
			e.ready = false
			e.state = "error"
			e.message = e.host.safe(err.Error())
		}
	} else {
		_ = e.plugin.Close()
	}
}
func (r *Registry) Update(id string, u Update) (ItemStatus, error) {
	e := r.entries[id]
	if e == nil {
		return ItemStatus{}, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	next := e.stored
	next.Settings = Merge(next.Settings, u.Settings)
	if u.Enabled != nil {
		next.Enabled = *u.Enabled
	}
	cfg, err := resolve(e.manifest, next.Settings, next.Enabled)
	if err != nil {
		return ItemStatus{}, err
	}
	// A temporary host scope permits Configure to inspect dependencies but no
	// network I/O is allowed by the plugin contract during Configure.
	e.host.configure(e.manifest, cfg)
	if err = e.plugin.Configure(cloneConfig(cfg)); err != nil {
		safe := e.configError(err)
		e.restore()
		e.message = safe.Error()
		if e.stored.Enabled {
			e.state = "error"
		}
		return ItemStatus{}, safe
	}
	next.UpdatedAt = e.host.Now().UTC()
	if err = r.persist(id, next); err != nil {
		e.restore()
		return ItemStatus{}, err
	}
	e.stored = next
	e.config = cfg
	e.ready = next.Enabled
	e.message = ""
	e.failures = 0
	e.checked = nil
	if next.Enabled {
		e.state = "ok"
	} else {
		e.state = "disabled"
		e.alerts = nil
		if err = e.plugin.Close(); err != nil {
			e.message = e.host.safe(err.Error())
		}
		e.host.configure(e.manifest, nil)
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return e.status(), nil
}
func (r *Registry) Test(ctx context.Context, id string, patch Config) (TestResult, error) {
	e := r.entries[id]
	if e == nil {
		return TestResult{}, ErrNotFound
	}
	select {
	case e.test <- struct{}{}:
		defer func() { <-e.test }()
	default:
		return TestResult{}, ErrBusy
	}
	e.op.Lock()
	defer e.op.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	cfg, err := resolve(e.manifest, Merge(e.stored.Settings, patch), true)
	if err != nil {
		return TestResult{}, err
	}
	e.host.configure(e.manifest, cfg)
	defer e.restore()
	if err = e.plugin.Configure(cloneConfig(cfg)); err != nil {
		safe := e.configError(err)
		e.message = safe.Error()
		if e.stored.Enabled {
			e.state = "error"
		}
		return TestResult{}, safe
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	e.mu.Unlock()
	result := e.plugin.Test(ctx)
	e.mu.Lock()
	if ctx.Err() != nil {
		result = TestResult{Message: "plugin test timed out"}
	}
	result.Message = e.host.safe(result.Message)
	for i := range result.Checks {
		result.Checks[i].Name = e.host.safe(result.Checks[i].Name)
		result.Checks[i].Message = e.host.safe(result.Checks[i].Message)
	}
	if result.Checks == nil {
		result.Checks = []Check{}
	}
	at := e.host.Now().UTC()
	e.checked = &at
	if result.OK {
		e.message = ""
		if e.ready {
			e.state = "ok"
		}
	} else {
		e.message = result.Message
		if e.stored.Enabled {
			e.state = "error"
		}
	}
	return result, nil
}
func (r *Registry) View(ctx context.Context, id, name string, params url.Values) ([]byte, error) {
	e := r.entries[id]
	if e == nil {
		return nil, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	if !e.stored.Enabled {
		return nil, ErrNotFound
	}
	if !e.ready {
		return nil, errors.New(e.message)
	}
	found := false
	if e.manifest.Tab != nil {
		for _, v := range e.manifest.Tab.Views {
			found = found || v.Name == name
		}
	}
	if !found {
		return nil, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := e.plugin.View(ctx, name, params)
	if err != nil {
		return nil, errors.New(e.host.safe(err.Error()))
	}
	if ctx.Err() != nil {
		return nil, errors.New("plugin view timed out")
	}
	return ValidateView(v, e.manifest)
}
func (r *Registry) Image(ctx context.Context, id, ref string) (string, []byte, error) {
	e := r.entries[id]
	if e == nil || !refPattern.MatchString(ref) {
		return "", nil, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	src, ok := e.plugin.(ImageSource)
	if !ok || !e.stored.Enabled {
		return "", nil, ErrNotFound
	}
	if !e.ready {
		return "", nil, errors.New(e.message)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ct, body, err := src.Image(ctx, ref)
	if err != nil {
		return "", nil, errors.New(e.host.safe(err.Error()))
	}
	if ctx.Err() != nil {
		return "", nil, errors.New("plugin image timed out")
	}
	return validateImage(ct, body)
}
func (r *Registry) Close() {
	for _, e := range r.entries {
		e.op.Lock()
		e.mu.Lock()
		_ = e.plugin.Close()
		e.host.transport.CloseIdleConnections()
		e.mu.Unlock()
		e.op.Unlock()
	}
}
