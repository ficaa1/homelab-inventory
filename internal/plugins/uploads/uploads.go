// Package uploads delivers files only to explicitly configured destinations.
package uploads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type Uploads struct {
	paperlessUser, paperlessPassword                  string
	host                                              plugin.Host
	folder, paperless, token, calibre, user, password string
	folderMax, remoteMax                              int64
	recent                                            []record
	loaded                                            bool
}
type record struct {
	Time        time.Time `json:"time"`
	Destination string    `json:"destination"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	Result      string    `json:"result"`
}

var _ plugin.Uploader = (*Uploads)(nil)

func New(h plugin.Host) plugin.Plugin { return &Uploads{host: h} }
func (*Uploads) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "uploads", Name: "Uploads", Description: "Send files to a folder, Paperless-ngx or Calibre-Web.", Version: "1", Tab: &plugin.Tab{Title: "Uploads", Icon: "upload", Views: []plugin.ViewRef{{Name: "recent", Title: "Recent"}}}, Settings: []plugin.Field{
		{Key: "folder_path", Label: "Folder path", Type: plugin.Text, Group: "Folder", Help: "Absolute destination outside the API data directory; use /uploads in Docker."},
		{Key: "folder_max_mb", Label: "Maximum file size (MiB)", Type: plugin.Number, Default: 100, Group: "Folder", Help: "Whole number from 1 to 2048."},
		{Key: "paperless_url", Label: "URL", Type: plugin.URL, Group: "Paperless-ngx"},
		{Key: "paperless_user", Label: "Username", Type: plugin.Text, Group: "Paperless-ngx"},
		{Key: "paperless_password", Label: "Password", Type: plugin.Secret, Group: "Paperless-ngx"},
		{Key: "paperless_token", Label: "API token", Type: plugin.Secret, Group: "Paperless-ngx"},
		{Key: "calibre_url", Label: "URL", Type: plugin.URL, Group: "Calibre-Web"},
		{Key: "calibre_user", Label: "Username", Type: plugin.Text, Group: "Calibre-Web"},
		{Key: "calibre_password", Label: "Password", Type: plugin.Secret, Group: "Calibre-Web"},
		{Key: "remote_max_mb", Label: "Maximum remote file size (MiB)", Type: plugin.Number, Default: 100, Help: "Whole number from 1 to 2048."},
	}}
}
func text(c plugin.Config, key string) string { s, _ := c[key].(string); return s }
func sizeSetting(c plugin.Config, key string) (int64, error) {
	n := 100.0
	if v, ok := c[key]; ok && v != nil {
		if _, err := fmt.Sscan(fmt.Sprint(v), &n); err != nil {
			return 0, &plugin.FieldError{Key: key, Message: "Use a whole number from 1 to 2048"}
		}
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > 2048 || n != math.Trunc(n) {
		return 0, &plugin.FieldError{Key: key, Message: "Use a whole number from 1 to 2048"}
	}
	return int64(n) << 20, nil
}
func baseURL(raw, key string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", &plugin.FieldError{Key: key, Message: "Use an http(s) base URL without credentials, query or fragment"}
	}
	return strings.TrimRight(raw, "/"), nil
}
func (u *Uploads) Configure(c plugin.Config) error {
	folder := text(c, "folder_path")
	if folder != "" {
		if !filepath.IsAbs(folder) || filepath.Clean(folder) != folder {
			return &plugin.FieldError{Key: "folder_path", Message: "Use an absolute, clean destination path"}
		}
		data := filepath.Dir(filepath.Dir(u.host.CacheDir()))
		// Only stat metadata during Configure. Appending /.. without cleaning
		// lets the kernel walk real ancestors even when folder is a symlink.
		info, err := os.Stat(folder)
		if err != nil || !info.IsDir() {
			return &plugin.FieldError{Key: "folder_path", Message: "Destination must be an existing directory"}
		}
		dataInfo, err := os.Stat(data)
		if err != nil {
			return errors.New("API data directory is unavailable")
		}
		ancestor := folder
		for depth := 0; ; depth++ {
			if os.SameFile(info, dataInfo) {
				return &plugin.FieldError{Key: "folder_path", Message: "Destination must be outside the API data directory"}
			}
			parent, statErr := os.Stat(ancestor + "/..")
			if statErr != nil || depth > 256 {
				return &plugin.FieldError{Key: "folder_path", Message: "Destination ancestry could not be checked"}
			}
			if os.SameFile(info, parent) {
				break
			}
			ancestor += "/.."
			info = parent
		}
	}
	paperless, err := baseURL(text(c, "paperless_url"), "paperless_url")
	if err != nil {
		return err
	}
	calibre, err := baseURL(text(c, "calibre_url"), "calibre_url")
	if err != nil {
		return err
	}
	token, user, password := text(c, "paperless_token"), text(c, "calibre_user"), text(c, "calibre_password")
	if paperless != "" || token != "" {
		if paperless == "" {
			return &plugin.FieldError{Key: "paperless_url", Message: "URL is required with a token"}
		}
		if token == "" && (text(c, "paperless_user") == "" || text(c, "paperless_password") == "") {
			return &plugin.FieldError{Key: "paperless_token", Message: "API token is required"}
		}
	}
	if strings.ContainsAny(token, "\r\n") {
		return &plugin.FieldError{Key: "paperless_token", Message: "Token must not contain line breaks"}
	}
	if calibre != "" || user != "" || password != "" {
		for _, f := range [][2]string{{"calibre_url", calibre}, {"calibre_user", user}, {"calibre_password", password}} {
			if f[1] == "" {
				return &plugin.FieldError{Key: f[0], Message: "URL, username and password are required together"}
			}
		}
	}
	if folder == "" && paperless == "" && calibre == "" {
		return &plugin.FieldError{Key: "folder_path", Message: "Configure at least one destination"}
	}
	folderMax, err := sizeSetting(c, "folder_max_mb")
	if err != nil {
		return err
	}
	remoteMax, err := sizeSetting(c, "remote_max_mb")
	if err != nil {
		return err
	}
	u.folder, u.paperless, u.token, u.calibre, u.user, u.password = folder, paperless, token, calibre, user, password
	u.paperlessUser = text(c, "paperless_user")
	u.paperlessPassword = text(c, "paperless_password")
	u.folderMax, u.remoteMax = folderMax, remoteMax
	return nil
}
func (u *Uploads) UploadTargets() []plugin.UploadTarget {
	out := []plugin.UploadTarget{}
	if u.folder != "" {
		out = append(out, plugin.UploadTarget{ID: "folder", Label: "Folder", Help: "Save a file without replacing existing files.", MaxBytes: u.folderMax})
	}
	if u.paperless != "" {
		out = append(out, plugin.UploadTarget{ID: "paperless", Label: "Paperless-ngx", Accept: []string{".pdf", ".png", ".jpg", ".jpeg", ".tif", ".tiff", ".webp", ".gif", ".txt"}, MaxBytes: u.remoteMax})
	}
	if u.calibre != "" {
		out = append(out, plugin.UploadTarget{ID: "calibre", Label: "Calibre-Web", Accept: []string{".epub", ".pdf", ".mobi", ".azw3", ".cbz", ".fb2"}, MaxBytes: u.remoteMax})
	}
	return out
}
func (u *Uploads) loadRecent() {
	if u.loaded {
		return
	}
	u.loaded = true
	file, err := os.Open(filepath.Join(u.host.CacheDir(), "recent.json"))
	if err != nil {
		return
	}
	defer file.Close()
	var entries []record
	if json.NewDecoder(io.LimitReader(file, 256<<10)).Decode(&entries) == nil {
		if len(entries) > 100 {
			entries = entries[len(entries)-100:]
		}
		u.recent = entries
	}
}
func (u *Uploads) Upload(ctx context.Context, target, name string, size int64, body io.Reader) (plugin.UploadResult, error) {
	var result plugin.UploadResult
	var err error
	switch {
	case target == "folder" && u.folder != "":
		result, err = u.writeFolder(ctx, name, size, body)
	case target == "paperless" && u.paperless != "":
		result, err = u.sendPaperless(ctx, name, size, body)
	case target == "calibre" && u.calibre != "":
		result, err = u.sendCalibre(ctx, name, size, body)
	default:
		return result, plugin.ErrNotFound
	}
	u.loadRecent()
	message := result.Message
	actualName := result.Name
	if actualName == "" {
		actualName = name
	}
	if err != nil {
		message = u.safe(err.Error())
		err = errors.New(message)
	}
	u.recent = append(u.recent, record{u.host.Now().UTC(), target, u.safe(actualName), size, u.safe(message)})
	if len(u.recent) > 100 {
		u.recent = u.recent[len(u.recent)-100:]
	}
	data, _ := json.Marshal(u.recent)
	if fsx.WriteAtomic(filepath.Join(u.host.CacheDir(), "recent.json"), data, 0600) != nil {
		u.host.Logf("could not persist recent upload metadata")
	}
	return result, err
}
func (u *Uploads) View(_ context.Context, name string, _ url.Values) (plugin.View, error) {
	if name != "recent" {
		return plugin.View{}, plugin.ErrNotFound
	}
	u.loadRecent()
	rows := []map[string]any{}
	for i := len(u.recent) - 1; i >= 0; i-- {
		r := u.recent[i]
		rows = append(rows, map[string]any{"time": r.Time.Format(time.RFC3339), "destination": u.safe(r.Destination), "name": u.safe(r.Name), "size": r.Size, "result": u.safe(r.Result)})
	}
	v := plugin.View{Title: "Recent uploads", UpdatedAt: u.host.Now().UTC(), RefreshSeconds: 30, Blocks: []plugin.Block{{Type: "table", Title: "Recent uploads", Empty: "No uploads yet", Columns: []plugin.Column{{Key: "time", Label: "Time", Format: "time"}, {Key: "destination", Label: "Destination"}, {Key: "name", Label: "Name"}, {Key: "size", Label: "Size", Format: "bytes"}, {Key: "result", Label: "Result"}}, Rows: rows}}}
	if len(u.UploadTargets()) == 0 {
		v.Blocks = append(v.Blocks, plugin.Block{Type: "notice", Tone: "info", Text: "No upload destinations are configured."})
	}
	return v, nil
}
func (u *Uploads) Close() error {
	u.folder, u.paperless, u.token, u.calibre, u.user, u.password = "", "", "", "", "", ""
	u.paperlessUser = ""
	u.paperlessPassword = ""
	u.recent = nil
	u.loaded = false
	return nil
}
func (u *Uploads) safe(s string) string {
	for _, secret := range []string{u.token, u.password, u.paperlessPassword} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r < ' ' {
			return ' '
		}
		return r
	}, s)
	runes := []rune(s)
	if len(runes) > 400 {
		s = string(runes[:400])
	}
	return strings.TrimSpace(s)
}
func (u *Uploads) Test(ctx context.Context) plugin.TestResult {
	out := plugin.TestResult{OK: true, Checks: []plugin.Check{}}
	for _, t := range u.UploadTargets() {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var err error
		switch t.ID {
		case "folder":
			var f *os.File
			f, err = os.CreateTemp(u.folder, ".hi-upload-check-*")
			if err == nil {
				name := f.Name()
				err = f.Close()
				if removeErr := os.Remove(name); err == nil {
					err = removeErr
				}
			}
		case "paperless":
			var token string
			token, err = u.paperlessAuth(checkCtx)
			if err == nil {
				_, err = u.remote(checkCtx, plugin.WithoutRedirects(u.host.HTTP()), "GET", u.paperless+"/api/", nil, 0, "", "Token "+token)
			}
		case "calibre":
			_, err = u.calibreLogin(checkCtx)
		}
		cancel()
		check := plugin.Check{Name: t.Label, OK: err == nil, Message: "Destination is available"}
		if err != nil {
			check.Message = "Destination check failed"
		}
		out.Checks = append(out.Checks, check)
		out.OK = out.OK && check.OK
	}
	if len(out.Checks) == 0 {
		out.OK = false
	}
	if out.OK {
		out.Message = "All configured destinations responded"
	} else {
		out.Message = "One or more destinations could not be checked"
	}
	return out
}
