package main

import (
	"encoding/json"
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// A migrated shared private-dashboard token is accepted only for inventory
// ingestion. It does not confer dashboard access or release-update authority.
func loadLegacyAgent(dir, admin, operator string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state-migration", "legacy-agent.json"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var v struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(b, &v) != nil {
		return "", errors.New("invalid legacy agent credential state")
	}
	if v.Token == "" {
		return "", nil
	}
	if strings.TrimSpace(v.Token) != v.Token || strings.ContainsAny(v.Token, "\r\n") || v.Token == admin || v.Token == operator {
		return "", errors.New("legacy reporting credential must be distinct from destination privileged credentials")
	}
	return v.Token, nil
}
func (s *server) observePluginRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.plugins != nil {
			s.plugins.ExcludeCanaryCredentials(s.apiToken(), s.agentCredential(), s.operatorToken, s.legacyAgentToken)
		}
		if s.plugins != nil && s.plugins.ObserveRequest(r) {
			writeError(w, 401, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) bindNetworkInventory() {
	s.plugins.BindNetworkSource(func() ([]plugin.NetworkHost, error) {
		inventories, err := s.store.LatestAll()
		if err != nil {
			return nil, err
		}
		out := []plugin.NetworkHost{}
		for _, inv := range inventories {
			if inv.VPN != nil {
				v := inv.VPN
				out = append(out, plugin.NetworkHost{Host: inv.Host.Hostname, CollectedAt: inv.CollectedAt, InterfaceUp: v.InterfaceUp, LatestHandshake: v.LatestHandshake, Endpoint: v.Endpoint, ExitIP: v.ExitIP, Country: v.Country, City: v.City, Org: v.Org, RXBytes: v.RXBytes, TXBytes: v.TXBytes})
			}
		}
		return out, nil
	})
}
