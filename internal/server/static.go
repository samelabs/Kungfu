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
		w.Header().Set("Cache-Control", "public, max-age=300")
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

// agentHomeHandler routes agent/curl requests to llms.txt, browser requests to HTML.
// Mirrors PHP index.php $isAgentRequest logic.
func (s *Server) agentHomeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ua := strings.ToLower(r.UserAgent())
		accept := strings.ToLower(r.Header.Get("Accept"))
		isAgent := strings.Contains(ua, "curl") ||
			strings.Contains(ua, "python") ||
			strings.Contains(ua, "bot") ||
			strings.Contains(ua, "agent") ||
			strings.Contains(accept, "text/plain")

		if isAgent {
			serveStaticFile("llms.txt", "text/plain; charset=utf-8", "")(w, r)
			return
		}
		s.handleHome(w, r)
	}
}
