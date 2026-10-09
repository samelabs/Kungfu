// Package kungfu embeds the normative protocol documents that live at
// the repository root (kungfu.md, kungfu.zh-CN.md). A Go embed pattern
// cannot reach outside its package directory, so the embed lives here,
// at the root, next to the documents themselves — the site serves
// exactly the committed text, frozen at compile time.
package kungfu

import "embed"

//go:embed kungfu.md kungfu.zh-CN.md
var files embed.FS

// ProtocolEnglish returns the normative English spec (kungfu.md).
func ProtocolEnglish() ([]byte, error) { return files.ReadFile("kungfu.md") }

// ProtocolChinese returns the informative Chinese translation
// (kungfu.zh-CN.md).
func ProtocolChinese() ([]byte, error) { return files.ReadFile("kungfu.zh-CN.md") }
