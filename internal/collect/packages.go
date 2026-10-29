package collect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

const maxPackages = 20000
const packageBytes = 64 << 20

// PackagesCollector reads package databases without invoking package managers
// or entering containers. Daily scans reread writable layers; image tags are
// never used as cache keys.
type PackagesCollector struct{}

func (PackagesCollector) Name() string { return "packages" }
func (PackagesCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	now := time.Now()
	if env.Now != nil {
		now = env.Now()
	}
	if reason := packageScanReason(env, now); reason == "" {
		inv.Software = nil
		env.Infof("packages: outside daily window 04:00-04:20, not just booted or installed")
		return nil
	} else if reason != "window" {
		env.Infof("packages: scanning because %s", reason)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	start := time.Now()
	s := &inventory.Software{CollectedAt: start.UTC(), Status: "ok", Images: []inventory.ImagePackages{}, Runtimes: []inventory.RuntimeVersion{}, ImagesStatus: "ok"}
	inv.Software = s
	defer func() {
		s.DurationMS = time.Since(start).Milliseconds()
		// No Docker means no images to sample: honest as unsupported, not a gap.
		if s.Host.Status != "ok" || (s.ImagesStatus != "ok" && s.ImagesStatus != "unsupported") || ctx.Err() != nil {
			s.Status = "partial"
		}
		if s.Status != "ok" {
			env.Warnf("software collection incomplete: host=%s images=%s", s.Host.Status, s.ImagesStatus)
		}
	}()
	root := env.Path("/")
	s.Host = collectPackageSet(ctx, root)
	addRuntime := func(name, version, source string) {
		if version = strings.TrimSpace(version); version != "" {
			s.Runtimes = append(s.Runtimes, inventory.RuntimeVersion{Name: name, Version: redact.String(version), Scope: "host", Source: source})
		}
	}
	for _, fact := range []struct{ name, path string }{{"kernel", "/proc/sys/kernel/osrelease"}, {"truenas", "/etc/version"}} {
		if v, err := env.ReadFile(fact.path); err == nil {
			addRuntime(fact.name, v, fact.path)
		}
	}
	for _, p := range s.Host.Packages {
		if strings.HasPrefix(p.Name, "linux-image-") || strings.HasPrefix(p.Name, "proxmox-kernel-") || strings.HasPrefix(p.Name, "pve-kernel-") {
			addRuntime("installed-kernel", p.Version, "package:"+p.Name)
		}
	}
	if !env.Run.Available("docker") {
		s.ImagesStatus = "unsupported"
		s.ImagesError = "docker not installed"
		env.Infof("package image scan: docker not installed")
		return nil
	}
	var version struct {
		Server struct {
			Version, GoVersion string
			Components         []struct{ Name, Version string }
		}
	}
	if err := packageDockerJSON(ctx, env, &version, "version", "--format", "{{json .}}"); err != nil {
		s.ImagesStatus = "failed"
		s.ImagesError = err.Error()
		return nil
	}
	addRuntime("docker", version.Server.Version, "docker version Server.Version")
	addRuntime("docker-go", version.Server.GoVersion, "docker version Server.GoVersion")
	for _, c := range version.Server.Components {
		switch c.Name {
		case "Engine", "docker-init":
		case "containerd":
			addRuntime(c.Name, c.Version, "docker version Server.Components")
		default:
			// The OCI runtime component is named after the default runtime:
			// with "default-runtime": "nvidia" it is "nvidia", but the version
			// it reports is still runc's.
			addRuntime("runc", c.Version, "docker version Server.Components:"+c.Name)
		}
	}
	res, err := env.Run.Run(ctx, "docker", "ps", "--no-trunc", "--format", "{{.ID}}")
	if err != nil || !res.OK() {
		s.ImagesStatus = "failed"
		s.ImagesError = "docker ps failed"
		return nil
	}
	seen := map[string]int{}
	for _, id := range res.Lines() {
		if ctx.Err() != nil {
			s.ImagesStatus = "partial"
			s.ImagesError = ctx.Err().Error()
			break
		}
		var rows []struct {
			Image, Name string
			Config      struct{ Image string }
			GraphDriver struct {
				Name string
				Data map[string]string
			}
		}
		// Narrow format deliberately excludes all container environment and labels.
		format := `[{"Image":{{json .Image}},"Name":{{json .Name}},"Config":{"Image":{{json .Config.Image}}},"GraphDriver":{{json .GraphDriver}}}]`
		if err := packageDockerJSON(ctx, env, &rows, "inspect", "--type=container", "--format", format, id); err != nil || len(rows) != 1 {
			s.ImagesStatus = "partial"
			s.ImagesError = "could not inspect a running container"
			env.Warnf("package scan: container inspect failed")
			continue
		}
		c := rows[0]
		name := strings.TrimPrefix(c.Name, "/")
		if i, ok := seen[c.Image]; ok {
			s.Images[i].Containers = append(s.Images[i].Containers, name)
			continue
		}
		image := inventory.ImagePackages{ImageID: c.Image, ImageRef: redact.String(c.Config.Image), Containers: []string{name}, SampleContainer: name, RepoDigests: []string{}, Runtimes: []inventory.RuntimeVersion{}}
		if c.GraphDriver.Name != "overlay2" || c.GraphDriver.Data["MergedDir"] == "" || !filepath.IsAbs(c.GraphDriver.Data["MergedDir"]) {
			image.PackageSet = unavailablePackageSet("unsupported", "running container has no overlay2 merged directory")
		} else {
			image.PackageSet = collectPackageSet(ctx, env.Path(c.GraphDriver.Data["MergedDir"]))
		}
		var meta []struct {
			Architecture, Os string
			RepoDigests      []string
			Config           struct{ Env []string }
		}
		// Only explicitly named version variables leave the Docker daemon.
		envFormat := `{{range .Config.Env}}{{$v := split . "="}}{{if or (eq (index $v 0) "PHP_VERSION") (eq (index $v 0) "NODE_VERSION") (eq (index $v 0) "PYTHON_VERSION") (eq (index $v 0) "GOLANG_VERSION") (eq (index $v 0) "JAVA_VERSION") (eq (index $v 0) "RUBY_VERSION") (eq (index $v 0) "NGINX_VERSION") (eq (index $v 0) "POSTGRES_VERSION") (eq (index $v 0) "PG_VERSION") (eq (index $v 0) "PG_MAJOR") (eq (index $v 0) "REDIS_VERSION")}}{{json .}},{{end}}{{end}}`
		// Emit an empty sentinel so the variable-length array remains valid JSON.
		format = `[{"Architecture":{{json .Architecture}},"Os":{{json .Os}},"RepoDigests":{{json .RepoDigests}},"Config":{"Env":[` + envFormat + `""]}}]`
		if err := packageDockerJSON(ctx, env, &meta, "inspect", "--type=image", "--format", format, c.Image); err == nil && len(meta) == 1 {
			m := meta[0]
			image.Architecture = m.Architecture
			image.OS = m.Os
			if m.RepoDigests != nil {
				image.RepoDigests = m.RepoDigests
			}
			for _, e := range m.Config.Env {
				k, v, ok := strings.Cut(e, "=")
				if ok && runtimeEnvKey(k) {
					image.Runtimes = append(image.Runtimes, inventory.RuntimeVersion{Name: strings.ToLower(strings.TrimSuffix(k, "_VERSION")), Version: redact.String(v), Scope: "image:" + c.Image, Source: "image Config.Env:" + k})
				}
			}
			s.Runtimes = append(s.Runtimes, image.Runtimes...)
		} else {
			s.ImagesStatus = "partial"
			s.ImagesError = "image metadata inspection failed"
		}
		switch image.PackageSet.Status {
		case "ok":
		case "unsupported":
			// Distroless and scratch images have no package database; that is
			// a fact about the image, not a failed scan.
			env.Infof("package scan for %s: %s", name, image.PackageSet.Error)
		default:
			s.ImagesStatus = "partial"
			env.Warnf("package scan for %s: %s", name, image.PackageSet.Error)
		}
		seen[c.Image] = len(s.Images)
		s.Images = append(s.Images, image)
	}
	return nil
}

// startupGrace is longer than the agent's 15-minute interval, so the first
// run after a boot or an install always lands inside it.
const startupGrace = 20 * time.Minute

// packageScanReason says why this run scans packages, or "" to skip. Besides
// the daily window, a run soon after boot or soon after this binary was
// installed scans too, so a new host or a fresh release has data without
// waiting for 04:00. Both are read from facts; discover keeps no state.
func packageScanReason(env *Env, now time.Time) string {
	switch {
	case env.ForcePackages:
		return "explicitly requested"
	case now.Hour() == 4 && now.Minute() < 20:
		return "window"
	}
	if s, err := env.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(s); len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil && up < startupGrace.Seconds() {
				return "host booted recently"
			}
		}
	}
	exe := env.Executable
	if exe == "" {
		exe, _ = os.Executable()
	}
	// Agent updates write a fresh file, so its mtime is the install time.
	if st, err := os.Stat(exe); err == nil {
		if age := now.Sub(st.ModTime()); age >= 0 && age < startupGrace {
			return "discover was installed recently"
		}
	}
	return ""
}

