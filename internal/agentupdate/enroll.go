package agentupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	// Persist the retry identity before sending anything. A lost response can
	// redeem the same single-use code only from this installation and hostname.
	noncePath := tokenFile + ".enrollment"
	hash := fmtHash(code)
	var saved struct {
		CodeHash string `json:"code_hash"`
		Nonce    string `json:"nonce"`
	}
	if b, e := os.ReadFile(noncePath); e == nil {
		if json.Unmarshal(b, &saved) != nil {
			return errors.New("invalid enrollment retry record")
		}
	}
	if saved.CodeHash != hash || len(saved.Nonce) != 32 {
		var b [16]byte
		if _, e := rand.Read(b[:]); e != nil {
			return errors.New("creating enrollment retry identity")
		}
		saved.CodeHash, saved.Nonce = hash, hex.EncodeToString(b[:])
		bjson, _ := json.Marshal(saved)
		if e := os.MkdirAll(filepath.Dir(tokenFile), 0700); e != nil {
			return errors.New("saving enrollment retry identity")
		}
		if e := fsx.WriteAtomic(noncePath, bjson, 0600); e != nil {
			return errors.New("saving enrollment retry identity")
		}
	}
	body, _ := json.Marshal(map[string]string{"code": code, "hostname": host, "nonce": saved.Nonce})
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

func fmtHash(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
