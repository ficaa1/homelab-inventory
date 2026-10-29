package main

import (
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestToolInfoRevision(t *testing.T) {
	if got := toolInfo().Revision; got != inventory.AgentRevision {
		t.Errorf("tool revision = %d, want %d", got, inventory.AgentRevision)
	}
}