func runtimeEnvKey(k string) bool {
	switch k {
	case "PHP_VERSION", "NODE_VERSION", "PYTHON_VERSION", "GOLANG_VERSION", "JAVA_VERSION", "RUBY_VERSION", "NGINX_VERSION", "POSTGRES_VERSION", "PG_VERSION", "PG_MAJOR", "REDIS_VERSION":
		return true
	}
	return false
}
func packageDockerJSON(ctx context.Context, env *Env, out any, args ...string) error {
	r, err := env.Run.Run(ctx, "docker", args...)
	if err != nil {
		return err
	}
	if !r.OK() {
		return errors.New("docker query failed")
	}
	if err = json.Unmarshal([]byte(r.Stdout), out); err != nil {
		return errors.New("invalid docker query response")
	}
	return nil
}
func unavailablePackageSet(status, reason string) inventory.PackageSet {
	return inventory.PackageSet{Manager: "none", Status: status, Error: reason, CollectedAt: time.Now().UTC(), Packages: []inventory.Package{}}
}

func collectPackageSet(ctx context.Context, root string) (set inventory.PackageSet) {
	start := time.Now()
	set = unavailablePackageSet("ok", "")
	defer func() {
		set.DurationMS = time.Since(start).Milliseconds()
		set.Count = len(set.Packages)
		if set.Status == "failed" && set.Count > 0 {
			set.Status = "partial"
		}
		sort.Slice(set.Packages, func(i, j int) bool {
			a, b := set.Packages[i], set.Packages[j]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Arch < b.Arch
		})
	}()
	fail := func(err error) {
		set.Status = "failed"
		if len(set.Packages) > 0 {
			set.Status = "partial"
		}
		set.Error = redact.String(err.Error())
	}
	// Read os-release through the same confinement rules, including absolute links.
	if p, err := confinedPath(root, "/etc/os-release"); err == nil {
		if b, err := readPackageFile(p, 256<<10); err == nil {
			kv := parseKeyValue(string(b), "=")
			set.Distro = inventory.Distro{ID: redact.String(unquote(kv["ID"])), VersionID: redact.String(unquote(kv["VERSION_ID"])), Codename: redact.String(unquote(kv["VERSION_CODENAME"]))}
		} else {
			fail(err)
		}
	} else if !os.IsNotExist(err) {
		fail(err)
	}
	type database struct {
		path     string
		fragment bool
	}
	var paths []database
	for _, db := range []struct{ path, manager string }{{"/var/lib/dpkg/status", "dpkg"}, {"/var/lib/dpkg/status.d", "dpkg"}, {"/lib/apk/db/installed", "apk"}, {"/var/lib/rpm", "rpm"}, {"/usr/lib/sysimage/rpm", "rpm"}} {
		if db.manager != "dpkg" && set.Manager == "dpkg" {
			break
		}
		p, err := confinedPath(root, db.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			set.Manager = db.manager
			fail(err)
			return
		}
		set.Manager = db.manager
		if db.manager == "rpm" {
			set.Status = "unsupported"
			set.Error = "RPM database parsing is unsupported"
			return
		}
		if db.path == "/var/lib/dpkg/status.d" {
			entries, err := os.ReadDir(p)
			if err != nil {
				fail(err)
				return
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				q, err := confinedPath(root, db.path+"/"+e.Name())
				if err != nil {
					fail(err)
					return
				}
				paths = append(paths, database{q, true})
				if len(paths) > maxPackages {
					fail(errors.New("too many package database fragments"))
					return
				}
			}
		} else {
			paths = append(paths, database{p, false})
		}
		if db.manager == "apk" {
			break
		}
		// Prefer dpkg if either status or status.d exists; gather both before parsing.
		if db.path == "/var/lib/dpkg/status.d" {
			break
		}
	}
	if set.Manager == "none" {
		set.Status = "unsupported"
		set.Error = "no supported package database found"
		return
	}
	remaining := int64(packageBytes)
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			fail(err)
			return
		}
		st, err := os.Lstat(p.path)
		if err != nil || !st.Mode().IsRegular() {
			fail(errors.New("package database is not a regular file"))
			return
		}
		f, err := os.Open(p.path)
		if err != nil {
			fail(err)
			return
		}
		st, err = f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			f.Close()
			fail(errors.New("package database is not a regular file"))
			return
		}
		limited := &io.LimitedReader{R: f, N: remaining + 1}
		pkgs, err := parsePackageDB(ctx, limited, set.Manager, maxPackages-len(set.Packages), p.fragment)
		f.Close()
		remaining = limited.N - 1
		set.Packages = append(set.Packages, pkgs...)
		if remaining < 0 {
			err = errors.New("package database exceeds 64 MiB budget")
		}
		if err != nil {
			fail(err)
			return
		}
	}
	return
}

