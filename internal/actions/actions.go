// Package actions resolves UI restarts through the existing execallow operations.
package actions

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/deps"
	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

const LogLimit = 100

var errTargetMissing = errors.New("target is not in latest target host inventory")

type Operation struct {
	Command string            `json:"command"`
	Args    map[string]string `json:"args"`
}
type Impact struct {
	Target string `json:"target"`
	deps.Dependent
}
type Preview struct {
	ContainerSetHash string      `json:"container_set_hash"`
	Kind             string      `json:"kind"`
	Target           string      `json:"target"`
	Containers       []string    `json:"containers"`
	Impact           []Impact    `json:"impact"`
	Operations       []Operation `json:"operations"`
}
type Stack struct {
	Name       string   `json:"name"`
	Containers []string `json:"containers"`
}

// Stacks lists only compose projects with containers in the target inventory.
func Stacks(inv *inventory.Inventory) []Stack {
	out := []Stack{}
	if inv == nil || inv.Docker == nil || !inv.Docker.Available {
		return out
	}
	groups := map[string][]string{}
	for _, c := range inv.Docker.Containers {
		if c.ComposeProject != "" {
			groups[c.ComposeProject] = append(groups[c.ComposeProject], c.Name)
		}
	}
	for name, containers := range groups {
		sort.Strings(containers)
		out = append(out, Stack{name, containers})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve uses compose project labels for membership and the deps graph for order.
// Restarts providers before dependents, with lexical order for cycles.
func Resolve(inv *inventory.Inventory, kind, target string, policy execallow.Policy) (Preview, error) {
	p := Preview{ContainerSetHash: ContainerSetHash(nil), Kind: kind, Target: target, Containers: []string{}, Impact: []Impact{}, Operations: []Operation{}}
	if policy.CheckInventory(inv) != nil || inv.Docker == nil || !inv.Docker.Available || inv.Docker.Containers == nil {
		return p, errors.New("latest target host Docker inventory unavailable")
	}
	switch kind {
	case "container_restart":
		c, err := execallow.FindContainer(inv, target)
		if err != nil {
			return p, fmt.Errorf("%w: %v", errTargetMissing, err)
		}
		p.Containers = append(p.Containers, c.Name)
	case "stack_restart":
		for _, c := range inv.Docker.Containers {
			if c.ComposeProject == target && target != "" {
				if _, err := execallow.FindContainer(inv, c.Name); err != nil {
					return p, err
				}
				p.Containers = append(p.Containers, c.Name)
			}
		}
		if len(p.Containers) == 0 {
			return p, errTargetMissing
		}
	default:
		return p, errors.New("unknown action kind")
	}
	p.ContainerSetHash = ContainerSetHash(p.Containers)
	sort.Strings(p.Containers)
	graph := deps.Build(inv)
	selected := map[string]bool{}
	for _, name := range p.Containers {
		selected[name] = true
	}
	for _, n := range graph.Nodes {
		if selected[n.Name] {
			for _, d := range n.Dependents {
				if d.Kind != deps.KindProject {
					if d.Kind == deps.KindDeclared {
						d.Effect = "depends on this container; may be briefly unavailable"
					}
					p.Impact = append(p.Impact, Impact{n.Name, d})
				}
			}
		}
	}
	remaining := append([]string{}, p.Containers...)
	ordered := []string{}
	for len(remaining) > 0 {
		next := -1
		for i, name := range remaining {
			blocked := false
			for _, impact := range p.Impact {
				if impact.Name == name && selected[impact.Target] {
					blocked = true
					break
				}
			}
			if !blocked {
				next = i
				break
			}
		}
		if next < 0 {
			next = 0
		} // deterministic cycle fallback
		name := remaining[next]
		delete(selected, name)
		ordered = append(ordered, name)
		remaining = append(remaining[:next], remaining[next+1:]...)
	}
	p.Containers = ordered
	for _, name := range ordered {
		p.Operations = append(p.Operations, Operation{"container-restart", map[string]string{"container": name}})
	}
	if kind == "stack_restart" {
		if file := policy.ComposeFiles[target]; file != "" {
			p.Operations = []Operation{{"compose-restart", map[string]string{"compose_file": file, "project": target}}}
		}
	}
	return p, nil
}

// ContainerSetHash hashes only sorted container names, independently of restart order.
func ContainerSetHash(containers []string) string {
	names := append([]string{}, containers...)
	sort.Strings(names)
	raw, _ := json.Marshal(names)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// PreviewChanged returns the current plan for another confirmation.
type PreviewChanged struct {
	Preview Preview `json:"preview"`
}

func (e *PreviewChanged) Error() string { return "container set changed; review the new preview" }

type ContainerResult struct {
	Container string `json:"container"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

type Entry struct {
	At         time.Time         `json:"at"`
	Actor      string            `json:"actor"`
	Kind       string            `json:"kind"`
	Target     string            `json:"target"`
	Containers []string          `json:"containers"`
	Result     string            `json:"result"`
	ExitCode   int               `json:"exit_code"`
	Results    []ContainerResult `json:"results"`
}

// Manager keeps one audit row per action, including an interrupted self-restart.
// Rows are stored atomically; a running row is replaced on completion.
type Manager struct {
	mu      sync.Mutex
	runner  *execallow.Runner
	path    string
	entries []Entry
}

func Open(dir string, runner *execallow.Runner) (*Manager, error) {
	m := &Manager{runner: runner, path: filepath.Join(dir, "actions-audit.jsonl"), entries: []Entry{}}
	f, err := os.Open(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var e Entry
		if err := json.Unmarshal(scan.Bytes(), &e); err != nil {
			return nil, err
		}
		m.entries = append(m.entries, e)
	}

	return m, scan.Err()
}
func (m *Manager) save() error {
	f, err := os.CreateTemp(filepath.Dir(m.path), ".actions-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	enc := json.NewEncoder(f)
	for _, e := range m.entries {
		if err = enc.Encode(e); err != nil {
			break
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), m.path)
	}
	return err
}
func (m *Manager) Execute(ctx context.Context, kind, target, actor string, confirmedHash string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runner == nil || m.runner.Inventory == nil {
		return Entry{}, errors.New("restart execution unavailable")
	}
	inv, err := m.runner.Inventory()
	if err != nil {
		return Entry{}, errors.New("latest target host inventory unavailable")
	}
	p, err := Resolve(inv, kind, target, m.runner.Policy)
	if err != nil {
		if errors.Is(err, errTargetMissing) && confirmedHash != "" {
			return Entry{}, &PreviewChanged{Preview: p}
		}
		return Entry{}, err
	}
	if confirmedHash != p.ContainerSetHash {
		return Entry{}, &PreviewChanged{Preview: p}
	}

	e := Entry{At: time.Now().UTC(), Actor: actor, Kind: kind, Target: target, Containers: p.Containers, Result: "running", ExitCode: -1, Results: []ContainerResult{}}
	m.entries = append(m.entries, e)

	if err = m.save(); err != nil {
		m.entries = m.entries[:len(m.entries)-1]
		return Entry{}, fmt.Errorf("action audit unavailable: %w", err)
	}
	e.Result = "ok"
	e.ExitCode = 0
	restarted := 0
	stopped := false
	for _, op := range p.Operations {
		result := ContainerResult{Container: op.Args["container"], Status: "skipped"}
		if op.Command == "compose-restart" {
			result.Container = "stack:" + op.Args["project"]
		}
		if !stopped {
			res := m.runner.Run(ctx, op.Command, op.Args)
			if res.ExitCode != 0 || res.Error != "" {
				result.Status = "failed"
				result.Error = strings.TrimSpace(res.Error)
				if result.Error == "" {
					result.Error = strings.TrimSpace(res.Stderr)
				}
				if result.Error == "" {
					result.Error = fmt.Sprintf("restart exited with code %d", res.ExitCode)
				}
				result.Error = strings.Join(strings.Fields(result.Error), " ")
				if len(result.Error) > 240 {
					result.Error = result.Error[:240]
				}
				e.ExitCode = res.ExitCode
				e.Result = "failed"
				if restarted > 0 {
					e.Result = "partial"
				}
				stopped = true
			} else {
				result.Status = "restarted"
				restarted++
			}
		}
		e.Results = append(e.Results, result)
	}
	m.entries[len(m.entries)-1] = e
	if err = m.save(); err != nil {
		return e, fmt.Errorf("action completed but audit update failed: %w", err)
	}
	return e, nil
}
func (m *Manager) Log(limit int) []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > LogLimit {
		limit = LogLimit
	}
	if limit > len(m.entries) {
		limit = len(m.entries)
	}
	out := make([]Entry, limit)
	for i := range out {
		out[i] = m.entries[len(m.entries)-1-i]
	}
	return out
}
