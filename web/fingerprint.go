package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"regexp"
	"sync"
)

// Asset fingerprinting.
//
// Every HTML page the server sends references assets as
// /assets/<path>?v=<first 10 hex of the file's SHA-256>. The value
// changes exactly when the file's content changes, so a deploy is
// picked up by every browser and proxy cache without any manual
// version bumping, and an unchanged file keeps its URL.

var (
	fingerprintOnce sync.Once
	fingerprints    map[string]string // "/assets/owner.css" -> "3fa9c01b2e"
	assetRef        = regexp.MustCompile(`((?:href|src)=")(/assets/[^"?#]+)(")`)
)

func loadFingerprints() {
	fingerprints = map[string]string{}
	_ = fs.WalkDir(assetFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := assetFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		fingerprints["/"+p] = hex.EncodeToString(sum[:])[:10]
		return nil
	})
}

// AssetURL returns the fingerprinted URL for an /assets/... path; paths
// that are not embedded assets are returned unchanged.
func AssetURL(path string) string {
	fingerprintOnce.Do(loadFingerprints)
	if v, ok := fingerprints[path]; ok {
		return path + "?v=" + v
	}
	return path
}

// FingerprintHTML rewrites every href="/assets/..." and src="/assets/..."
// in an HTML document to its fingerprinted URL. References that already
// carry a query string are left alone.
func FingerprintHTML(html []byte) []byte {
	return assetRef.ReplaceAllFunc(html, func(m []byte) []byte {
		parts := assetRef.FindSubmatch(m)
		out := make([]byte, 0, len(m)+14)
		out = append(out, parts[1]...)
		out = append(out, AssetURL(string(parts[2]))...)
		return append(out, parts[3]...)
	})
}
