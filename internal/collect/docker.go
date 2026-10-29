package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

// DockerCollector inventories containers and the compose files behind them.
//
// It uses `docker inspect` with narrowly-scoped --format expressions rather
// than the whole inspect document. That keeps the strong property we want —
// container environment variables never leave the daemon, so they cannot leak
// into the inventory — while still getting structured labels and mounts.
//
// The first version parsed labels out of `docker ps --format`'s comma-joined
// string. That format is ambiguous: Portainer's labels contain HTML and JSON
// with embedded commas and equals signs, and the heuristic shredded them into
// nonsense keys. Asking the daemon for real JSON is the fix; there is no
// clever parse that recovers an ambiguous format.
type DockerCollector struct{}

func (DockerCollector) Name() string { return "docker" }

// dockerPS mirrors `docker ps --format '{{json .}}'`. Only the fields that are
// unambiguous in that output are taken from here; labels and mounts come from
// inspect.
type dockerPS struct {
	ID        string `json:"ID"`
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	Command   string `json:"Command"`
	CreatedAt string `json:"CreatedAt"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	Ports     string `json:"Ports"`
	Networks  string `json:"Networks"`
}

// dockerMount mirrors one entry of a container's .Mounts array.
type dockerMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

func (c DockerCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	d := &inventory.Docker{
		Containers:      []inventory.Container{},
		ComposeProjects: []inventory.ComposeProject{},
	}
	inv.Docker = d

	if !env.Run.Available("docker") {
		env.Infof("docker not installed")
		return nil
	}

	res, err := env.Run.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil || !res.OK() {
		detail := res.Stderr
		if err != nil {
			detail = err.Error()
		}
		env.Warnf("docker is installed but the daemon is not reachable: %s", firstLine(detail))
		return nil
	}
	d.Available = true
	d.Version = strings.TrimSpace(res.Stdout)

	res, err = env.Run.Run(ctx, "docker", "ps", "--all", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	if !res.OK() {
		env.Warnf("docker ps exited %d: %s", res.ExitCode, firstLine(res.Stderr))
		return nil
	}

	var records []dockerPS
	var ids []string
	for _, line := range res.Lines() {
		var ps dockerPS
		if err := json.Unmarshal([]byte(line), &ps); err != nil {
			env.WarnErr("could not parse a docker ps record", err)
			continue
		}
		records = append(records, ps)
		ids = append(ids, ps.ID)
	}
	if len(records) == 0 {
		return nil
	}

	labels := inspectLabels(ctx, env, ids)
	mounts := inspectMounts(ctx, env, ids)
	runtime := inspectRuntime(ctx, env, ids)

	for _, ps := range records {
		ct := inventory.Container{
			ID:        shortID(ps.ID),
			Name:      ps.Names,
			Image:     ps.Image,
			State:     ps.State,
			Status:    ps.Status,
			Command:   redact.String(strings.Trim(ps.Command, `"`)),
			CreatedAt: ps.CreatedAt,
			Ports:     splitList(ps.Ports),
			Networks:  splitList(ps.Networks),
			Labels:    redact.Map(labels[ps.ID]),
			Mounts:    []inventory.Mount{},
		}
		for _, m := range mounts[ps.ID] {
			ct.Mounts = append(ct.Mounts, inventory.Mount{
				Type:        m.Type,
				Name:        m.Name,
				Source:      m.Source,
				Destination: m.Destination,
				ReadWrite:   m.RW,
			})
		}
		if r, ok := runtime[ps.ID]; ok {
			ct.RestartCount = r.RestartCount
			ct.NetworkMode = r.NetworkMode
		}
		if l := labels[ps.ID]; l != nil {
			ct.ComposeProject = l["com.docker.compose.project"]
			ct.ComposeService = l["com.docker.compose.service"]
			// Compose records its dependency graph in a label, so the
			// "what breaks if I stop this" question needs no extra command.
			if dep := l["com.docker.compose.depends_on"]; dep != "" {
				ct.DependsOn = splitList(dep)
			}
		}
		d.Containers = append(d.Containers, ct)
	}
	sort.Slice(d.Containers, func(a, b int) bool { return d.Containers[a].Name < d.Containers[b].Name })

	d.ComposeProjects = buildComposeProjects(env, d.Containers, labels, newMountTable(d.Containers))
	return nil
}

