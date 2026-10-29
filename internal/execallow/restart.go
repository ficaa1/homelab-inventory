package execallow

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// FindContainer requires an exact safe name in the supplied inventory.
func FindContainer(inv *inventory.Inventory, name string) (inventory.Container, error) {
	if !safeName.MatchString(name) {
		return inventory.Container{}, fmt.Errorf("invalid container name")
	}
	if inv == nil || inv.Docker == nil || !inv.Docker.Available || inv.Docker.Containers == nil {
		return inventory.Container{}, fmt.Errorf("latest target host Docker inventory unavailable")
	}
	for _, c := range inv.Docker.Containers {
		if c.Name == name {
			return c, nil
		}
	}
	return inventory.Container{}, fmt.Errorf("container %q is not in latest target host inventory", name)
}

// Policy is operator configuration, never inferred from inventory paths.
type Policy struct {
	Host         string
	ComposeFiles map[string]string
}

func (p Policy) CheckInventory(inv *inventory.Inventory) error {
	if p.Host == "" || inv == nil || inv.Host.Hostname != p.Host {
		return errors.New("restart target inventory unavailable")
	}
	return nil
}
func (p Policy) ComposeFileAllowed(path string) bool {
	for _, allowed := range p.ComposeFiles {
		if path == allowed {
			return true
		}
	}
	return false
}
func ParseComposeFiles(value string) (map[string]string, error) {
	files := map[string]string{}
	if value == "" {
		return files, nil
	}
	for _, item := range strings.Split(value, ",") {
		pair := strings.SplitN(item, "=", 2)
		if len(pair) != 2 || !safeName.MatchString(pair[0]) || !filepath.IsAbs(pair[1]) || filepath.Clean(pair[1]) != pair[1] || files[pair[0]] != "" || strings.ContainsAny(pair[1], "\\\n\r") {
			return nil, errors.New("ACTIONS_COMPOSE_FILES requires unique project=/absolute/compose.yml entries")
		}
		files[pair[0]] = pair[1]
	}
	return files, nil
}
