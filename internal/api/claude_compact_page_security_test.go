package api

import (
	"os"
	"strings"
	"testing"
)

func TestClaudeCompactPageEscapesDynamicEventFields(t *testing.T) {
	body, err := os.ReadFile("claude_compact_page.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, want := range []string{"esc=v=>", "safe=", "safe(x.cause)", "safe(x.result)"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing XSS-safe dynamic rendering marker %q", want)
		}
	}
	if strings.Contains(page, "${x.cause}`") || strings.Contains(page, "${x.result}`") {
		t.Fatal("event fields are interpolated without escaping")
	}
	for _, forbidden := range []string{"localStorage", "?key=", "&key="} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("page persists or transmits management key unsafely: %q", forbidden)
		}
	}
	if !strings.Contains(page, "X-Management-Key") || !strings.Contains(page, "Load more") {
		t.Fatal("page is missing explicit in-memory auth or pagination UI")
	}
}