func readPackageFile(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return b, err
}

// confinedPath interprets absolute symlinks relative to root, never the host.
// Parent components at the root are rejected rather than silently cleaned.
// This component walk is for stable Docker merged mounts, not a security
// boundary against a privileged process concurrently replacing directories.
func confinedPath(root, path string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	todo := strings.Split(filepath.ToSlash(path), "/")
	parts := []string{}
	hops := 0
	for len(todo) > 0 {
		part := todo[0]
		todo = todo[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(parts) == 0 {
				return "", errors.New("rootfs path escapes root")
			}
			parts = parts[:len(parts)-1]
			continue
		}
		candidate := filepath.Join(append([]string{root}, append(parts, part)...)...)
		st, err := os.Lstat(candidate)
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > 40 {
				return "", errors.New("too many rootfs symlinks")
			}
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				parts = nil
			}
			todo = append(strings.Split(filepath.ToSlash(target), "/"), todo...)
		} else {
			parts = append(parts, part)
		}
	}
	return filepath.Join(append([]string{root}, parts...)...), nil
}

func parsePackageDB(ctx context.Context, r io.Reader, manager string, limit int, fragment bool) ([]inventory.Package, error) {
	out := []inventory.Package{}
	fields := map[string]string{}
	flush := func() error {
		defer func() { fields = map[string]string{} }()
		if len(fields) == 0 {
			return nil
		}
		var p inventory.Package
		if manager == "dpkg" {
			// Distroless status.d entries omit Status; dpkg's main database may not.
			if fields["Status"] == "" && !fragment {
				return errors.New("dpkg record is missing Status")
			}
			if fields["Status"] != "install ok installed" && !(fragment && fields["Status"] == "") {
				return nil
			}
			p = inventory.Package{Name: fields["Package"], Version: fields["Version"], Arch: fields["Architecture"]}
			src := fields["Source"]
			if name, v, ok := strings.Cut(src, " ("); ok {
				src = name
				p.SourceVersion = strings.TrimSuffix(v, ")")
			}
			if src != "" && src != p.Name {
				p.Source = src
			}
			if p.SourceVersion == p.Version {
				p.SourceVersion = ""
			}
		} else {
			p = inventory.Package{Name: fields["P"], Version: fields["V"], Arch: fields["A"]}
			if fields["o"] != p.Name {
				p.Source = fields["o"]
			}
		}
		if p.Name == "" || p.Version == "" {
			return errors.New("malformed package record")
		}
		if len(out) >= limit {
			return errors.New("package count limit reached")
		}
		p.Name = redact.String(p.Name)
		p.Version = redact.String(p.Version)
		p.Source = redact.String(p.Source)
		p.SourceVersion = redact.String(p.SourceVersion)
		p.Arch = redact.String(p.Arch)
		out = append(out, p)
		return nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return out, err
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			switch k {
			case "Package", "Version", "Architecture", "Source", "Status", "P", "V", "A", "o":
				fields[k] = strings.TrimSpace(v)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	if err := flush(); err != nil {
		return out, err
	}
	return out, nil
}
