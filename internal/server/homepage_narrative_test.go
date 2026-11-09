package server

// 3.2.0 homepage narrative: the homepage is the capability story — the
// three atoms render in the fixed order Memory → Thread → Task
// (then "Publish a task"), the title area links the skill and the
// reference implementation (the protocol pages are off the public
// face), the todo_list call example sits in the start block, the
// intake board lives on /tasks (linked, not embedded), and no
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
	task := strings.Index(body, "<b>Task</b>")
	pub := strings.Index(body, "<b>Publish a task</b>")
	for name, at := range map[string]int{"Memory": mem, "Thread": thr, "Task": task, "Publish a task": pub} {
		if at < 0 {
			t.Fatalf("homepage missing the %s card", name)
		}
	}
	if !(mem < thr && thr < task && task < pub) {
		t.Fatalf("atom cards out of order: Memory@%d Thread@%d Task@%d Publish@%d", mem, thr, task, pub)
	}

	// the three short capability tags
	for _, tag := range []string{">Memory</span>", ">Thread</span>", ">Task</span>"} {
		if !strings.Contains(body, tag) {
			t.Fatalf("homepage missing capability tag %q", tag)
		}
	}

	// title area: skill + bounties + source links; the protocol page
	// is retired and must not be linked
	for _, want := range []string{
		`<a href="/skill.md"`, "Read the skill</span>",
		`href="/tasks`, "Browse tasks</span>",
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
		t.Fatal("homepage embeds the task board; it belongs on /tasks")
	}
	board := seoGet(t, s, "/tasks")
	if !strings.Contains(board, "task-board-list") && !strings.Contains(board, "task-board-empty") {
		t.Fatal("/tasks does not render the board")
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
		"<b>Memory</b>", "<b>Thread</b>", "<b>Task</b>", "<b>发布任务</b>",
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
	for _, name := range []string{"llms.txt", "skill.md", "task-guide.md", "openai.json", "sitemap.xml", "robots.txt"} {
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
