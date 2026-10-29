package execallow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestContainerRestartInventoryGuard(t *testing.T) {
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{{Name: "homelab-api"}, {Name: "qbittorrent"}, {Name: "--help"}}}}
	calls := 0
	r := &Runner{Policy: Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(context.Context, string, map[string]string) Result { calls++; return Result{ExitCode: 0} }}
	for _, name := range []string{"homelab-api", "qbittorrent"} {
		if res := r.Run(context.Background(), "container-restart", map[string]string{"container": name}); res.ExitCode != 0 {
			t.Fatalf("refused %s: %+v", name, res)
		}
	}
	for _, name := range []string{"typo", "--help", "a;pwd"} {
		if res := r.Run(context.Background(), "container-restart", map[string]string{"container": name}); res.ExitCode != -1 {
			t.Fatalf("accepted %s", name)
		}
	}
	r.Inventory = nil
	if res := r.Run(context.Background(), "container-restart", map[string]string{"container": "homelab-api"}); res.ExitCode != -1 {
		t.Fatal("accepted missing inventory")
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestExactComposeAllowlistAndTargetPolicy(t *testing.T) {
	files, err := ParseComposeFiles("app=/srv/app/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Policy: Policy{Host: "host-a", ComposeFiles: files}}
	for _, file := range []string{"/srv/app/other.yml", "/srv/app/../app/compose.yml", "/srv/app/compose.yml.bak"} {
		if r.Validate("compose-restart", map[string]string{"project": "app", "compose_file": file}) == nil {
			t.Fatal("broad path match")
		}
	}
	if r.Validate("compose-restart", map[string]string{"project": "other", "compose_file": "/srv/app/compose.yml"}) == nil {
		t.Fatal("wrong project accepted")
	}
	for _, value := range []string{"app=relative.yml", "app=/srv/a,app=/srv/b", "--help=/srv/a", "app=/srv/../a"} {
		if _, err := ParseComposeFiles(value); err == nil {
			t.Fatal("bad mapping accepted")
		}
	}
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "wrong-host"}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{{Name: "app"}}}}
	r.Inventory = func() (*inventory.Inventory, error) { return inv, nil }
	if r.Validate("container-restart", map[string]string{"container": "app"}) == nil {
		t.Fatal("wrong inventory host accepted")
	}
	if got := buildArgv("compose-restart", map[string]string{"compose_file": "/srv/app/compose.yml", "project": "app"}); len(got) != 7 || got[4] != "-p" || got[5] != "app" {
		t.Fatal(got)
	}
}

func TestLiveDockerExactName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{{Name: "abc123"}}}}
	r := &Runner{Policy: Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }}
	for _, tc := range []struct {
		output string
		exit   string
		ok     bool
	}{
		{"/abc123", "0", true}, {"/different-container", "0", false}, {"/abc123", "1", false}, {"", "0", false},
	} {
		script := `#!/bin/sh
if [ "$1" = inspect ]; then
[ "$2" = --type=container ] && [ "$3" = --format ] && [ "$4" = '{{.Name}}' ] && [ "$5" = -- ] && [ "$6" = abc123 ] || exit 2
printf '%s\n' '` + tc.output + `'
exit ` + tc.exit + `
fi
[ "$1" = restart ] && [ "$2" = /abc123 ]
`
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
		res := r.Run(context.Background(), "container-restart", map[string]string{"container": "abc123"})
		if (res.ExitCode == 0) != tc.ok {
			t.Errorf("live match ok=%v: %+v", tc.ok, res)
		}
	}
}
