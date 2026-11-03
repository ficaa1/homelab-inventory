package agentupdate

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
)

// Worker has no credential, lock, floor writer or POST path. The launcher
// brokers credential-bearing fetches on its private pipe and verifies the plan.
func Worker(ctx context.Context, in io.Reader, out io.Writer) error {
	if err := worker(ctx, in, out); err != nil {
		return json.NewEncoder(out).Encode(agentproto.Message{Protocol: agentproto.Version, Operation: "error", Code: "trial_invalid_output"})
	}
	return nil
}
func worker(ctx context.Context, in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(io.LimitReader(in, 131072))
	enc := json.NewEncoder(out)
	var req agentproto.Input
	if dec.Decode(&req) != nil || req.Protocol != agentproto.Version {
		return agentproto.ErrOutput
	}
	reply := agentproto.Message{Protocol: agentproto.Version}
	switch req.Operation {
	case "plan":
		if err := enc.Encode(agentproto.Message{Protocol: agentproto.Version, Operation: "offer"}); err != nil {
			return err
		}
		var offer agentproto.Message
		if dec.Decode(&offer) != nil || offer.Protocol != agentproto.Version || offer.Operation != "offer" {
			return agentproto.ErrOutput
		}
		reply.Operation = "plan"
		if len(offer.Envelope) > 0 {
			m, e := agentrelease.Verify(offer.Envelope, req.Public)
			if e != nil {
				return e
			}
			if m.LauncherProtocol == agentproto.Version {
				reply.Envelope = offer.Envelope
			}
		}
	case "collect":
		var buf cappedOutput
		if e := command(ctx, req.Discover, []string{"-compact"}, &buf); e != nil {
			reply.Operation = "error"
			reply.Code = classify("trial", executionError("trial", e)).code
			return enc.Encode(reply)
		}
		data, e := mergeUpdate(buf.Bytes(), req.Update)
		if e != nil {
			return e
		}
		raw, e := agentproto.Object(data)
		if e != nil {
			return e
		}
		// A collector cannot supply or disguise the launcher's transaction identity.
		for k := range raw {
			if k != "agent_runtime" && strings.EqualFold(k, "agent_runtime") {
				return agentproto.ErrOutput
			}
		}
		raw["agent_runtime"], _ = json.Marshal(agentproto.Runtime{Protocol: agentproto.Version, Nonce: req.Nonce, Identity: req.Identity})
		data, e = json.Marshal(raw)
		if e != nil {
			return e
		}
		if _, e = agentproto.ValidateReport(data, req.Host, req.Nonce, req.Identity); e != nil {
			return e
		}
		reply.Operation = "report"
		reply.Report = data
	default:
		return agentproto.ErrOutput
	}
	return enc.Encode(reply)
}
