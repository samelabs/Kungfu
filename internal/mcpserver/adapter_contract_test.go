package mcpserver

// Adapter contract test: the wake kit's agent-facing materials (the
// official adapter's prompt and the glm executor's tool list) document
// REAL tool calls — the argument names they teach an agent must match
// the registry's input schemas, and every tool they name must exist.
// A simulated executor already knows the right shape and would never
// catch this drift (external re-audit finding: the prompt once taught
// thread_get {"code"} while the tool takes "thread").

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func adapterFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Skipf("wake kit not present: %v", err)
	}
	return string(b)
}

// TestAdapterPromptTeachesRealArgs: every `/api/v1/<tool> {"<arg>":`
// example in the adapter prompt matches the tool's required field in
// the registry schema.
func TestAdapterPromptTeachesRealArgs(t *testing.T) {
	prompt := adapterFile(t, "../../examples/wake/kungfu-agent.sh")
	schemas := map[string]map[string]any{}
	for _, def := range tools {
		var sch struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal([]byte(def.InputSchema), &sch); err != nil {
			t.Fatalf("%s: schema not JSON: %v", def.Name, err)
		}
		if len(sch.Required) > 0 {
			schemas[def.Name] = map[string]any{"first": sch.Required[0]}
		} else {
			schemas[def.Name] = map[string]any{"first": ""}
		}
	}
	re := regexp.MustCompile(`/api/v1/(\w+) \{"(\w+)"`)
	taught := re.FindAllStringSubmatch(prompt, -1)
	if len(taught) < 2 {
		t.Fatal("no tool-call examples found in the adapter prompt — the pattern moved?")
	}
	for _, m := range taught {
		tool, arg := m[1], m[2]
		want, ok := schemas[tool]
		if !ok {
			t.Errorf("prompt teaches unknown tool %q", tool)
			continue
		}
		if arg != want["first"] {
			t.Errorf("prompt teaches %s {\"%s\":...} but the tool's required field is %q", tool, arg, want["first"])
		}
	}
}

// TestGlmExecutorListsRealTools: every tool name in the glm
// executor's system prompt exists in the registry.
func TestGlmExecutorListsRealTools(t *testing.T) {
	src := adapterFile(t, "../../examples/wake/kungfu-glm-loop.py")
	names := map[string]bool{}
	for _, def := range tools {
		names[def.Name] = true
	}
	// the system prompt's tool list sits inside one quoted line of
	// the SYSTEM literal: "tools: todo_list, thread_list, ..."
	listLine := ""
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "tools: todo_list") {
			listLine = line
			break
		}
	}
	if listLine == "" {
		t.Fatal("glm executor tool list not found — the SYSTEM literal moved?")
	}
	word := regexp.MustCompile(`^[a-z_]+$`)
	for _, name := range strings.Split(listLine, ",") {
		name = strings.TrimSpace(name)
		name = strings.Trim(name, "(){}\\n\" ")
		if name == "" || !word.MatchString(name) {
			continue
		}
		if !names[name] {
			t.Errorf("glm executor lists unknown tool %q", name)
		}
	}
}
