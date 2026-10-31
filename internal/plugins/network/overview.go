package network

import (
	"context"
	"regexp"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/network/plexload"
)

var loadLabelValue = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

func (n *Network) configureLoad(c plugin.Config, base, token string) error {
	job, _ := c["load_job"].(string)
	if job == "" {
		job = "node_exporter"
	}
	instance, _ := c["load_instance"].(string)
	label, _ := c["load_label"].(string)
	if label == "" {
		label = "Host load"
	}
	for key, value := range map[string]string{"load_job": job, "load_instance": instance, "load_label": label} {
		if len(value) > 256 || strings.ContainsAny(value, "\r\n") {
			return &plugin.FieldError{Key: key, Message: "Use at most 256 characters without line breaks"}
		}
	}
	for key, value := range map[string]string{"load_job": job, "load_instance": instance} {
		if value != "" && !loadLabelValue.MatchString(value) {
			return &plugin.FieldError{Key: key, Message: "Use a simple Prometheus label value"}
		}
	}
	n.load = nil
	if instance != "" {
		n.load = plexload.New(base, token, job, instance, label, plugin.WithoutRedirects(n.host.HTTP()))
	}
	return nil
}

func (n *Network) Overview(ctx context.Context) map[string]any {
	if n.load == nil {
		return nil
	}
	return map[string]any{"plex_load": n.load.Fetch(ctx, n.host.Now().UTC())}
}

var _ plugin.OverviewSource = (*Network)(nil)
