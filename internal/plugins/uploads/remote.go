package uploads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// The envelope has a known length so Python upload handlers need not accept
// chunked requests. File contents stream directly from the incoming request.
func multipartBody(fields url.Values, field, name string, size int64, body io.Reader) (io.Reader, int64, string) {
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	for key, values := range fields {
		for _, value := range values {
			_ = writer.WriteField(key, value)
		}
	}
	escape := strings.NewReplacer("\\", "\\\\", `"`, `\"`)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escape.Replace(field), escape.Replace(name)))
	header.Set("Content-Type", "application/octet-stream")
	_, _ = writer.CreatePart(header)
	suffix := "\r\n--" + writer.Boundary() + "--\r\n"
	return io.MultiReader(&prefix, body, strings.NewReader(suffix)), int64(prefix.Len()) + size + int64(len(suffix)), writer.FormDataContentType()
}
func (u *Uploads) remote(ctx context.Context, client *http.Client, method, endpoint string, body io.Reader, size int64, contentType, authorization string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("Destination request could not be created")
	}
	req.ContentLength = size
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if strings.HasSuffix(endpoint, "/upload") {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Accept", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Destination request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Destination returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errors.New("Destination response could not be read")
	}
	return data, nil
}
func (u *Uploads) sendPaperless(ctx context.Context, name string, size int64, body io.Reader) (plugin.UploadResult, error) {
	reader, length, contentType := multipartBody(nil, "document", name, size, body)
	// Preserve the host guard, response cap and redirect policy. A longer timeout
	// is needed only while this request streams the uploaded file.
	client := *u.host.HTTP()
	client.Timeout = plugin.UploadTimeout
	data, err := u.remote(ctx, &client, "POST", u.paperless+"/api/documents/post_document/", reader, length, contentType, "Token "+u.token)
	if err != nil {
		return plugin.UploadResult{}, err
	}
	var task string
	if json.Unmarshal(data, &task) != nil || strings.TrimSpace(task) == "" {
		return plugin.UploadResult{}, errors.New("Paperless-ngx did not return a task id")
	}
	return plugin.UploadResult{Name: name, Message: "Paperless-ngx queued task " + u.safe(task)}, nil
}

var inputRE = regexp.MustCompile(`(?is)<input\b[^>]*>`)
var attributeRE = regexp.MustCompile(`(?i)([a-z_][a-z0-9_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
var bookRE = regexp.MustCompile(`/book/([0-9]+)(?:/|$|[?#])`)
var flashRE = regexp.MustCompile(`(?is)<div\b[^>]*class=["'][^"']*alert-danger[^"']*["'][^>]*>(.*?)</div>`)
var tagRE = regexp.MustCompile(`(?s)<[^>]*>`)

func csrfToken(page []byte) string {
	for _, input := range inputRE.FindAll(page, -1) {
		attributes := map[string]string{}
		for _, a := range attributeRE.FindAllSubmatch(input, -1) {
			attributes[strings.ToLower(string(a[1]))] = html.UnescapeString(string(a[2]) + string(a[3]))
		}
		if attributes["name"] == "csrf_token" {
			return attributes["value"]
		}
	}
	return ""
}
func (u *Uploads) calibreLogin(ctx context.Context) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	client := plugin.WithCookieJar(u.host.HTTP(), jar)
	page, err := u.remote(ctx, client, "GET", u.calibre+"/login", nil, 0, "", "")
	if err != nil {
		return nil, err
	}
	csrf := csrfToken(page)
	if csrf == "" {
		return nil, errors.New("Calibre-Web login page has no CSRF token")
	}
	values := url.Values{"username": {u.user}, "password": {u.password}, "csrf_token": {csrf}}
	req, err := http.NewRequestWithContext(ctx, "POST", u.calibre+"/login", strings.NewReader(values.Encode()))
	if err != nil {
		return nil, errors.New("Calibre-Web login request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Calibre-Web login request failed")
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil || resp.StatusCode != 200 || resp.Request == nil || strings.HasSuffix(strings.TrimRight(resp.Request.URL.Path, "/"), "/login") {
		return nil, errors.New("Calibre-Web login refused")
	}
	return client, nil
}
func (u *Uploads) sendCalibre(ctx context.Context, name string, size int64, body io.Reader) (plugin.UploadResult, error) {
	client, err := u.calibreLogin(ctx)
	if err != nil {
		return plugin.UploadResult{}, err
	}
	page, err := u.remote(ctx, client, "GET", u.calibre+"/", nil, 0, "", "")
	if err != nil {
		return plugin.UploadResult{}, err
	}
	csrf := csrfToken(page)
	if csrf == "" || (!strings.Contains(string(page), `id="form-upload"`) && !strings.Contains(string(page), `id='form-upload'`)) {
		return plugin.UploadResult{}, errors.New("Calibre-Web account cannot upload")
	}
	reader, length, contentType := multipartBody(url.Values{"csrf_token": {csrf}}, "btn-upload", name, size, body)
	client.Timeout = plugin.UploadTimeout
	data, err := u.remote(ctx, client, "POST", u.calibre+"/upload", reader, length, contentType, "")
	if err != nil {
		return plugin.UploadResult{}, err
	}
	var response struct {
		Location string `json:"location"`
	}
	if json.Unmarshal(data, &response) != nil {
		return plugin.UploadResult{}, errors.New("Calibre-Web returned an invalid upload response")
	}
	// Only extract a numeric book id. Never follow upstream-supplied locations.
	if match := bookRE.FindStringSubmatch(response.Location); match != nil {
		return plugin.UploadResult{Name: name, Message: "Added book " + match[1]}, nil
	}
	message := "Calibre-Web refused the file"
	if page, err = u.remote(ctx, client, "GET", u.calibre+"/", nil, 0, "", ""); err == nil {
		if match := flashRE.FindSubmatch(page); match != nil {
			flash := u.safe(html.UnescapeString(tagRE.ReplaceAllString(string(match[1]), "")))
			if flash != "" {
				message = "Calibre-Web: " + flash
			}
		}
	}
	return plugin.UploadResult{}, errors.New(u.safe(message))
}
