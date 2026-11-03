package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/actions"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/vpnwatch"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	s.plugins.BindVPNPanelSource(func(ctx context.Context) {
		if s.vpnWatch != nil {
			go func() {
				checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				_ = s.vpnWatch.Check(checkCtx, true)
			}()
		}
	})
	s.plugins.BindNetworkSource(func() ([]plugin.NetworkHost, error) {
		inventories, err := s.store.LatestAll()
		if err != nil {
			return nil, err
		}
		manager := s.actions
		if manager == nil {
			manager, _ = actions.Open(s.store.Dir(), nil)
		}
		out := []plugin.NetworkHost{}
		for _, inv := range inventories {
			for _, v := range inv.VPNGateways() {
				out = append(out, vpnwatch.Project(inv, &v, s.vpnWatch, manager, s.actionPolicy.Host, s.exec != nil))
			}
		}
		return out, nil
	})
}
