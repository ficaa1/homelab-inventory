package actions

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func reconnectFixture() *inventory.Inventory {
	return &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, VPN: &inventory.VPN{Container: "gateway", Dependents: []inventory.VPNDependent{{Name: "worker-b"}}}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "gateway", ID: "abcdef123456"},
		{Name: "worker-a", NetworkMode: "container:abcdef1234567890"},
		{Name: "worker-b", NetworkMode: "container:gateway"},
		{Name: "unrelated", ComposeProject: "example"},
	}}}
}
func TestResolveVPNReconnectHashAndMembership(t *testing.T) {
	inv := reconnectFixture()
	p, err := Resolve(inv, "vpn_reconnect", "gateway", execallow.Policy{Host: "host-a"})
	if err != nil || !reflect.DeepEqual(p.Containers, []string{"gateway", "worker-a", "worker-b"}) || p.ContainerSetHash != ContainerSetHash(p.Containers) || len(p.Operations) != 3 {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
	if _, err := Resolve(inv, "vpn_reconnect", "worker-a", execallow.Policy{Host: "host-a"}); err == nil {
		t.Fatal("accepted non-gateway target")
	}
	inv.VPNs = []inventory.VPN{*inv.VPN, {Container: "tunnel", Dependents: []inventory.VPNDependent{{Name: "proxy"}}}}
	inv.Docker.Containers = append(inv.Docker.Containers, inventory.Container{Name: "tunnel"}, inventory.Container{Name: "proxy", NetworkMode: "container:tunnel"})
	p, err = Resolve(inv, "vpn_reconnect", "tunnel", execallow.Policy{Host: "host-a"})
	if err != nil || !reflect.DeepEqual(p.Containers, []string{"tunnel", "proxy"}) {
		t.Fatalf("second gateway plan=%+v err=%v", p, err)
	}
	inv.Docker.Containers = inv.Docker.Containers[:2]
	if _, err := Resolve(inv, "vpn_reconnect", "gateway", execallow.Policy{Host: "host-a"}); err == nil {
		t.Fatal("accepted missing configured dependent")
	}
}
func TestReconnectOrderNoHealthcheckAndFinalVerification(t *testing.T) {
	for _, health := range []string{"", "healthy"} {
		t.Run(health, func(t *testing.T) {
			inv := reconnectFixture()
			calls := []string{}
			r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(_ context.Context, operation string, args map[string]string) execallow.Result {
				name := args["container"]
				calls = append(calls, operation+":"+name)
				switch operation {
				case "container-state":
					started := "2026-01-01T00:00:01Z"
					if name != "gateway" {
						started = "2026-01-01T00:00:02Z"
					}
					return execallow.Result{Stdout: `{"name":"/` + name + `","state":"running","health":"` + health + `","startedAt":"` + started + `"}`}
				case "vpn-wg-dump":
					return execallow.Result{ExitCode: 1, Error: "WireGuard unavailable"}
				case "vpn-exit-check":
					return execallow.Result{Stdout: `{"ip":"192.0.2.20","country":"ZZ"}`}
				default:
					return execallow.Result{}
				}
			}}
			m, err := Open(t.TempDir(), r)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := Resolve(inv, "vpn_reconnect", "gateway", r.Policy)
			e, err := m.Execute(context.Background(), "vpn_reconnect", "gateway", "operator", p.ContainerSetHash)
			want := []string{"container-restart:gateway", "container-state:gateway", "vpn-wg-dump:gateway", "vpn-exit-check:gateway", "container-restart:worker-a", "container-restart:worker-b", "vpn-exit-check:gateway", "container-state:gateway", "container-state:worker-a", "container-state:worker-b"}
			if err != nil || e.Result != "ok" || e.ExitIP == nil || *e.ExitIP != "192.0.2.20" || !reflect.DeepEqual(calls, want) {
				t.Fatalf("entry=%+v err=%v calls=%v", e, err, calls)
			}
		})
	}
}
func TestReconnectAttemptsAllDependentsAfterReadinessDeadline(t *testing.T) {
	inv := reconnectFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := []string{}
	r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(_ context.Context, op string, args map[string]string) execallow.Result {
		name := args["container"]
		if op == "container-restart" {
			calls = append(calls, name)
			if name == "worker-a" {
				return execallow.Result{ExitCode: 1}
			}
			return execallow.Result{}
		}
		if op == "container-state" {
			cancel()
			return execallow.Result{Stdout: `{"name":"/` + name + `","state":"running","health":"starting","startedAt":"2026-01-01T00:00:01Z"}`}
		}
		return execallow.Result{ExitCode: 1, Error: "private-fixture"}
	}}
	m, _ := Open(t.TempDir(), r)
	p, _ := Resolve(inv, "vpn_reconnect", "gateway", r.Policy)
	e, err := m.Execute(ctx, "vpn_reconnect", "gateway", "operator", p.ContainerSetHash)
	if err != nil || e.Result != "partial" || !reflect.DeepEqual(calls, []string{"gateway", "worker-a", "worker-b"}) || strings.Contains(e.Reason, "private-fixture") {
		t.Fatalf("entry=%+v err=%v calls=%v", e, err, calls)
	}
}
func TestTryExecuteSkipsBusyAndLogShowsRunning(t *testing.T) {
	inv := reconnectFixture()
	entered := make(chan struct{})
	release := make(chan struct{})
	r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(context.Context, string, map[string]string) execallow.Result {
		close(entered)
		<-release
		return execallow.Result{}
	}}
	m, _ := Open(t.TempDir(), r)
	done := make(chan error, 1)
	go func() {
		_, err := m.Execute(context.Background(), "container_restart", "gateway", "operator", ContainerSetHash([]string{"gateway"}))
		done <- err
	}()
	<-entered
	if !m.Busy() {
		t.Fatal("manager does not report running action")
	}
	e, err := m.TryExecute(context.Background(), "vpn_reconnect", "gateway", "auto", "unused")
	if !errors.Is(err, ErrBusy) || e.Result != "skipped" {
		t.Fatalf("entry=%+v err=%v", e, err)
	}
	if rows := m.Log(10); len(rows) != 1 || rows[0].Result != "running" {
		t.Fatalf("running audit=%+v", rows)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m.Busy() || len(m.Log(10)) != 1 {
		t.Fatal("busy skip must not write an audit row")
	}
}
