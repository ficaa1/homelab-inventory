package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func fixture() *inventory.Inventory {
	return &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "qbittorrent", ID: "q", ComposeProject: "vpn", NetworkMode: "container:g"},
		{Name: "vpnservice", ID: "g", ComposeProject: "vpn"},
		{Name: "homelab-api", ID: "a", ComposeProject: "api"},
	}, ComposeProjects: []inventory.ComposeProject{{Name: "vpn", ConfigFiles: []inventory.ComposeFile{{Path: "/other/compose.yml", Exists: true}}}}}}
}
func TestResolveNetworkImpactAndFallbackOrder(t *testing.T) {
	inv := fixture()
	p, err := Resolve(inv, "container_restart", "vpnservice", execallow.Policy{Host: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Impact) != 1 || p.Impact[0].Name != "qbittorrent" || !strings.Contains(p.Impact[0].Effect, "networking") {
		t.Fatalf("impact: %+v", p.Impact)
	}
	p, err = Resolve(inv, "stack_restart", "vpn", execallow.Policy{Host: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Containers, []string{"vpnservice", "qbittorrent"}) || len(p.Operations) != 2 {
		t.Fatalf("fallback: %+v", p)
	}
}
func TestResolveExplicitProjectUsesInventoryContainers(t *testing.T) {
	inv := fixture()
	// A -p project differs from the directory name even for an allowed file.
	inv.Docker.ComposeProjects[0].ConfigFiles = []inventory.ComposeFile{{Path: "/srv/app/compose.yml", Exists: true}}
	p, err := Resolve(inv, "stack_restart", "vpn", execallow.Policy{Host: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Operation{
		{"container-restart", map[string]string{"container": "vpnservice"}},
		{"container-restart", map[string]string{"container": "qbittorrent"}},
	}
	if !reflect.DeepEqual(p.Operations, want) {
		t.Fatalf("plan: %+v", p)
	}
}
func TestResolveRefusals(t *testing.T) {
	for _, tc := range []struct{ kind, target string }{{"container_restart", "--help"}, {"container_restart", "typo"}, {"stack_restart", "typo"}, {"unknown", "vpnservice"}} {
		if _, err := Resolve(fixture(), tc.kind, tc.target, execallow.Policy{Host: "host-a"}); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	inv := fixture()
	inv.Docker.Containers = nil
	if _, err := Resolve(inv, "container_restart", "vpnservice", execallow.Policy{Host: "host-a"}); err == nil {
		t.Fatal("accepted uncollected containers")
	}
}
func TestStacksFromContainerLabels(t *testing.T) {
	stacks := Stacks(fixture())
	if len(stacks) != 2 || stacks[0].Name != "api" || stacks[1].Name != "vpn" {
		t.Fatalf("stacks: %+v", stacks)
	}
}
func TestExecuteAuditOneLineAndReload(t *testing.T) {
	dir := t.TempDir()
	inv := fixture()
	calls := 0
	runner := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(_ context.Context, command string, args map[string]string) execallow.Result {
		calls++
		raw, err := os.ReadFile(filepath.Join(dir, "actions-audit.jsonl"))
		if err != nil || !strings.Contains(string(raw), `"result":"running"`) {
			t.Fatalf("pre-execution audit missing: %v", err)
		}
		return execallow.Result{ExitCode: 0}
	}}
	m, err := Open(dir, runner)
	if err != nil {
		t.Fatal(err)
	}
	e, err := m.Execute(context.Background(), "container_restart", "homelab-api", "bearer", ContainerSetHash([]string{"homelab-api"}))
	if err != nil || e.Result != "ok" || calls != 1 {
		t.Fatalf("execute: %+v %v", e, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "actions-audit.jsonl"))
	if strings.Count(string(raw), "\n") != 1 {
		t.Fatal("expected one audit line")
	}
	reloaded, err := Open(dir, runner)
	if err != nil || len(reloaded.Log(1)) != 1 || reloaded.Log(1)[0].Actor != "bearer" {
		t.Fatalf("reload: %v", err)
	}
}
func TestExecuteFailureStopsFallback(t *testing.T) {
	inv := fixture()
	calls := 0
	r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(context.Context, string, map[string]string) execallow.Result {
		calls++
		return execallow.Result{ExitCode: 2}
	}}
	m, _ := Open(t.TempDir(), r)
	e, err := m.Execute(context.Background(), "stack_restart", "vpn", "operator", ContainerSetHash([]string{"vpnservice", "qbittorrent"}))
	if err != nil || e.Result != "failed" || e.ExitCode != 2 || calls != 1 {
		t.Fatalf("failure: %+v %v calls=%d", e, err, calls)
	}
}
func TestExecuteAuditUnavailableNeverExecutes(t *testing.T) {
	calls := 0
	inv := fixture()
	r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(context.Context, string, map[string]string) execallow.Result { calls++; return execallow.Result{} }}
	m, _ := Open(t.TempDir(), r)
	m.path = filepath.Join(t.TempDir(), "missing", "audit")
	if _, err := m.Execute(context.Background(), "container_restart", "vpnservice", "bearer", ContainerSetHash([]string{"vpnservice"})); err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
func TestLogLimitAndInterruptedRow(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "actions-audit.jsonl"), []byte("{\"result\":\"running\"}\n"), 0600)
	m, err := Open(dir, nil)
	if err != nil || m.Log(1)[0].Result != "running" {
		t.Fatalf("interrupted audit: %v", err)
	}
	for i := 0; i < 110; i++ {
		m.entries = append(m.entries, Entry{Target: "test"})
	}
	if len(m.Log(0)) != 100 {
		t.Fatal("log limit")
	}
}

func TestComposeServiceDependenciesOrderAndImpact(t *testing.T) {
	inv := fixture()
	inv.Docker.Containers = []inventory.Container{
		{Name: "a-web", ComposeProject: "app", ComposeService: "web", DependsOn: []string{"db:service_healthy:false"}},
		{Name: "z-db", ComposeProject: "app", ComposeService: "db"},
	}
	p, err := Resolve(inv, "stack_restart", "app", execallow.Policy{Host: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Containers, []string{"z-db", "a-web"}) || len(p.Impact) != 1 || p.Impact[0].Target != "z-db" || p.Impact[0].Name != "a-web" {
		t.Fatalf("service dependency: %+v", p)
	}
}

func TestExecuteRejectsChangedContainerSet(t *testing.T) {
	for _, change := range []string{"added", "removed", "all removed", "missing confirmation"} {
		t.Run(change, func(t *testing.T) {
			inv := fixture()
			p, err := Resolve(inv, "stack_restart", "vpn", execallow.Policy{Host: "host-a"})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "added":
				inv.Docker.Containers = append(inv.Docker.Containers, inventory.Container{Name: "new-service", ComposeProject: "vpn"})
			case "removed":
				inv.Docker.Containers = inv.Docker.Containers[1:]
			case "all removed":
				inv.Docker.Containers = []inventory.Container{}
			case "missing confirmation":
				p.ContainerSetHash = ""
			}
			calls := 0
			r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(context.Context, string, map[string]string) execallow.Result {
				calls++
				return execallow.Result{}
			}}
			m, _ := Open(t.TempDir(), r)
			_, err = m.Execute(context.Background(), "stack_restart", "vpn", "operator", p.ContainerSetHash)
			var changed *PreviewChanged
			if !errors.As(err, &changed) || calls != 0 || len(m.Log(1)) != 0 {
				t.Fatalf("err=%v calls=%d log=%+v", err, calls, m.Log(1))
			}
			fresh, _ := Resolve(inv, "stack_restart", "vpn", execallow.Policy{Host: "host-a"})
			if !reflect.DeepEqual(changed.Preview, fresh) {
				t.Fatalf("preview: %+v", changed.Preview)
			}
		})
	}
}

