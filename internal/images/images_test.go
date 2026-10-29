package images

import (
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// The reference format is more awkward than it looks: a colon can introduce a
// tag or a registry port, and a slash can separate a registry, a namespace, or
// neither. The examples include shapes that break naive parsers.
func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw      string
		registry string
		repo     string
		tag      string
		digest   string
		pin      Pinning
	}{
		{"plexinc/pms-docker:1.40", "", "plexinc/pms-docker", "1.40", "", PinTag},
		{"ghcr.io/immich-app/immich-server:release", "ghcr.io", "immich-app/immich-server", "release", "", PinTag},
		{"louislam/uptime-kuma:1", "", "louislam/uptime-kuma", "1", "", PinTag},
		{"portainer/portainer-ce:latest", "", "portainer/portainer-ce", "latest", "", PinFloating},
		{"redis", "", "redis", "", "", PinFloating},
		{"redis:7-alpine", "", "redis", "7-alpine", "", PinTag},
		// A registry port must not be read as a tag.
		{"registry.local:5000/thing", "registry.local:5000", "thing", "", "", PinFloating},
		{"registry.local:5000/thing:v2", "registry.local:5000", "thing", "v2", "", PinTag},
		{"localhost:5000/mine:dev", "localhost:5000", "mine", "dev", "", PinTag},
		// A digest is immutable regardless of any tag beside it.
		{"nginx@sha256:abc123", "", "nginx", "", "sha256:abc123", PinDigest},
		{"ghcr.io/a/b:v1@sha256:deadbeef", "ghcr.io", "a/b", "v1", "sha256:deadbeef", PinDigest},
		{"", "", "", "", "", PinFloating},
	}
	for _, c := range cases {
		got := Parse(c.raw)
		if got.Registry != c.registry || got.Repository != c.repo ||
			got.Tag != c.tag || got.Digest != c.digest || got.Pinning != c.pin {
			t.Errorf("Parse(%q) = reg %q repo %q tag %q digest %q pin %q;\n  want reg %q repo %q tag %q digest %q pin %q",
				c.raw, got.Registry, got.Repository, got.Tag, got.Digest, got.Pinning,
				c.registry, c.repo, c.tag, c.digest, c.pin)
		}
		if got.Raw != c.raw {
			t.Errorf("Parse(%q) lost the raw reference", c.raw)
		}
	}
}

func inv(host string, cs ...inventory.Container) *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host:          inventory.Host{Hostname: host},
		Docker:        &inventory.Docker{Available: true, Containers: cs},
	}
}

func TestBuildCountsPinningLevels(t *testing.T) {
	t.Parallel()
	r := Build([]*inventory.Inventory{inv("host-a",
		inventory.Container{Name: "plex", Image: "plexinc/pms-docker:1.40", State: "running"},
		inventory.Container{Name: "sonarr", Image: "linuxserver/sonarr:latest", State: "running"},
		inventory.Container{Name: "radarr", Image: "linuxserver/radarr", State: "running"},
		inventory.Container{Name: "db", Image: "postgres@sha256:abc", State: "running"},
	)})

	if r.Counts.Total != 4 || r.Counts.Tag != 1 || r.Counts.Floating != 2 || r.Counts.Digest != 1 {
		t.Errorf("counts = %+v", r.Counts)
	}
	f := r.Floating()
	if len(f) != 2 {
		t.Fatalf("Floating = %d, want 2", len(f))
	}
	// An omitted tag is just as floating as an explicit :latest.
	var names []string
	for _, c := range f {
		names = append(names, c.Name)
	}
	if names[0] != "radarr" && names[1] != "radarr" {
		t.Errorf("an untagged image was not counted as floating: %v", names)
	}
}

// A section that did not run is not a host with no containers.
func TestSkipsHostsWithoutDocker(t *testing.T) {
	t.Parallel()
	none := &inventory.Inventory{Host: inventory.Host{Hostname: "host-b"}}
	off := &inventory.Inventory{Host: inventory.Host{Hostname: "host-c"},
		Docker: &inventory.Docker{Available: false}}
	r := Build([]*inventory.Inventory{none, off, inv("host-a",
		inventory.Container{Name: "a", Image: "x:1", State: "running"})})
	if r.Counts.Total != 1 {
		t.Errorf("total = %d, want 1", r.Counts.Total)
	}
}

// Two containers on one image move together, which is worth knowing before
// pinning either of them.
func TestSharedImages(t *testing.T) {
	t.Parallel()
	r := Build([]*inventory.Inventory{
		inv("host-a",
			inventory.Container{Name: "a", Image: "redis:7", State: "running"},
			inventory.Container{Name: "b", Image: "redis:7", State: "running"}),
		inv("host-b", inventory.Container{Name: "c", Image: "redis:7", State: "running"}),
	})
	if len(r.Shared) != 1 || len(r.Shared[0].Users) != 3 {
		t.Fatalf("Shared = %+v, want one image with three users", r.Shared)
	}
	if r.Shared[0].Repository != "redis" || r.Shared[0].Tag != "7" {
		t.Errorf("Shared[0] = %+v", r.Shared[0])
	}
}

func TestForHost(t *testing.T) {
	t.Parallel()
	r := Build([]*inventory.Inventory{
		inv("host-a", inventory.Container{Name: "a", Image: "x:latest", State: "running"}),
		inv("host-b", inventory.Container{Name: "b", Image: "y:1.2", State: "running"}),
	})
	p := r.ForHost("host-a")
	if p.Counts.Total != 1 || p.Counts.Floating != 1 {
		t.Errorf("ForHost counts = %+v", p.Counts)
	}
	if len(r.ForHost("nope").Containers) != 0 {
		t.Error("an unknown host should be empty, not everything")
	}
}
