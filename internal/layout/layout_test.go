package layout

import (
	"strings"
	"testing"
	"time"

	"github.com/sarin/gemini-notes-sync/internal/config"
)

func TestDefaults(t *testing.T) {
	tpl, err := New(config.DefaultPath, config.DefaultTitle, config.DefaultChildTitle)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 4, 20, 50, 0, 0, time.UTC)

	path, err := tpl.Path(Meeting{Date: at})
	if err != nil || strings.Join(path, "|") != "2026|09 September" {
		t.Errorf("path = %v, %v", path, err)
	}
	title, _ := tpl.Title(Meeting{Title: "Kickoff", Date: at})
	if title != "2026-09-04 — Kickoff" {
		t.Errorf("title = %q", title)
	}
	title, _ = tpl.Title(Meeting{Title: "Meeting started", Untitled: true, Date: at})
	if title != "2026-09-04 — Meeting at 20:50" {
		t.Errorf("untitled title = %q", title)
	}
	child, _ := tpl.ChildTitle(Child{Tab: "Full notes", MainTitle: "2026-09-04 — Kickoff"})
	if child != "Full notes — 2026-09-04 — Kickoff" {
		t.Errorf("child = %q", child)
	}
	long, _ := tpl.Title(Meeting{Title: strings.Repeat("a", 150), Date: at})
	if n := len([]rune(long)); n != 100 {
		t.Errorf("title not truncated to 100 runes: %d", n)
	}
}

func TestMeetCode(t *testing.T) {
	if got := MeetCode("Google Meet/qgy-dzje-onv - 2026/09/27 22:35 IST"); got != "qgy-dzje-onv" {
		t.Errorf("MeetCode = %q", got)
	}
	if got := MeetCode("Meet Recordings"); got != "" {
		t.Errorf("MeetCode = %q", got)
	}
}
