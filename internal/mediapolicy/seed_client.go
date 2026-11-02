package mediapolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"

	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

type SeedTorrent struct {
	Hash         string   `json:"hash"`
	Name         string   `json:"name"`
	SavePath     string   `json:"save_path"`
	DownloadPath *string  `json:"download_path"`
	ContentPath  string   `json:"content_path"`
	Size         int64    `json:"total_size"`
	Progress     *float64 `json:"progress"`
	AmountLeft   *int64   `json:"amount_left"`
	State        string   `json:"state"`
	AutoTMM      *bool    `json:"auto_tmm"`
}
type SeedTorrentFile struct {
	Index    int      `json:"index"`
	Name     string   `json:"name"`
	Size     int64    `json:"size"`
	Progress *float64 `json:"progress"`
}

// SeedQbit is separate from the read-only network health client. It never adds,
// deletes, renames, reannounces or changes the identity of a torrent.
type SeedQbit struct {
	Base, User, Password string
	HTTP                 *http.Client
	mu                   sync.Mutex
	logged               bool
}

func NewSeedQbit(base, user, password string, h *http.Client) *SeedQbit {
	cp := *h
	cp.Jar, _ = cookiejar.New(nil)
	return &SeedQbit{Base: strings.TrimRight(base, "/"), User: user, Password: password, HTTP: &cp}
}

var errSeedForbidden = errors.New("qBittorrent HTTP 403")

// call re-authenticates once when a read answers 403: qBittorrent's WebUI
// session expires after an hour without requests, and a long-lived client
// otherwise stays locked out. Writes are never repeated; every write follows a
// read, which has already refreshed the session.
func (q *SeedQbit) call(ctx context.Context, method, p string, v url.Values, out any) error {
	e := q.do(ctx, method, p, v, out)
	if !errors.Is(e, errSeedForbidden) || method != "GET" {
		return e
	}
	q.mu.Lock()
	q.logged = false
	q.mu.Unlock()
	if e := q.Login(ctx); e != nil {
		return e
	}
	return q.do(ctx, method, p, v, out)
}
func (q *SeedQbit) do(ctx context.Context, method, p string, v url.Values, out any) error {
	u, e := url.Parse(q.Base)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || q.User == "" || q.Password == "" || q.HTTP == nil {
		return fmt.Errorf("qBittorrent is not configured")
	}
	endpoint := q.Base + "/api/v2/" + p
	var body io.Reader
	if method == "GET" {
		if len(v) > 0 {
			endpoint += "?" + v.Encode()
		}
	} else {
		body = strings.NewReader(v.Encode())
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, body)
	if e != nil {
		return fmt.Errorf("qBittorrent request failed")
	}
	req.Header.Set("Referer", q.Base+"/")
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	cp := *q.HTTP
	cp.CheckRedirect = httpx.RefuseRedirects()
	resp, e := cp.Do(req)
	if e != nil {
		return fmt.Errorf("qBittorrent transport failed; outcome unknown")
	}
	defer resp.Body.Close()
	if p == "auth/login" {
		// qBittorrent before 5.1 answers a good login 200 "Ok." (and a bad
		// one 200 "Fails."); 5.1 and later answer 204 with no body.
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, 16))
		if resp.StatusCode != 200 || e != nil || string(b) != "Ok." {
			return fmt.Errorf("qBittorrent login refused")
		}
		return nil
	}
	if resp.StatusCode == http.StatusForbidden {
		return errSeedForbidden
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("qBittorrent HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if s, ok := out.(*string); ok {
			b, e := io.ReadAll(io.LimitReader(resp.Body, 256))
			if e != nil {
				return fmt.Errorf("qBittorrent response failed")
			}
			*s = string(b)
		} else if json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out) != nil {
			return fmt.Errorf("qBittorrent response invalid")
		}
	}
	if p == "torrents/setDownloadPath" {
		// The pinned endpoint returns an empty body. A truncated or unexpected
		// response leaves the intent unresolved, even if the server applied it.
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1))
		if err != nil || len(b) != 0 {
			return fmt.Errorf("qBittorrent response unclear; outcome unknown")
		}
	}
	return nil
}
func (q *SeedQbit) Login(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.logged {
		return nil
	}
	if e := q.do(ctx, "POST", "auth/login", url.Values{"username": {q.User}, "password": {q.Password}}, nil); e != nil {
		return e
	}
	q.logged = true
	return nil
}
func (q *SeedQbit) Reviewed(ctx context.Context) error {
	if e := q.Login(ctx); e != nil {
		return e
	}
	var version string
	if e := q.call(ctx, "GET", "app/version", nil, &version); e != nil {
		return e
	}
	if version != "v5.2.3" {
		return fmt.Errorf("qBittorrent version is not reviewed")
	}
	var build struct {
		Libtorrent string `json:"libtorrent"`
	}
	if e := q.call(ctx, "GET", "app/buildInfo", nil, &build); e != nil {
		return e
	}
	if build.Libtorrent != "2.0.14.0" && build.Libtorrent != "2.0.14" {
		return fmt.Errorf("libtorrent version is not reviewed (requires 2.0.14)")
	}
	var preferences struct {
		Extensions *bool `json:"incomplete_files_ext"`
	}
	if e := q.call(ctx, "GET", "app/preferences", nil, &preferences); e != nil {
		return e
	}
	// A failed check must not rename hardlinks or move them to an incomplete
	// path. qBittorrent performs those adjustments even for stopped torrents.
	if preferences.Extensions == nil || *preferences.Extensions {
		return fmt.Errorf("incomplete file extensions must be verified disabled")
	}
	return nil
}
func (q *SeedQbit) Torrents(ctx context.Context) ([]SeedTorrent, error) {
	if e := q.Login(ctx); e != nil {
		return nil, e
	}
	var out *[]SeedTorrent
	e := q.call(ctx, "GET", "torrents/info", nil, &out)
	if e != nil {
		return nil, e
	}
	if out == nil {
		return nil, fmt.Errorf("torrent inventory missing")
	}
	seen := map[string]bool{}
	for _, t := range *out {
		if !ValidSeedHash(t.Hash) || seen[t.Hash] || t.SavePath == "" || t.State == "" || t.Progress == nil || t.AmountLeft == nil || t.DownloadPath == nil || t.AutoTMM == nil {
			return nil, fmt.Errorf("torrent inventory incomplete")
		}
		seen[t.Hash] = true
	}
	return *out, nil
}

