package server

// WO-30 §1/§5: the homepage carries the protocol narrative — the
// three work atoms render in the fixed order Memory → Thread → Task
// (then "Publish a task"), the title area links the rendered protocol
// and the reference implementation, the todo_list call example sits in
// the start block, and no public surface carries the retired slogans
// or describes Kungfu itself as a harness/runtime/orchestration.

import (
	"strings"
	"testing"

	"kungfu.md/web"
)

func TestHomePageThreeAtomsInOrder(t *testing.T) {
	s := storeTestServer(t)
	body := seoGet(t, s, "/")

	// the three atoms as endpoint cards, in the order the protocol
	// fixes, with "Publish a task" extending Task
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

	// title area: protocol + source links
	for _, want := range []string{
		`<a href="/protocol"`, "Read the protocol</span>",
		`href="https://github.com/samelabs/Kungfu"`, "Reference implementation on GitHub</span>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("homepage title area missing %q", want)
		}
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
		"Agent 持久工作协议",
		"<b>Memory</b>", "<b>Thread</b>", "<b>Task</b>", "<b>发布任务</b>",
		"每次会话从 todo_list 开始",
		"阅读协议", "GitHub 上的参考实现",
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
// fields, not the protocol).
func TestPublicSurfacesDropRetiredNarrative(t *testing.T) {
	s := storeTestServer(t)

	surfaces := map[string]string{}
	for _, name := range []string{"llms.txt", "kungfu_skill.md", "task-guide.md", "openai.json", "sitemap.xml", "robots.txt"} {
		raw, err := web.StaticFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		surfaces[name] = string(raw)
	}
	surfaces["home_en"] = seoGet(t, s, "/")
	surfaces["home_zh"] = seoGet(t, s, "/?lang=zh")
	surfaces["protocol_en"] = seoGet(t, s, "/protocol")
	surfaces["protocol_zh"] = seoGet(t, s, "/protocol/zh-CN")

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
