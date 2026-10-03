package zoom

import (
	"strings"
	"testing"
	"time"
)

var ist = time.FixedZone("IST", 5*3600+1800)

func TestParseEmailStyle(t *testing.T) {
	md := `# Meeting summary for Weekly Product Sync (10/02/2026)

**Attendees:** Ada Lovelace, Grace Hopper

**Quick recap**

The team reviewed the roadmap and agreed on Q4 priorities.

## Next steps

- Ada will draft the spec.
- Grace will review the budget.

## Summary

### Roadmap review

Discussion of milestones.

AI-generated content may be inaccurate or misleading. Always check for accuracy.

## Transcript

**Ada:** Hello everyone.
`
	d, meta := Parse(md, "anything.docx", time.Date(2026, 10, 5, 9, 0, 0, 0, ist), Options{Location: ist})

	if meta.Title != "Weekly Product Sync" || meta.Untitled {
		t.Errorf("title = %q untitled = %v", meta.Title, meta.Untitled)
	}
	if got := meta.Start.Format("2006-01-02"); got != "2026-10-02" || !meta.HasDate || meta.HasTime {
		t.Errorf("start = %s hasDate=%v hasTime=%v", got, meta.HasDate, meta.HasTime)
	}
	if strings.Join(d.Attendees, "|") != "Ada Lovelace|Grace Hopper" {
		t.Errorf("attendees = %v", d.Attendees)
	}
	summary, _ := d.Tab(Summary)
	want := `## Quick recap

The team reviewed the roadmap and agreed on Q4 priorities.

## Next steps

- Ada will draft the spec.
- Grace will review the budget.

## Summary

### Roadmap review

Discussion of milestones.`
	if summary.Body != want {
		t.Errorf("summary:\n%s\n--- want ---\n%s", summary.Body, want)
	}
	tr, ok := d.Tab(Transcript)
	if !ok || tr.Body != "**Ada:** Hello everyone." {
		t.Errorf("transcript = %q, %v", tr.Body, ok)
	}
}

func TestParseFallbacks(t *testing.T) {
	uploaded := time.Date(2026, 10, 5, 9, 30, 0, 0, ist)
	d, meta := Parse("Some notes without a heading.\n", "Sarin's Zoom Meeting - 2026-10-03.docx", uploaded, Options{Location: ist})
	if !meta.Untitled || meta.Title != "Sarin's Zoom Meeting" {
		t.Errorf("title = %q untitled = %v", meta.Title, meta.Untitled)
	}
	if got := meta.Start.Format("2006-01-02"); got != "2026-10-03" {
		t.Errorf("start = %s", got)
	}
	if s, _ := d.Tab(Summary); s.Body != "Some notes without a heading." {
		t.Errorf("summary = %q", s.Body)
	}

	_, meta = Parse("Notes.\n", "notes.md", uploaded, Options{Location: ist})
	if meta.HasDate || !meta.Start.Equal(uploaded) || meta.Title != "notes" {
		t.Errorf("fallback: title %q start %v hasDate %v", meta.Title, meta.Start, meta.HasDate)
	}
}

func TestFindDate(t *testing.T) {
	tests := []struct {
		in       string
		dayFirst bool
		want     string
		hasTime  bool
	}{
		{"Oct 2, 2026 10:30 AM", false, "2026-10-02 10:30", true},
		{"2 October 2026, 3:05 PM IST", false, "2026-10-02 15:05", true},
		{"10/02/2026", false, "2026-10-02 00:00", false},
		{"10/02/2026", true, "2026-02-10 00:00", false},
		{"25/12/2026 18:00", false, "2026-12-25 18:00", true},
		{"Meeting on 2026-09-30 at 12:15 pm", false, "2026-09-30 12:15", true},
	}
	for _, tt := range tests {
		got, hasTime, ok := findDate(tt.in, Options{Location: ist, DayFirst: tt.dayFirst})
		if !ok || got.Format("2006-01-02 15:04") != tt.want || hasTime != tt.hasTime {
			t.Errorf("findDate(%q) = %s, %v, %v", tt.in, got.Format("2006-01-02 15:04"), hasTime, ok)
		}
	}
}
