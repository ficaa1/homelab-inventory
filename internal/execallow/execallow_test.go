package execallow

import (
	"context"
	"os"
	"path/filepath"
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
	expected := []string{"compose-pull", "compose-restart", "compose-set-image", "compose-up", "container-restart", "image-prune"}
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

func TestUpgradeCommands(t *testing.T) {
	file := filepath.Join(t.TempDir(), "compose.yml")
	r := &Runner{Policy: Policy{Host: "host-a", ComposeFiles: map[string]string{"app": file}}}
	for _, tc := range []struct {
		command, service string
		argv             []string
	}{
		{"compose-pull", "app", []string{"docker", "compose", "-f", file, "pull", "app"}},
		{"compose-up", "", []string{"docker", "compose", "-f", file, "up", "-d"}},
		{"image-prune", "", []string{"docker", "image", "prune", "-f"}},
	} {
		args := map[string]string{"compose_file": file, "service": tc.service}
		if err := r.Validate(tc.command, args); err != nil {
			t.Fatal(err)
		}
		if !strSliceEq(buildArgv(tc.command, args), tc.argv) {
			t.Fatal(buildArgv(tc.command, args))
		}
	}
	args := map[string]string{"compose_file": file, "old_image": "app:v1", "new_image": "app:v2"}
	for _, content := range []string{"image: app:v1\n", "image: other:v1\n", "image: app:v1\n# app:v1\n"} {
		if err := os.WriteFile(file, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		result := r.Run(context.Background(), "compose-set-image", args)
		wantOK := strings.Count(content, "app:v1") == 1
		if (result.ExitCode == 0) != wantOK {
			t.Fatal(result)
		}
		data, _ := os.ReadFile(file)
		if wantOK && string(data) != "image: app:v2\n" || !wantOK && string(data) != content {
			t.Fatal(string(data))
		}
		info, _ := os.Stat(file)
		if info.Mode().Perm() != 0644 {
			t.Fatal(info.Mode())
		}
	}
	for _, bad := range []map[string]string{
		{"compose_file": file + ".bak"}, {"compose_file": file, "service": "--help"},
		{"compose_file": file, "old_image": "app:v1", "new_image": "app:v1"},
		{"compose_file": file, "old_image": "app:v1", "new_image": "app:v2;pwd"},
	} {
		command := "compose-set-image"
		if bad["old_image"] == "" {
			command = "compose-pull"
		}
		if r.Validate(command, bad) == nil {
			t.Fatal("accepted", bad)
		}
	}
}
