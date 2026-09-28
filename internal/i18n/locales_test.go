package i18n

import (
	"sort"
	"strings"
	"testing"
)

// flatten walks a locale catalog into dotted keys.
func flatten(prefix string, obj map[string]interface{}, out map[string]string) {
	for k, v := range obj {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if nested, ok := v.(map[string]interface{}); ok {
			flatten(key, nested, out)
			continue
		}
		if s, ok := v.(string); ok {
			out[key] = s
		}
	}
}

// placeholders extracts the {name} placeholders of a message, sorted
// (word order legitimately differs across languages; the SET of
// placeholders is the invariant).
func placeholders(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '{':
			start = i
		case s[i] == '}' && start >= 0:
			out = append(out, s[start:i+1])
			start = -1
		}
	}
	sort.Strings(out)
	return out
}

// TestLocaleKeySetParity: every supported locale carries EXACTLY the
// same key set as en, and every shared message has the same
// placeholders — a missing key would surface raw in one language only.
func TestLocaleKeySetParity(t *testing.T) {
	base := map[string]string{}
	flatten("", locales["en"], base)
	if len(base) == 0 {
		t.Fatal("en catalog is empty")
	}
	baseKeys := make([]string, 0, len(base))
	for k := range base {
		baseKeys = append(baseKeys, k)
	}
	sort.Strings(baseKeys)

	for _, lang := range SupportedLocales() {
		if lang == "en" {
			continue
		}
		got := map[string]string{}
		flatten("", locales[lang], got)
		for _, k := range baseKeys {
			v, ok := got[k]
			if !ok {
				t.Errorf("%s: missing key %q (present in en)", lang, k)
				continue
			}
			if strings.Join(placeholders(base[k]), ",") != strings.Join(placeholders(v), ",") {
				t.Errorf("%s: %s placeholders = %q, want %q", lang, k, placeholders(v), placeholders(base[k]))
			}
		}
		for k := range got {
			if _, ok := base[k]; !ok {
				t.Errorf("%s: extra key %q (absent in en)", lang, k)
			}
		}
	}
}
