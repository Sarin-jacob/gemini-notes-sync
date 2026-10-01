package syncer

import (
	"testing"

	"github.com/sarin/gemini-notes-sync/internal/outline"
)

func TestBuildIndex(t *testing.T) {
	got := buildIndex([]*outline.Node{
		{Title: "2026-09-04 — A", URL: "/doc/a"},
		{Title: "2026-09-27 — [B]", URL: "/doc/b"},
	})
	want := "## Index\n\n- [2026-09-27 — \\[B\\]](/doc/b)\n- [2026-09-04 — A](/doc/a)\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestReplaceSection(t *testing.T) {
	index := "## Index\n\n- [new](/doc/new)\n"
	tests := []struct{ name, in, want string }{
		{"append", "My notes.", "My notes.\n\n## Index\n\n- [new](/doc/new)\n"},
		{"replace middle", "Intro\n\n## Index\n\n- [old](/doc/old)\n\n## Mine\n\nKeep me.",
			"Intro\n\n## Index\n\n- [new](/doc/new)\n\n## Mine\n\nKeep me.\n"},
		{"replace end", "Intro\n\n## Index\n\n- [old](/doc/old)\n",
			"Intro\n\n## Index\n\n- [new](/doc/new)\n"},
	}
	for _, tt := range tests {
		if got := replaceSection(tt.in, indexHeading, index); got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
}
