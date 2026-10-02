package syncer

import (
	"testing"
	"time"

	"github.com/sarin/gemini-notes-sync/internal/outline"
)

func TestBuildIndex(t *testing.T) {
	// Months: no meeting times, descending titles.
	got := buildIndex([]*outline.Node{
		{ID: "8", Title: "08 August", URL: "/doc/aug"},
		{ID: "9", Title: "09 September", URL: "/doc/sep"},
	}, nil)
	want := "## Index\n\n- [09 September](/doc/sep)\n- [08 August](/doc/aug)\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// Meetings: ordered by time regardless of title, with dates.
	times := map[string]time.Time{
		"a": time.Date(2026, 9, 4, 14, 32, 0, 0, time.UTC),
		"b": time.Date(2026, 9, 27, 22, 35, 0, 0, time.UTC),
	}
	got = buildIndex([]*outline.Node{
		{ID: "a", Title: "Zebra review", URL: "/doc/a"},
		{ID: "b", Title: "[Ad-hoc] Meeting at 22:35", URL: "/doc/b"},
	}, times)
	want = "## Index\n\n- Sun 27 Sep 2026 · [\\[Ad-hoc\\] Meeting at 22:35](/doc/b)\n- Fri 4 Sep 2026 · [Zebra review](/doc/a)\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestOutlineSafe(t *testing.T) {
	in := "## Decisions\n\n* **A** one.\n\n## Next steps\n\n- [ ] \\[Ada, Grace\\] Task: first.\n\n- [x] Second.\n\n- [ ] \\[Ada\\] Third.\n\n## Details\n\n* **B** kept.\n\n- [ ] Alone."
	want := "## Decisions\n\n* **A** one.\n\n## Next steps\n\n- [ ] [Ada, Grace] Task: first.\n* [x] Second.\n- [ ] [Ada] Third.\n\n## Details\n\n* **B** kept.\n\n- [ ] Alone."
	if got := outlineSafe(in); got != want {
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