// inspectLabels returns labels keyed by full container ID.
//
// The format prints the ID and then compact JSON on one line. Container IDs
// contain no spaces, so splitting on the first space is unambiguous — which is
// exactly the property the old comma-joined label parsing lacked.
func inspectLabels(ctx context.Context, env *Env, ids []string) map[string]map[string]string {
	out := map[string]map[string]string{}
	lines := inspectLines(ctx, env, ids, "{{.Id}} {{json .Config.Labels}}", "labels")
	for id, payload := range lines {
		var m map[string]string
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			env.WarnErr("could not parse labels for "+shortID(id), err)
			continue
		}
		out[id] = m
	}
	return out
}

// runtimeFacts are the two things about a container that only the inspect
// document knows and that nothing else in the schema records.
type runtimeFacts struct {
	RestartCount int    `json:"restart_count"`
	NetworkMode  string `json:"network_mode"`
}

// inspectRuntime returns restart counts and network modes keyed by container ID.
//
// RestartCount turns the crash-loop rule from an inference into a fact: the
// uptime-versus-age heuristic says "this looks like it restarted", and this
// says how many times. NetworkMode is the only place the schema can learn that
// a container reaches the network through another one — five services here go
// through vpnservice, and `docker ps` shows nothing about it.
func inspectRuntime(ctx context.Context, env *Env, ids []string) map[string]runtimeFacts {
	out := map[string]runtimeFacts{}
	lines := inspectLines(ctx, env, ids,
		`{{.Id}} {"restart_count":{{.RestartCount}},"network_mode":"{{.HostConfig.NetworkMode}}"}`,
		"runtime")
	for id, payload := range lines {
		var f runtimeFacts
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			env.WarnErr("could not parse runtime facts for "+shortID(id), err)
			continue
		}
		out[id] = f
	}
	return out
}

func inspectMounts(ctx context.Context, env *Env, ids []string) map[string][]dockerMount {
	out := map[string][]dockerMount{}
	lines := inspectLines(ctx, env, ids, "{{.Id}} {{json .Mounts}}", "mounts")
	for id, payload := range lines {
		var m []dockerMount
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			env.WarnErr("could not parse mounts for "+shortID(id), err)
			continue
		}
		out[id] = m
	}
	return out
}

// inspectLines runs one inspect over every container and returns the payload
// after the leading ID, keyed by ID.
func inspectLines(ctx context.Context, env *Env, ids []string, format, what string) map[string]string {
	args := append([]string{"inspect", "--format", format}, ids...)
	res, err := env.Run.Run(ctx, "docker", args...)
	if err != nil {
		env.WarnErr("docker inspect for "+what+" failed", err)
		return nil
	}
	// inspect exits non-zero if *any* ID is unknown (a container removed
	// between ps and inspect), but still prints the ones it found.
	if !res.OK() {
		env.Warnf("docker inspect for %s exited %d: %s", what, res.ExitCode, firstLine(res.Stderr))
	}

	out := map[string]string{}
	for _, line := range res.Lines() {
		id, payload, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		out[id] = payload
	}
	return out
}

// mountTable maps container-visible paths back to host paths.
//
// This is what makes Portainer-managed stacks legible. Portainer records
// working_dir as /data/compose/<id>, a path that exists only inside the
// Portainer container; on the host it lives under that container's
// portainer_data volume. Rather than special-casing Portainer, we build the
// destination -> source map from every container's mounts and use it to
// rewrite paths that are not reachable as recorded.
type mountTable []mountEntry

type mountEntry struct {
	container   string
	destination string
	source      string
}

func newMountTable(containers []inventory.Container) mountTable {
	var t mountTable
	for _, c := range containers {
		for _, m := range c.Mounts {
			// "/" would match everything and rewrite paths that were already
			// correct; an empty source is unusable.
			if m.Destination == "" || m.Destination == "/" || m.Source == "" {
				continue
			}
			t = append(t, mountEntry{container: c.Name, destination: m.Destination, source: m.Source})
		}
	}
	// Longest destination first, so /data/compose beats /data when both exist.
	sort.Slice(t, func(a, b int) bool {
		if len(t[a].destination) != len(t[b].destination) {
			return len(t[a].destination) > len(t[b].destination)
		}
		return t[a].container < t[b].container
	})
	return t
}

