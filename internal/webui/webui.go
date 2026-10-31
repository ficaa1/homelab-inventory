// Package webui serves the dashboard: one self-contained HTML page that reads
// the API's own endpoints.
//
// The page is embedded rather than read from disk because the API ships as a
// FROM scratch image holding a single static binary — there is no filesystem
// to put an asset directory on, and adding one would mean a second thing to
// keep in sync during deploys.
//
// It is deliberately one file with inline CSS and JS, and no external
// requests: no CDN, no font fetch, no build step. That matches the rest of the
// project (stdlib only, no dependencies) and means the dashboard works on a
// homelab that cannot reach the internet, which is exactly the situation you
// are in when something is broken enough to go looking at it.
package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

// buildTime is fixed at process start so the page carries a stable
// Last-Modified across requests, letting conditional GETs work.
var buildTime = time.Now()

// Hash the exact embedded blocks so edits cannot leave stale CSP hashes. Style
// attributes drive numeric meters/layout; scripts and style elements stay hashed.
// blob: images are plugin pictures the page fetched itself with the bearer
// token, since an <img> cannot send it.
var contentPolicy string

func assetIntegrity(body []byte) string {
	hash := sha256.Sum256(body)
	return "sha256-" + base64.StdEncoding.EncodeToString(hash[:])
}

func inlineHash(tag string) string {
	_, rest, ok := strings.Cut(string(indexHTML), "<"+tag+">")
	if !ok {
		panic("dashboard missing inline " + tag)
	}
	block, _, ok := strings.Cut(rest, "</"+tag+">")
	if !ok {
		panic("dashboard unclosed inline " + tag)
	}
	hash := sha256.Sum256([]byte(block))
	return "'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'"
}

// Handler serves the dashboard at "/" and 404s anything else that reaches it.
//
// The page itself is served without authentication, and must be: a browser
// cannot attach an Authorization header to a plain navigation, so gating the
// HTML would leave no way to ever load it. This is not a hole — the document
// contains no inventory data, only the code to ask for it. Every request it
// then makes carries the bearer token the page collects, and the API rejects
// those the same as any other client's.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentPolicy)
		switch r.URL.Path {
		case "/backup-ui.js":
			serve(w, r, "backup-ui.js", "text/javascript; charset=utf-8", backupUI)
			return
		case "/manifest.webmanifest":
			serve(w, r, "manifest.webmanifest", "application/manifest+json", manifestJSON)
			return
		case "/sw.js":
			// A service worker may only control pages at or below its own
			// path, so this has to be served from the root rather than tucked
			// away — that is a browser rule, not a choice.
			serve(w, r, "sw.js", "text/javascript; charset=utf-8", serviceWorker)
			return
		case markedPath:
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeContent(w, r, markedPath, buildTime, strings.NewReader(string(markedJS)))
			return
		case "/icon.svg":
			serve(w, r, "icon.svg", "image/svg+xml", appIcon)
			return

		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// srcdoc inherits this policy. A fresh style nonce allows preview styles
		// while the dashboard's own script and stylesheet remain hash-bound.
		nonce := rand.Text()
		w.Header().Set("Content-Security-Policy", strings.Replace(contentPolicy, "__PREVIEW_STYLE_NONCE__", nonce, 1))
		w.Header().Set("Cache-Control", "private, no-store")
		page := strings.Replace(string(indexHTML), "__PREVIEW_STYLE_NONCE__", nonce, 1)
		io.WriteString(w, page)
	})
}

//go:embed manifest.webmanifest
var manifestJSON []byte

//go:embed sw.js
var serviceWorker []byte

//go:embed icon.svg
var appIcon []byte

// marked is loaded only when a Markdown preview is opened. Its MIT notice is
// kept beside the vendored renderer; it is outside the service worker cache.
const markedPath = "/vendor/marked-18.1.0.umd.js"

//go:embed vendor/marked-18.1.0.umd.js
var markedJS []byte

// serve writes one embedded asset with a long cache lifetime.
//
// Unlike the page, these carry no token and never change between deploys of
// the same build, so they are safe to cache hard. The service worker is the
// exception the browser already handles: it revalidates it on its own schedule.
func serve(w http.ResponseWriter, r *http.Request, name, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, name, buildTime, strings.NewReader(string(body)))
}

//go:embed backup-ui.css
var backupCSS []byte

//go:embed backup-ui.js
var backupUI []byte

// Built-in plugin pages; see pages/README.md.
//
//go:embed pages
var pageFiles embed.FS

// pageAssets joins every page file with the given extension, in name order.
func pageAssets(ext string) string {
	entries, err := pageFiles.ReadDir("pages")
	if err != nil {
		panic(err)
	}
	var b strings.Builder
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ext) {
			data, err := pageFiles.ReadFile("pages/" + e.Name())
			if err != nil {
				panic(err)
			}
			b.WriteString("\n")
			b.Write(data)
		}
	}
	return b.String()
}

func init() {
	indexHTML = []byte(strings.Replace(string(indexHTML), "__MARKED_INTEGRITY__", assetIntegrity(markedJS), 1))
	indexHTML = []byte(strings.Replace(string(indexHTML), "</style>", pageAssets(".css")+"\n</style>", 1))
	indexHTML = []byte(strings.Replace(string(indexHTML), "<script>", "<script>"+pageAssets(".js")+"\n", 1))
	indexHTML = []byte(strings.Replace(string(indexHTML), "</style>", "\n"+string(backupCSS)+"</style>", 1))
	indexHTML = []byte(strings.Replace(string(indexHTML), "<!-- backup-ui -->", "", 1))
	indexHTML = []byte(strings.Replace(string(indexHTML), "</script>", "\n"+string(backupUI)+"</script>", 1))
	contentPolicy = "default-src 'none'; base-uri 'none'; object-src 'none'; form-action 'none'; frame-ancestors 'none'; connect-src 'self'; img-src 'self' data: blob:; manifest-src 'self'; worker-src 'self'; frame-src 'self'; script-src " + inlineHash("script") + " '" + assetIntegrity(markedJS) + "'; style-src " + inlineHash("style") + " 'nonce-__PREVIEW_STYLE_NONCE__'" + "; style-src-attr 'unsafe-inline'"
}
