// Package api_test checks the packaged compose configuration.
package api_test

import (
	"os"
	"strings"
	"testing"
)

// TestAPIServiceWiresEnvFile protects optional manual configuration without requiring a file.
func TestAPIServiceWiresEnvFile(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("reading deploy/api/docker-compose.yml: %v", err)
	}
	body := serviceBody(string(data), "api")
	if body == nil {
		t.Fatalf("deploy/api/docker-compose.yml has no api service")
	}
	if hasEnvFile(body, ".env") {
		t.Fatal("default compose must not require .env")
	}
	if !strings.Contains(string(data), "# env_file:") {
		t.Fatal("keep the optional manual env_file example")
	}
}

// serviceBody returns the lines between a two-space-indented `  name:` key and
// the next line indented at two spaces or fewer (the next service or a
// top-level key), or to end of file. Blank lines do not close a block. Two
// spaces is what separates a service name from its four-space keys, so this
// finds the api *service* rather than some nested property that happens to be
// called "api".
func serviceBody(doc, name string) []string {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if l == "  "+name+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if indent(lines[i]) <= 2 {
			return lines[start+1 : i]
		}
	}
	return lines[start+1:]
}

// hasEnvFile reports whether body has an env_file key whose list contains
// envFile. The list is the six-space-indented `- ...` entries that follow the
// four-space `env_file:` key, up to the next four-space key.
func hasEnvFile(body []string, envFile string) bool {
	envIdx := -1
	for i, l := range body {
		if strings.TrimSpace(l) == "env_file:" {
			envIdx = i
			break
		}
	}
	if envIdx < 0 {
		return false
	}
	for _, l := range body[envIdx+1:] {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indent(l) <= 4 {
			return false
		}
		item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		if item == envFile {
			return true
		}
	}
	return false
}

// indent returns the number of leading spaces and tabs, used to read YAML
// block nesting without pulling in a YAML dependency for one file.
func indent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

func TestPackagedAPIAllowsFirstRunSetup(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "${API_TOKEN:?") {
		t.Fatal("packaged API must start setup without credentials")
	}
	if strings.Contains(string(data), "allow-unauthenticated") {
		t.Fatal("packaged API enabled development authentication mode")
	}
}
