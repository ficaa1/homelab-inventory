package collect

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestParsePortMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []portMapping
	}{
		{"0.0.0.0:8096->8096/tcp", []portMapping{{8096, 8096, "tcp"}}},
		{":::8096->8096/tcp", []portMapping{{8096, 8096, "tcp"}}},
		{"0.0.0.0:5375->3000/tcp", []portMapping{{5375, 3000, "tcp"}}},
		{"0.0.0.0:51820->51820/udp", []portMapping{{51820, 51820, "udp"}}},
		{"127.0.0.1:9090->9090/tcp", []portMapping{{9090, 9090, "tcp"}}},

		// Ranges. tdarr publishes 8265-8266 and the first version dropped it
		// silently, leaving four docker-proxy sockets unattributed.
		{"0.0.0.0:8265-8266->8265-8266/tcp", []portMapping{{8265, 8265, "tcp"}, {8266, 8266, "tcp"}}},
		{":::8265-8266->8265-8266/tcp", []portMapping{{8265, 8265, "tcp"}, {8266, 8266, "tcp"}}},
		// A shifted range still lines up host and container ports pairwise.
		{"0.0.0.0:9000-9002->8000-8002/tcp", []portMapping{
			{9000, 8000, "tcp"}, {9001, 8001, "tcp"}, {9002, 8002, "tcp"},
		}},

		// Exposed but not published: reserves no host port.
		{"8096/tcp", nil},
		{"", nil},
		// Mismatched widths would misattribute ports, so refuse rather than guess.
		{"0.0.0.0:9000-9005->8000-8002/tcp", nil},
		{"0.0.0.0:0->8096/tcp", nil},
	}
	for _, c := range cases {
		got := parsePortMapping(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parsePortMapping(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

// TestParsePortMappingRejectsHugeRanges guards the memory cap: docker will
// publish 1-65535 if you ask it to.
func TestParsePortMappingRejectsHugeRanges(t *testing.T) {
	t.Parallel()
	if got := parsePortMapping("0.0.0.0:1-65535->1-65535/tcp"); got != nil {
		t.Errorf("expanded a %d-port range", len(got))
	}
}

func TestContainerFromCgroup(t *testing.T) {
	t.Parallel()
	const full = "3a7f1c2e9b4d5a6f8c0e1d2b3a4f5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b"
	byID := map[string]*inventory.Container{
		shortID(full): {ID: shortID(full), Name: "plex"},
	}

	// Every cgroup shape docker and friends write, across both cgroup versions.
	for _, cg := range []string{
		"0::/docker/" + full,
		"0::/system.slice/docker-" + full + ".scope",
		"12:pids:/docker/" + full,
		"0::/kubepods/besteffort/pod123/docker-" + full + ".scope",
		"0::/machine.slice/libpod-" + full + ".scope",
	} {
		got := containerFromCgroup(cg, byID)
		if got == nil || got.Name != "plex" {
			t.Errorf("containerFromCgroup(%q) did not resolve", cg)
		}
	}

	// A host process must not be attributed to a container.
	if got := containerFromCgroup("0::/system.slice/sshd.service", byID); got != nil {
		t.Errorf("host process attributed to container %q", got.Name)
	}
	// An unknown container ID resolves to nothing rather than a wrong guess.
	if got := containerFromCgroup("0::/docker/"+"f"+full[1:], byID); got != nil {
		t.Errorf("unknown container id resolved to %q", got.Name)
	}
}

func TestUnitFromCgroup(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"0::/system.slice/sshd.service":                          "sshd.service",
		"0::/system.slice/system-getty.slice/getty@tty1.service": "getty@tty1.service",
		"12:pids:/system.slice/pveproxy.service":                 "pveproxy.service",
		// A .service nested under a .scope wins: it is the more useful name.
		"0::/user.slice/user-1000.slice/session-3.scope/tailscaled.service": "tailscaled.service",
		// A bare scope with no service is better than nothing.
		"0::/user.slice/user-1000.slice/session-3.scope": "session-3.scope",
		"0::/": "",
	}
	for cg, want := range cases {
		if got := unitFromCgroup(cg); got != want {
			t.Errorf("unitFromCgroup(%q) = %q, want %q", cg, got, want)
		}
	}
}

func TestReadEphemeralRange(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "proc/sys/net/ipv4")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ip_local_port_range"), []byte("32768\t60999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readEphemeralRange(&Env{RootFS: root}); got != [2]int{32768, 60999} {
		t.Errorf("readEphemeralRange = %v", got)
	}
	// A host where the file is missing falls back rather than reporting 0-0,
	// which would mark every port ephemeral.
	if got := readEphemeralRange(&Env{RootFS: t.TempDir()}); got != [2]int{32768, 60999} {
		t.Errorf("fallback range = %v", got)
	}
}

// TestAttributeListeners exercises the three attribution paths together, using
// a host-network container, a
// published port behind docker-proxy, and a plain systemd service.
func TestAttributeListeners(t *testing.T) {
	t.Parallel()
	const plexID = "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccddddddddddddeeeeeeeeeeeeffff"
	const jellyID = "1111111111112222222222223333333333334444444444445555555555556666"

	root := t.TempDir()
	writeCgroup := func(pid int, content string) {
		dir := filepath.Join(root, "proc", strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCgroup(100, "0::/system.slice/docker-"+plexID+".scope\n")
	writeCgroup(200, "0::/system.slice/docker.service\n") // docker-proxy lives on the host
	writeCgroup(300, "0::/system.slice/ssh.service\n")

	inv := &inventory.Inventory{
		Docker: &inventory.Docker{
			Available: true,
			Containers: []inventory.Container{
				{ID: shortID(plexID), Name: "plex", ComposeProject: "plex", ComposeService: "plex"},
				{ID: shortID(jellyID), Name: "jellyfin", ComposeProject: "media", ComposeService: "jellyfin",
					Ports: []string{"0.0.0.0:8096->8096/tcp", ":::8096->8096/tcp"}},
			},
		},
		Network: &inventory.Network{
			EphemeralRange: [2]int{32768, 60999},
			Listeners: []inventory.Listener{
				{Protocol: "tcp", Port: 32400, Process: "Plex Media Server", PID: 100},
				{Protocol: "tcp", Port: 8096, Process: "docker-proxy", PID: 200},
				{Protocol: "tcp", Port: 22, Process: "sshd", PID: 300},
				{Protocol: "tcp", Port: 45511, Process: "rpc.statd", PID: 0},
			},
		},
	}
	attributeListeners(&Env{RootFS: root}, inv)
	ls := inv.Network.Listeners

	// A host-network container: no published port, looks like a host process
	// to ss, and only the cgroup gives it away.
	if o := ls[0].Owner; o == nil || o.Kind != inventory.OwnerContainer || o.Name != "plex" {
		t.Errorf("host-network container not attributed: %+v", o)
	}
	// A published port: the process is docker-proxy on the host, the service
	// is a container.
	if o := ls[1].Owner; o == nil || o.Kind != inventory.OwnerPublished || o.Name != "jellyfin" {
		t.Fatalf("published port not attributed: %+v", o)
	}
	if ls[1].Owner.ComposeProject != "media" {
		t.Errorf("compose project lost: %+v", ls[1].Owner)
	}
	if o := ls[2].Owner; o == nil || o.Kind != inventory.OwnerUnit || o.Name != "ssh.service" {
		t.Errorf("systemd unit not attributed: %+v", o)
	}
	// No PID means no attribution, not a wrong one.
	if ls[3].Owner != nil {
		t.Errorf("unattributable socket got an owner: %+v", ls[3].Owner)
	}
}

func TestMarkEphemeral(t *testing.T) {
	t.Parallel()
	n := &inventory.Network{
		EphemeralRange: [2]int{32768, 60999},
		Listeners: []inventory.Listener{
			{Port: 22}, {Port: 8096}, {Port: 32400}, {Port: 45511}, {Port: 60999}, {Port: 61000},
		},
	}
	markEphemeral(n)
	want := []bool{false, false, false, true, true, false}
	for i, w := range want {
		if n.Listeners[i].Ephemeral != w {
			t.Errorf("port %d ephemeral = %v, want %v", n.Listeners[i].Port, n.Listeners[i].Ephemeral, w)
		}
	}
}