func TestExecutePerContainerResultsPersist(t *testing.T) {
	for _, failAt := range []int{0, 1, -1} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			inv := fixture()
			inv.Docker.Containers = []inventory.Container{
				{Name: "a", ComposeProject: "app"},
				{Name: "b", ComposeProject: "app"},
				{Name: "c", ComposeProject: "app"},
			}
			dir := t.TempDir()
			calls := 0
			r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(_ context.Context, cmd string, args map[string]string) execallow.Result {
				if cmd != "container-restart" || args["container"] != []string{"a", "b", "c"}[calls] {
					t.Fatalf("unexpected operation: %s %v", cmd, args)
				}
				i := calls
				calls++
				if i == failAt {
					return execallow.Result{ExitCode: 2, Stderr: "daemon refused\nrestart"}
				}
				return execallow.Result{}
			}}
			m, _ := Open(dir, r)
			// Confirmation is a set, so input ordering does not matter.
			e, err := m.Execute(context.Background(), "stack_restart", "app", "operator", ContainerSetHash([]string{"c", "a", "b"}))
			if err != nil {
				t.Fatal(err)
			}
			want := []ContainerResult{{Container: "a", Status: "restarted"}, {Container: "b", Status: "restarted"}, {Container: "c", Status: "restarted"}}
			status := "ok"
			if failAt >= 0 {
				status = "failed"
				if failAt > 0 {
					status = "partial"
				}
				want[failAt].Status = "failed"
				want[failAt].Error = "daemon refused restart"
				for i := failAt + 1; i < len(want); i++ {
					want[i].Status = "skipped"
				}
			}
			if e.Result != status || !reflect.DeepEqual(e.Results, want) {
				t.Fatalf("result: %+v", e)
			}
			reloaded, err := Open(dir, nil)
			if err != nil || !reflect.DeepEqual(reloaded.Log(1)[0], e) {
				t.Fatalf("persisted result: %v %+v", err, reloaded)
			}
		})
	}
}

