package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/ficaa1/homelab-inventory/internal/actions"
)

type actionRequest struct {
	Host             json.RawMessage `json:"host,omitempty"`
	Kind             string          `json:"kind"`
	Target           string          `json:"target"`
	Confirm          bool            `json:"confirm"`
	ContainerSetHash string          `json:"container_set_hash"`
}

func decodeAction(w http.ResponseWriter, r *http.Request, host string) (actionRequest, bool) {
	var in actionRequest
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json required")
		return in, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid action JSON")
		return in, false
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "one JSON object required")
		return in, false
	}
	if len(in.Host) != 0 {
		var requested string
		if string(in.Host) == "null" || json.Unmarshal(in.Host, &requested) != nil || requested != host {
			writeError(w, http.StatusBadRequest, "host must match ACTIONS_HOST")
			return in, false
		}
	}
	return in, true
}
func (s *server) handleActionPreview(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeAction(w, r, s.actionPolicy.Host)
	if !ok {
		return
	}
	inv, err := s.store.Latest(s.actionPolicy.Host)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "latest target host inventory unavailable")
		return
	}
	p, err := actions.Resolve(inv, in.Kind, in.Target, s.actionPolicy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}
func (s *server) handleActionExecute(w http.ResponseWriter, r *http.Request) {
	actor, authorized := s.agentSession(r)
	if !authorized {
		writeError(w, 403, "operator token required or route disabled")
		return
	}
	in, ok := decodeAction(w, r, s.actionPolicy.Host)
	if !ok {
		return
	}
	if !in.Confirm {
		writeError(w, http.StatusBadRequest, "confirm: true required")
		return
	}
	if s.actions == nil {
		writeError(w, http.StatusServiceUnavailable, "restart execution unavailable")
		return
	}
	e, err := s.actions.Execute(r.Context(), in.Kind, in.Target, actor, in.ContainerSetHash)
	if err != nil {
		var changed *actions.PreviewChanged
		if errors.As(err, &changed) {
			writeJSON(w, http.StatusConflict, struct {
				Error   string          `json:"error"`
				Preview actions.Preview `json:"preview"`
			}{err.Error(), changed.Preview})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if e.Result != "ok" {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, e)
}
func (s *server) handleActionLog(w http.ResponseWriter, r *http.Request) {
	if s.actions == nil {
		writeError(w, http.StatusServiceUnavailable, "restart execution unavailable")
		return
	}
	n := actions.LogLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		v, err := strconv.Atoi(q)
		if err != nil || v < 1 || v > actions.LogLimit {
			writeError(w, http.StatusBadRequest, "limit must be 1..100")
			return
		}
		n = v
	}
	writeJSON(w, http.StatusOK, s.actions.Log(n))
}
func (s *server) handleActionStacks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("host") && r.URL.Query().Get("host") != s.actionPolicy.Host {
		writeJSON(w, http.StatusOK, []actions.Stack{})
		return
	}
	inv, err := s.store.Latest(s.actionPolicy.Host)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "latest target host inventory unavailable")
		return
	}
	if inv.Docker == nil || !inv.Docker.Available || inv.Docker.Containers == nil {
		writeError(w, http.StatusServiceUnavailable, "target host Docker inventory unavailable")
		return
	}
	writeJSON(w, http.StatusOK, actions.Stacks(inv))
}
