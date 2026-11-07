// Package kungfu embeds the normative protocol documents that live at
// the repository root (kungfu-protocol.md, kungfu-协议.md). A Go embed
// pattern cannot reach outside its package directory, so the embed
// lives here, at the root, next to the documents themselves — the site
// serves exactly the committed text, frozen at compile time.
//
// The kungfu.md name belongs to the agent skill (web/kungfu.md,
// served at https://kungfu.md/kungfu.md); the protocol is the reference
// specification behind it.
package kungfu

import "embed"

//go:embed kungfu-protocol.md kungfu-协议.md
var files embed.FS

// The wake keeper script ships in the repository (examples/wake/);
// the site serves exactly the committed bytes so the downloaded
// keeper and the deployed server can never drift apart.
//
//go:embed examples/wake/kungfu-wake.mjs
var wakeScript []byte

// WakeKeeperScript returns the committed kungfu-wake.mjs.
func WakeKeeperScript() []byte { return wakeScript }

// ProtocolEnglish returns the normative English spec
// (kungfu-protocol.md).
func ProtocolEnglish() ([]byte, error) { return files.ReadFile("kungfu-protocol.md") }

// ProtocolChinese returns the informative Chinese translation
// (kungfu-协议.md).
func ProtocolChinese() ([]byte, error) { return files.ReadFile("kungfu-协议.md") }
