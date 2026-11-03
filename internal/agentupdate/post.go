package agentupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Config is private local configuration for both posting and optional updates.
// Credentials stay in a separate mode-0600 file and never reach discover.
type Config struct {
	Dir       string `json:"dir"`
	Public    string `json:"public"`
	URL       string `json:"api_url"`
	TokenFile string `json:"token_file"`
	Discover  string `json:"discover"`
	AllowHTTP bool   `json:"allow_http,omitempty"`
	Signed    bool   `json:"signed"`
}

func WriteConfig(path string, c Config) error {
	if err := ValidateURL(c.URL, c.AllowHTTP); err != nil {
		return err
	}
	if c.Dir == "" || c.Discover == "" || c.TokenFile == "" {
		return errors.New("state, discover and token-file paths required")
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(path, data, 0600)
}
func ReadConfig(path string, allowHTTP bool) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	if err != nil {
		return c, errors.New("invalid agent config")
	}
	c.AllowHTTP = c.AllowHTTP || allowHTTP
	return c, ValidateURL(c.URL, c.AllowHTTP)
}
func ValidateURL(value string, allowHTTP bool) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return errors.New("API URL must be an http(s) origin without credentials, query or path")
	}
	if u.Scheme == "http" && !allowHTTP {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("plain HTTP exposes the agent credential; use HTTPS or explicitly allow LAN HTTP with --allow-http / ALLOW_HTTP=1")
		}
	}
	return nil
}

// ReadToken refuses shared-readable files. Do not include file content in errors.
func ReadToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open token file")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return "", errors.New("token file must be a private regular file (mode 0600)")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", errors.New("cannot read token file")
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("token file must contain one non-empty line")
	}
	return token, nil
}

// Post runs a read-only collector, validates its output, and posts the exact
// discover fields, merging only agent_update. A failed command never posts inventory.
// A private temporary file bounds memory use and is removed on every outcome.
func (r Runner) Post(ctx context.Context, discover string) (postErr error) {
	u := inventory.AgentUpdate{Result: "ok"}
	writable := false
	var release func()
	// Lock/state failures can still use the last safe status for first-failure
	// continuity, but only the lock owner may persist a new local status.
	if b, e := os.ReadFile(filepath.Join(r.Dir, "post-status.json")); e == nil {
		var old inventory.AgentUpdate
		if json.Unmarshal(b, &old) == nil {
			u = old
		}
	}
	r.stamp(&u, "unsigned")
	u.AttemptedAt = time.Now().UTC()
	defer func() {
		if postErr != nil {
			failed(&u, "state", postErr)
		} else {
			resolved(&u)
		}
		r.stamp(&u, "unsigned")
		if writable {
			if b, e := json.Marshal(u); e == nil {
				if e = fsx.WriteAtomic(filepath.Join(r.Dir, "post-status.json"), b, 0600); e != nil {
					failed(&u, "state", e)
				}
			}
		}
		if release != nil {
			release()
		}
		if e := r.postStatus(ctx, u); postErr == nil && e != nil {
			postErr = e
		}
	}()
	if r.Token == "" {
		return errors.New("bearer token required")
	}
	if err := ValidateURL(r.URL, r.AllowHTTP); err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0700); err != nil {
		return err
	}
	unlock, err := fsx.LockDir(r.Dir)
	if err != nil {
		if errors.Is(err, fsx.ErrLocked) {
			return fault("lock", "lock_busy", nil)
		}
		return classify("lock", err)
	}
	release = unlock
	writable = true
	f, err := os.CreateTemp(r.Dir, ".inventory-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if err = command(ctx, discover, []string{"-compact"}, &limitedFile{file: f, left: 16 << 20}); err != nil {
		return executionError("trial", err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var inv inventory.Inventory
	dec := json.NewDecoder(f)
	if dec.Decode(&inv) != nil || inv.Host.Hostname == "" || inv.SchemaVersion != inventory.SchemaVersion || dec.Decode(new(any)) != io.EOF {
		return trialFault("trial_invalid_output", nil)
	}
	if r.Host == "" {
		r.Host = inv.Host.Hostname
	}
	if inv.Host.Hostname != r.Host {
		return trialFault("trial_invalid_output", nil)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	data, err := mergeUpdate(raw, u)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &inv) != nil || inv.Host.Hostname != r.Host || inv.SchemaVersion != inventory.SchemaVersion {
		return trialFault("trial_invalid_output", nil)
	}
	_, _, err = r.request(ctx, "POST", "/inventory", bytes.NewReader(data), 65536)
	if err != nil {
		return fault("post", "post_rejected", err)
	}
	return nil
}

type limitedFile struct {
	file *os.File
	left int
}

func (w *limitedFile) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errOutputTooLarge
	}
	n, err := w.file.Write(p)
	w.left -= n
	return n, err
}

// ConfigRunner builds a runner without passing credentials through the shell.
func ConfigRunner(c Config) (Runner, error) {
	token, err := ReadToken(c.TokenFile)
	if err != nil {
		return Runner{}, err
	}
	host, err := os.Hostname()
	if err != nil {
		return Runner{}, err
	}
	return Runner{Dir: filepath.Clean(c.Dir), Public: c.Public, URL: strings.TrimRight(c.URL, "/"), Token: token, Host: host, AllowHTTP: c.AllowHTTP}, nil
}
