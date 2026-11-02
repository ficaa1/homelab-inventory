package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

// NewSendTool sends options as JSON rather than query parameters.
func NewSendTool(name, description, path string, options ...Option) Tool {
	return Tool{Name: name, Description: description, Method: http.MethodPost, options: options,
		Path: func(map[string]any) (string, url.Values, error) { return path, nil, nil }}
}

func (s *Server) post(ctx context.Context, u string, payload any, token string, shrink bool) ([]byte, int, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := *s.client
	if shrink {
		client.Timeout = 6 * time.Minute
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	return body, resp.StatusCode, err
}
