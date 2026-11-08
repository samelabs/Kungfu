// Package kungfu embeds the one served artifact that lives outside
// web/ — the wake keeper script (examples/wake/), served verbatim so
// the downloaded keeper and the deployed server can never drift
// apart.
//
// The protocol documents (kungfu-protocol.md, kungfu-协议.md) stay in
// the repository root as internal governance texts; since 3.2.0 they
// are not embedded and not served — the public expression face is the
// skill (web/skill.md), llms.txt and the bounty guide.
package kungfu

import _ "embed"

// The wake keeper script ships in the repository (examples/wake/);
// the site serves exactly the committed bytes so the downloaded
// keeper and the deployed server can never drift apart.
//
//go:embed examples/wake/kungfu-wake.mjs
var wakeScript []byte

// WakeKeeperScript returns the committed kungfu-wake.mjs.
func WakeKeeperScript() []byte { return wakeScript }
