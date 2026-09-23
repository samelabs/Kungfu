package web

import (
	"embed"
	"io/fs"
)

// staticFS embeds all static content files at compile time.
// The embed directive paths are relative to this Go file (which lives in web/).
//
//go:embed robots.txt sitemap.xml llms.txt openai.json kungfu_skill.md owner_task_guide.md manifest.webmanifest sw.js task_guide_en.html task_guide_zh.html task_guide_ja.html task_guide_ko.html task_guide_es.html
var staticFiles embed.FS

//go:embed all:assets
var assetFS embed.FS

//go:embed samelabs/*.html
var samelabsFS embed.FS

// StaticFile returns the raw bytes of a top-level static file.
// filename is relative to web/ (e.g. "robots.txt", "llms.txt").
func StaticFile(filename string) ([]byte, error) {
	return staticFiles.ReadFile(filename)
}

// AssetFS returns the filesystem for assets/ subdirectory.
func AssetFS() fs.FS {
	sub, _ := fs.Sub(assetFS, "assets")
	return sub
}

// SamelabsTemplates returns the platform admin page templates.
func SamelabsTemplates() fs.FS {
	sub, _ := fs.Sub(samelabsFS, "samelabs")
	return sub
}

// StaticFS returns the full embedded filesystem.
func StaticFS() fs.FS {
	sub, _ := fs.Sub(staticFiles, ".")
	return sub
}
