package agentupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

// Enroll redeems a short-lived code and writes only the agent credential. The
// response and credential never pass through a shell or an error message.
func Enroll(ctx context.Context, origin, code, host, tokenFile string, allowHTTP bool) error {
	if err := ValidateURL(origin, allowHTTP); err != nil {
		return err
	}
	if tokenFile == "" {
		return errors.New("enrollment requires -token-file")
	}
	body, _ := json.Marshal(map[string]string{"code": code, "hostname": host})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(origin, "/")+"/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid enrollment request")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: httpx.RefuseRedirects()}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("enrollment request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("enrollment refused; request a fresh code or wait for the rate limit")
	}
	var out struct {
		Token string `json:"agent_token"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out) != nil || len(out.Token) < 32 || strings.ContainsAny(out.Token, "\r\n") {
		return errors.New("invalid enrollment response")
	}
	return fsx.WriteAtomic(tokenFile, []byte(out.Token+"\n"), 0600)
}
