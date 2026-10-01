package gemini

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseName(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	tests := []struct {
		name, title string
		untitled    bool
		at          string
	}{
		{"Tata Hitachi - Documentation - 2026/09/04 20:50 IST - Notes by Gemini", "Tata Hitachi - Documentation", false, "2026-09-04 20:50"},
		{"Meeting started 2026/09/27 22:35 IST - Notes by Gemini", "Meeting started", true, "2026-09-27 22:35"},
		{"Weekly sync - 2026/01/02 09:00 GMT+1 - Notes by Gemini", "Weekly sync", false, "2026-01-02 09:00"},
	}
	for _, tt := range tests {
		title, start, untitled, ok := ParseName(tt.name, ist)
		if !ok || title != tt.title || untitled != tt.untitled || start.Format("2006-01-02 15:04") != tt.at {
			t.Errorf("ParseName(%q) = %q, %v, %v, %v", tt.name, title, start, untitled, ok)
		}
	}
	if _, _, _, ok := ParseName("Random doc", ist); ok {
		t.Error("ParseName accepted a non-Gemini name")
	}
}

func TestParse(t *testing.T) {
	raw, err := os.ReadFile("testdata/note.md")
	if err != nil {
		t.Fatal(err)
	}
	d := Parse(string(raw), nil)

	var keys []string
	for _, tab := range d.Tabs {
		keys = append(keys, tab.Key)
	}
	if got := strings.Join(keys, ","); got != "quick_notes,full_notes,transcript" {
		t.Fatalf("tabs = %s", got)
	}
	if got := strings.Join(d.Attendees, ","); got != "ada@example.com,Grace Hopper" {
		t.Errorf("attendees = %s", got)
	}
	if d.SeriesID != "series123" || d.EventID != "series123_20260904T152000Z" {
		t.Errorf("event = %q series = %q", d.EventID, d.SeriesID)
	}
	if !strings.Contains(d.TranscriptURL, "docs.google.com/document/d/abc123") {
		t.Errorf("transcript url = %q", d.TranscriptURL)
	}
	if _, ok := d.Images["image1"]; !ok {
		t.Error("image1 definition not captured")
	}

	quick, _ := d.Tab(QuickNotes)
	wantQuick := `Kickoff covering scope and timeline.

## Scope

* Phase 1 is a data ingestion portal.
* See the [spec](https://example.com/spec).

## Next steps

- [ ] \[Grace Hopper\] Draft schema: Prepare the first database schema.`
	if quick.Body != wantQuick {
		t.Errorf("quick notes:\n%s\n--- want ---\n%s", quick.Body, wantQuick)
	}

	full, _ := d.Tab(FullNotes)
	wantFull := `## Summary

Scope agreed ([00:01:48](#00:01:48)).

![][image1]

## Decisions

### Aligned

* **Database** PostgreSQL.

## Next steps

- [ ] \[Grace Hopper\] Draft schema.`
	if full.Body != wantFull {
		t.Errorf("full notes:\n%s\n--- want ---\n%s", full.Body, wantFull)
	}

	tr, _ := d.Tab(Transcript)
	for _, bad := range []string{"{#", "**00:", "computer generated", "Transcript**"} {
		if strings.Contains(tr.Body, bad) {
			t.Errorf("transcript still contains %q:\n%s", bad, tr.Body)
		}
	}
	if !strings.HasPrefix(tr.Body, "## 00:00:00") {
		t.Errorf("transcript should start with H2 timestamp:\n%s", tr.Body)
	}
	if strings.Contains(string(raw), "data:image") && strings.Contains(full.Body+quick.Body+tr.Body, "data:image") {
		t.Error("image data left in body")
	}
}
