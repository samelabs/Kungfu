package server

// 3.2.0 homepage narrative: the homepage is the capability story — the
// three atoms render in the fixed order Memory → Thread → Bounty
// (then "Publish a bounty"), the title area links the skill and the
// reference implementation (the protocol pages are off the public
// face), the todo_list call example sits in the start block, the
// intake board lives on /bounties (linked, not embedded), and no
// public surface carries the retired slogans or describes Kungfu
// itself as a harness/runtime/orchestration.

import (
	"strings"
	"testing"

	"kungfu.md/web"
)

func TestHomePageThreeAtomsInOrder(t *testing.T) {
	s := storeTestServer(t)
	body := seoGet(t, s, "/")

	// the three atoms as endpoint cards, in the fixed order, with
	// "Publish a bounty" extending Bounty
	mem := strings.Index(body, "<b>Memory</b>")
	thr := strings.Index(body, "<b>Thread</b>")
	bounty := strings.Index(body, "<b>Bounty</b>")
	pub := strings.Index(body, "<b>Publish a bounty</b>")
	for name, at := range map[string]int{"Memory": mem, "Thread": thr, "Bounty": bounty, "Publish a bounty": pub} {
		if at < 0 {
			t.Fatalf("homepage missing the %s card", name)
		}
	}
	if !(mem < thr && thr < bounty && bounty < pub) {
		t.Fatalf("atom cards out of order: Memory@%d Thread@%d Bounty@%d Publish@%d", mem, thr, bounty, pub)
	}

	// the three short capability tags
	for _, tag := range []string{">Memory</span>", ">Thread</span>", ">Bounty</span>"} {
		if !strings.Contains(body, tag) {
			t.Fatalf("homepage missing capability tag %q", tag)
		}
	}

	// title area: skill + bounties + source links; the protocol page
	// is retired and must not be linked
	for _, want := range []string{
		`<a href="/skill.md"`, "Read the skill</span>",
		`href="/bounties`, "Browse bounties</span>",
		`href="https://github.com/samelabs/Kungfu"`, "Reference implementation on GitHub</span>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("homepage title area missing %q", want)
		}
	}
	if strings.Contains(body, `href="/protocol"`) {
		t.Fatal("homepage still links the retired /protocol page")
	}

	// the intake board is its own page now: the homepage links it,
	// the board markup stays off the capability story
	if strings.Contains(body, "task-board-list") {
		t.Fatal("homepage embeds the bounty board; it belongs on /bounties")
	}
	board := seoGet(t, s, "/bounties")
	if !strings.Contains(board, "task-board-list") && !strings.Contains(board, "task-board-empty") {
		t.Fatal("/bounties does not render the board")
	}

	// the start block: todo_list copy plus the literal MCP call (the
	// example is HTML-escaped as a whole; assert on quote-free spans)
	for _, want := range []string{
		"Start every session with todo_list",
		`<pre class="start-example"><code>`,
		"tools/call",
		"todo_list",
		"Authorization: Bearer",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("homepage start block missing %q", want)
		}
	}

	// zh: the same structure in the Chinese copy
	zh := seoGet(t, s, "/?lang=zh")
	for _, want := range []string{
		"Agent 工作服务器",
		"<b>Memory</b>", "<b>Thread</b>", "<b>悬赏</b>", "<b>发布悬赏</b>",
		"每次会话从 todo_list 开始",
		"阅读 skill", "GitHub 上的参考实现",
	} {
		if !strings.Contains(zh, want) {
			t.Fatalf("homepage (?lang=zh) missing %q", want)
		}
	}
}

// TestPublicSurfacesDropRetiredNarrative: the retired slogans and the
// retired harness framing of Kungfu itself appear nowhere in the
// public texts or the rendered public pages (tool names work_harness
// and field names harness_refs remain legal — they name tools and
// fields, not the platform).
func TestPublicSurfacesDropRetiredNarrative(t *testing.T) {
	s := storeTestServer(t)

	surfaces := map[string]string{}
	for _, name := range []string{"llms.txt", "skill.md", "bounty-guide.md", "openai.json", "sitemap.xml", "robots.txt"} {
		raw, err := web.StaticFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		surfaces[name] = string(raw)
	}
	surfaces["home_en"] = seoGet(t, s, "/")
	surfaces["home_zh"] = seoGet(t, s, "/?lang=zh")

	banned := []string{
		"Give AI Memory", "Give AI Work.",
		"storage and tasks for AI agents",
		"给 AI 记忆，给 AI 工作",
		"为 AI agent 提供存储和任务",
		"Kungfu is a harness", "harness for AI agents",
		"orchestration",
	}
	for surface, body := range surfaces {
		for _, phrase := range banned {
			if strings.Contains(body, phrase) {
				t.Errorf("%s carries retired narrative %q", surface, phrase)
			}
		}
	}
}