var errSeedNoFiles = errors.New("torrent files missing")

// PeerFiles is Files for another torrent checked for shared files. A torrent
// still fetching its metadata has no file list and has written nothing, so it
// shares nothing; any other empty list stays an error.
func (q *SeedQbit) PeerFiles(ctx context.Context, t SeedTorrent) ([]SeedTorrentFile, error) {
	fs, e := q.Files(ctx, t.Hash)
	if errors.Is(e, errSeedNoFiles) && (t.State == "metaDL" || t.State == "forcedMetaDL") {
		return nil, nil
	}
	return fs, e
}

func (q *SeedQbit) Files(ctx context.Context, hash string) ([]SeedTorrentFile, error) {
	if !ValidSeedHash(hash) {
		return nil, fmt.Errorf("invalid hash")
	}
	var out *[]SeedTorrentFile
	e := q.call(ctx, "GET", "torrents/files", url.Values{"hash": {hash}}, &out)
	if e != nil {
		return nil, e
	}
	if out == nil || len(*out) == 0 {
		return nil, errSeedNoFiles
	}
	seen := map[int]bool{}
	for _, f := range *out {
		if f.Index < 0 || seen[f.Index] || f.Progress == nil || f.Size < 0 {
			return nil, fmt.Errorf("torrent files incomplete")
		}
		seen[f.Index] = true
	}
	return *out, nil
}
func (q *SeedQbit) Write(ctx context.Context, action, hash, location string) error {
	if !ValidSeedHash(hash) {
		return fmt.Errorf("invalid hash")
	}
	switch action {
	case "stop", "start", "recheck", "setLocation":
	case "setDownloadPath":
		if location != "" {
			return fmt.Errorf("only clearing the download path is allowed")
		}
		// Unlike setLocation, 5.2.3 requires id, not hashes. Include path even
		// when empty: omitting it is a different, rejected API request.
		return q.call(ctx, "POST", "torrents/"+action, url.Values{"id": {hash}, "path": {""}}, nil)
	default:
		return fmt.Errorf("torrent write refused")
	}
	v := url.Values{"hashes": {hash}}
	if action == "setLocation" {
		v.Set("location", location)
	}
	return q.call(ctx, "POST", "torrents/"+action, v, nil)
}
