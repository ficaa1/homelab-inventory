package media

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

const cacheTTL = 20 * time.Second
const maxImages = 2048
const maxCacheBytes = 16 << 20

type cached struct {
	body []byte
	err  error
	at   time.Time
}
type imagePath struct{ service, path string }

func (m *Media) request(ctx context.Context, s service, path string, q url.Values) (*http.Response, error) {
	u, err := url.Parse(s.base + path)
	if err != nil {
		return nil, errors.New(s.name + ": invalid request")
	}
	query := url.Values{}
	for k, v := range q {
		query[k] = append([]string{}, v...)
	}
	if s.id == "tautulli" {
		query.Set("apikey", s.key)
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New(s.name + ": invalid request")
	}
	if s.id != "tautulli" {
		req.Header.Set("X-Api-Key", s.key)
	}
	resp, err := m.host.HTTP().Do(req)
	// Do not wrap upstream errors: even url.Error may contain the query API key.
	if err != nil {
		return nil, errors.New(s.name + ": request failed")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", s.name, resp.StatusCode)
	}
	return resp, nil
}
func (m *Media) get(ctx context.Context, s service, path string, q url.Values, out any, useCache bool) error {
	cacheKey := s.id + path + "?" + q.Encode() // No API key is ever part of a cache key.
	c, found := m.cache[cacheKey]
	if !useCache || !found || m.host.Now().Sub(c.at) >= cacheTTL {
		c = cached{at: m.host.Now()}
		resp, err := m.request(ctx, s, path, q)
		if err != nil {
			c.err = err
		} else {
			c.body, err = io.ReadAll(io.LimitReader(resp.Body, plugin.MaxHTTPBody+1))
			resp.Body.Close()
			if err != nil || len(c.body) > plugin.MaxHTTPBody {
				c.body = nil
				c.err = errors.New(s.name + ": response could not be read")
			}
		}
		if useCache {
			if len(m.cache) >= 64 || m.cacheBytes+len(c.body) > maxCacheBytes {
				m.cache = map[string]cached{}
				m.cacheBytes = 0
			}
			m.cacheBytes -= len(m.cache[cacheKey].body)
			m.cache[cacheKey] = c
			m.cacheBytes += len(c.body)
		}
	}
	if c.err != nil {
		return c.err
	}
	data := c.body
	if s.id == "tautulli" {
		var env struct {
			Response struct {
				Result string
				Data   json.RawMessage
			}
		}
		if json.Unmarshal(data, &env) != nil || env.Response.Result != "success" {
			return errors.New(s.name + ": API request failed")
		}
		data = env.Response.Data
	}
	if len(data) == 0 || string(data) == "null" || json.Unmarshal(data, out) != nil {
		return errors.New(s.name + ": invalid response")
	}
	return nil
}

// Only constructed Arr MediaCover paths enter this bounded table. No URL or
// path supplied by an upstream response or browser can turn Image into a proxy.
func (m *Media) poster(s service, id int) string {
	if id <= 0 {
		return ""
	}
	target := imagePath{s.id, fmt.Sprintf("/MediaCover/%d/poster.jpg", id)}
	if ref := m.imageRefs[target]; ref != "" {
		return ref
	}
	if len(m.imageOrder) >= maxImages {
		old := m.imageOrder[0]
		m.imageOrder = m.imageOrder[1:]
		delete(m.imageRefs, m.images[old])
		delete(m.images, old)
	}
	ref := rand.Text()
	m.images[ref] = target
	m.imageRefs[target] = ref
	m.imageOrder = append(m.imageOrder, ref)
	return ref
}
func (m *Media) Image(ctx context.Context, ref string) (string, []byte, error) {
	target, ok := m.images[ref]
	if !ok {
		return "", nil, errors.New("Unknown media image reference")
	}
	for _, s := range m.services {
		if s.id != target.service {
			continue
		}
		resp, err := m.request(ctx, s, target.path, nil)
		if err != nil {
			return "", nil, err
		}
		defer resp.Body.Close()
		ct, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(ct, "image/") {
			return "", nil, errors.New("Media image has an invalid content type")
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, plugin.MaxImageBytes+1))
		if err != nil || len(body) > plugin.MaxImageBytes {
			return "", nil, errors.New("Media image could not be read within the size limit")
		}
		return ct, body, nil
	}
	return "", nil, errors.New("Media image service is not configured")
}

// Tautulli emits counts and offsets as either JSON numbers or numeric strings.
type number float64

func (n *number) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return errors.New("invalid numeric field")
	}
	switch x := v.(type) {
	case float64:
		*n = number(x)
	case string:
		if x == "" {
			return nil
		}
		j := json.Number(x)
		f, err := j.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("invalid numeric field")
		}
		*n = number(f)
	default:
		return errors.New("invalid numeric field")
	}
	return nil
}