func TestContainerSetHashDeterministic(t *testing.T) {
	names := []string{"b", "a"}
	const want = "0473ef2dc0d324ab659d3580c1134e9d812035905c4781fdd6d529b0c6860e13"
	if got := ContainerSetHash(names); got != want || !reflect.DeepEqual(names, []string{"b", "a"}) {
		t.Fatalf("hash=%s names=%v", got, names)
	}
	if ContainerSetHash([]string{"a", "b"}) != want || ContainerSetHash([]string{"a"}) == want {
		t.Fatal("hash must ignore ordering and detect membership changes")
	}
	if ContainerSetHash(nil) != ContainerSetHash([]string{}) {
		t.Fatal("empty set hash must be consistent")
	}
}

func TestExecuteHashIgnoresInventoryOrderAndMetadata(t *testing.T) {
	inv := fixture()
	p, err := Resolve(inv, "stack_restart", "vpn", execallow.Policy{Host: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	inv.Docker.Containers[0], inv.Docker.Containers[1] = inv.Docker.Containers[1], inv.Docker.Containers[0]
	inv.Docker.Containers[0].Image = "changed-image"
	calls := []string{}
	r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(_ context.Context, command string, args map[string]string) execallow.Result {
		if command != "container-restart" {
			t.Fatalf("command=%s", command)
		}
		calls = append(calls, args["container"])
		return execallow.Result{}
	}}
	m, err := Open(t.TempDir(), r)
	if err != nil {
		t.Fatal(err)
	}
	e, err := m.Execute(context.Background(), "stack_restart", "vpn", "operator", p.ContainerSetHash)
	if err != nil || e.Result != "ok" || !reflect.DeepEqual(calls, []string{"vpnservice", "qbittorrent"}) {
		t.Fatalf("result=%+v err=%v calls=%v", e, err, calls)
	}
}

func TestConfiguredHostAndExactComposeProject(t *testing.T) {
	inv := fixture()
	inv.Host.Hostname = "custom-host"
	policy := execallow.Policy{Host: "custom-host", ComposeFiles: map[string]string{"vpn": "/srv/app/compose.yml"}}
	p, err := Resolve(inv, "stack_restart", "vpn", policy)
	if err != nil || len(p.Operations) != 1 || p.Operations[0].Command != "compose-restart" || p.Operations[0].Args["project"] != "vpn" {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
	runner := &execallow.Runner{Policy: policy, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(_ context.Context, command string, args map[string]string) execallow.Result {
		return execallow.Result{}
	}}
	m, _ := Open(t.TempDir(), runner)
	if e, err := m.Execute(context.Background(), "stack_restart", "vpn", "operator", p.ContainerSetHash); err != nil || e.Result != "ok" {
		t.Fatal(e, err)
	}
	policy.Host = "another-host"
	if _, err := Resolve(inv, "stack_restart", "vpn", policy); err == nil {
		t.Fatal("wrong host accepted")
	}
	policy.Host = ""
	if _, err := Resolve(inv, "stack_restart", "vpn", policy); err == nil {
		t.Fatal("unset host accepted")
	}
}