// resolve rewrites a container-visible path to a host path.
//
// It only reports success when the rewritten path actually exists. Without
// that check a coincidental mount prefix would silently point the inventory at
// the wrong file, which is worse than reporting the file as unreachable.
func (t mountTable) resolve(env *Env, path string) (hostPath, via string, ok bool) {
	for _, e := range t {
		if path != e.destination && !strings.HasPrefix(path, e.destination+"/") {
			continue
		}
		cand := e.source + strings.TrimPrefix(path, e.destination)
		if _, err := os.Stat(env.Path(cand)); err == nil {
			return cand, fmt.Sprintf("mount %s:%s", e.container, e.destination), true
		}
	}
	return "", "", false
}

// buildComposeProjects groups containers by compose project label and pulls in
// the compose files that produced them. Those files are the closest thing to
// documentation this server has, so capturing them is most of the value of
// this collector.
func buildComposeProjects(env *Env, containers []inventory.Container, labels map[string]map[string]string, table mountTable) []inventory.ComposeProject {
	type acc struct {
		workingDir string
		files      map[string]bool
		services   map[string]bool
	}
	projects := map[string]*acc{}

	// Labels are indexed by full ID; containers carry the short one.
	byShort := map[string]map[string]string{}
	for id, l := range labels {
		byShort[shortID(id)] = l
	}

	for _, ct := range containers {
		name := ct.ComposeProject
		if name == "" {
			continue
		}
		a, ok := projects[name]
		if !ok {
			a = &acc{files: map[string]bool{}, services: map[string]bool{}}
			projects[name] = a
		}
		if ct.ComposeService != "" {
			a.services[ct.ComposeService] = true
		}
		// Read compose metadata from the raw labels: the copy on the container
		// has been through redaction, and a path is not something we want to
		// depend on having survived that.
		raw := byShort[ct.ID]
		wd := raw["com.docker.compose.project.working_dir"]
		if wd != "" {
			a.workingDir = wd
		}
		for _, p := range strings.Split(raw["com.docker.compose.project.config_files"], ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			// Older compose versions record a path relative to working_dir.
			if !filepath.IsAbs(p) && wd != "" {
				p = filepath.Join(wd, p)
			}
			a.files[p] = true
		}
	}

	out := make([]inventory.ComposeProject, 0, len(projects))
	for name, a := range projects {
		p := inventory.ComposeProject{
			Name:        name,
			WorkingDir:  a.workingDir,
			Services:    sortedKeys(a.services),
			ConfigFiles: []inventory.ComposeFile{},
		}
		if len(a.files) == 0 {
			env.Infof("compose project %q declares no config files", name)
		}
		for _, path := range sortedKeys(a.files) {
			p.ConfigFiles = append(p.ConfigFiles, readComposeFile(env, path, table))
		}
		out = append(out, p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func readComposeFile(env *Env, path string, table mountTable) inventory.ComposeFile {
	cf := inventory.ComposeFile{Path: path}

	readPath := path
	info, err := os.Stat(env.Path(readPath))
	if err != nil {
		// The recorded path may be container-visible rather than host-visible.
		if hostPath, via, ok := table.resolve(env, path); ok {
			readPath = hostPath
			cf.ResolvedPath = hostPath
			cf.ResolvedVia = via
			info, err = os.Stat(env.Path(readPath))
		}
	}
	if err != nil {
		cf.ReadError = err.Error()
		env.Warnf("compose file %s is not reachable on this host: %v", path, err)
		return cf
	}

	cf.Exists = true
	cf.SizeBytes = info.Size()
	cf.ModTime = info.ModTime().UTC()

	raw, err := env.ReadFile(readPath)
	if err != nil {
		cf.ReadError = err.Error()
		env.Warnf("compose file %s could not be read: %v", readPath, err)
		return cf
	}

	// Redact first, hash second. The stored hash therefore describes exactly
	// what is stored — no plaintext-derived value ever leaves this function.
	cf.Content = redact.Text(raw)
	sum := sha256.Sum256([]byte(cf.Content))
	cf.SHA256 = hex.EncodeToString(sum[:])
	return cf
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shortID trims a full container ID to the 12 characters docker shows, which
// is what you will type when following up on a finding.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
