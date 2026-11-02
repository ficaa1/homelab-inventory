//go:build !linux

package mediapolicy

import (
	"fmt"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

func seedLink(string, string, archivetier.Evidence) error {
	return fmt.Errorf("seeding filesystem writes require Linux")
}
func seedRemove(string, archivetier.Evidence, bool) error {
	return fmt.Errorf("seeding filesystem writes require Linux")
}
func seedLinks(string) (uint64, error) { return 0, fmt.Errorf("seeding requires Linux") }
func seedPrepareDestination(string) error {
	return fmt.Errorf("seeding filesystem writes require Linux")
}
