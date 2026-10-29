package agentupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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
// bytes, including partial sections and diagnostics. A failed command never posts.
// A private temporary file bounds memory use and is removed on every outcome.
func (r Runner) Post(ctx context.Context, discover string) error {
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
		return errors.New("another run holds the lock or state is unavailable")
	}
	defer unlock()
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
		return errors.New("discover failed; inventory not posted")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var inv inventory.Inventory
	dec := json.NewDecoder(f)
	if dec.Decode(&inv) != nil || inv.Host.Hostname == "" || inv.SchemaVersion != inventory.SchemaVersion || dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid discover output; inventory not posted")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, _, err = r.request(ctx, "POST", "/inventory", f, 65536)
	return err
}

type limitedFile struct {
	file *os.File
	left int
}

func (w *limitedFile) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errors.New("discover output too large")
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
