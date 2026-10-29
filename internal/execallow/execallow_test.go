package execallow

import (
	"context"
	"strings"
	"testing"
)

func TestRunUnknownCommand(t *testing.T) {
	r := &Runner{}
	res := r.Run(context.Background(), "rm -rf /", nil)
	if res.ExitCode != -1 {
		t.Fatalf("expected ExitCode -1, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Error, "unknown command") {
		t.Fatalf("expected error to mention unknown command, got %q", res.Error)
	}
}

func TestRunComposeRestartEmptyComposeFile(t *testing.T) {
	r := &Runner{}
	res := r.Run(context.Background(), "compose-restart", map[string]string{"compose_file": ""})
	if res.ExitCode != -1 {
		t.Fatalf("expected ExitCode -1, got %d", res.ExitCode)
	}
}

func TestRunComposeRestartDisallowedPath(t *testing.T) {
	r := &Runner{}
	res := r.Run(context.Background(), "compose-restart", map[string]string{"compose_file": "/etc/passwd"})
	if res.ExitCode != -1 {
		t.Fatalf("expected ExitCode -1, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Error, "not allowed") {
		t.Fatalf("expected error to say not allowed, got %q", res.Error)
	}
}

func TestRunContainerRestartShellMetacharacter(t *testing.T) {
	r := &Runner{}
	res := r.Run(context.Background(), "container-restart", map[string]string{"container": "a; rm -rf /"})
	if res.ExitCode != -1 {
		t.Fatalf("expected ExitCode -1, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Error, "not a valid name") {
		t.Fatalf("expected error to mention invalid name, got %q", res.Error)
	}
}

func TestBuildArgvContainerRestart(t *testing.T) {
	a := buildArgv("container-restart", map[string]string{"container": "myapp"})
	expected := []string{"docker", "restart", "/myapp"}
	if !strSliceEq(a, expected) {
		t.Fatalf("expected %v, got %v", expected, a)
	}
}

func TestCommands(t *testing.T) {
	names := Commands()
	expected := []string{"compose-restart", "container-restart"}
	if !strSliceEq(names, expected) {
		t.Fatalf("expected %v, got %v", expected, names)
	}
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRemovedOperationsRefused(t *testing.T) {
	for _, name := range []string{"compose-set-image", "compose-pull", "compose-up", "image-prune"} {
		if res := new(Runner).Run(context.Background(), name, nil); res.ExitCode != -1 || !strings.Contains(res.Error, "unknown command") {
			t.Errorf("accepted %s", name)
		}
	}
}
