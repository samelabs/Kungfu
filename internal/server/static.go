package server

import (
	"io/fs"
	"net/http"
	"strings"

	"kungfu.md/web"
)

// assetMimeTypes maps file extensions to MIME types.
var assetMimeTypes = map[string]string{
	"css":         "text/css; charset=utf-8",
	"js":          "application/javascript; charset=utf-8",
	"svg":         "image/svg+xml",
	"png":         "image/png",
	"jpg":         "image/jpeg",
	"jpeg":        "image/jpeg",
	"webp":        "image/webp",
	"gif":         "image/gif",
	"json":        "application/json; charset=utf-8",
	"txt":         "text/plain; charset=utf-8",
	"md":          "text/markdown; charset=utf-8",
	"xml":         "application/xml; charset=utf-8",
	"webmanifest": "application/manifest+json; charset=utf-8",
}

// serveStaticFile returns a handler that serves a single embedded file.
func serveStaticFile(filename, contentType, cacheControl string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := web.StaticFile(filename)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}
}

// serveAssets handles GET /assets/* requests.
func serveAssets() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/assets/")
		if path == "" {
			http.NotFound(w, r)
			return
		}

		assetFS := web.AssetFS()
		data, err := fs.ReadFile(assetFS, path)
		if err != nil {
			ErrorResponse(w, 404, "NOT_FOUND", "Asset not found", nil)
			return
		}

		ext := strings.ToLower(filepathExt(path))
		if mime, ok := assetMimeTypes[ext]; ok {
			w.Header().Set("Content-Type", mime)
		}
		// Code assets (JS/CSS) must always revalidate: a stale cached
		// script must never survive a deployment. Images/fonts are
		// content-stable and keep a fresh cache window.
		if ext == "js" || ext == "css" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}
}

// filepathExt returns the file extension (without dot), lowercased.
func filepathExt(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return ""
	}
	return path[idx+1:]
}

// agentHomeHandler routes explicit agent/CLI requests to llms.txt, browser
// requests (including search crawlers like Googlebot/Bingbot, which must see
// the canonical human homepage) to HTML.
//
// The check is an allowlist of known agent/CLI client signatures, NOT a
// generic "bot" substring match: a generic match would hijack Googlebot and
// Bingbot into the agent discovery text and damage search indexing.
// Search-engine crawlers are browsers for our purposes; agents that want the
// discovery surface can always fetch /llms.txt explicitly.
func (s *Server) agentHomeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ua := strings.ToLower(r.UserAgent())
		accept := strings.ToLower(r.Header.Get("Accept"))
		isAgent := strings.Contains(ua, "curl") ||
			strings.Contains(ua, "wget") ||
			strings.Contains(ua, "python-requests") ||
			strings.Contains(ua, "python-urllib") ||
			strings.Contains(ua, "go-http-client") ||
			strings.Contains(ua, "node-fetch") ||
			strings.Contains(ua, "httpie") ||
			strings.Contains(ua, "python-httpx") ||
			strings.Contains(ua, "aiohttp") ||
			strings.Contains(ua, "axios") ||
			strings.Contains(ua, "undici") ||
			strings.Contains(ua, "deno/") ||
			ua == "node" || // Node.js built-in fetch
			strings.Contains(accept, "text/plain")

		// The body at "/" depends on these headers: shared caches must
		// key on them or they would serve llms.txt to browsers (or HTML
		// to agents).
		w.Header().Add("Vary", "User-Agent")
		w.Header().Add("Vary", "Accept")
		if isAgent {
			serveStaticFile("llms.txt", "text/plain; charset=utf-8", "")(w, r)
			return
		}
		s.handleHome(w, r)
	}
}
