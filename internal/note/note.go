// Package note is the source-independent model of a parsed meeting note.
package note

import "time"

// Sources.
const (
	Gemini = "gemini"
	Zoom   = "zoom"
)

// Tab is one section of a note that can become its own document, such as
// Gemini's "Full notes" tab or a Zoom transcript.
type Tab struct {
	Key  string // e.g. "full_notes"
	Name string // e.g. "Full notes"
	Body string // cleaned Markdown; images may remain as ![][imageN] references
}

// Image is an inline image definition carried in the source file.
type Image struct {
	MIME string
	Data []byte
}

type Doc struct {
	Tabs          []Tab
	Images        map[string]Image // "image1" -> data
	Attendees     []string
	CalendarURL   string
	EventID       string // Calendar event ID (instance), from the event link
	SeriesID      string // Calendar event ID without the recurrence suffix
	TranscriptURL string // link to the transcript in the source system

	// Set by parsers that find them in the content rather than the file name.
	Title string
	Start time.Time
}

func (d *Doc) Tab(key string) (Tab, bool) {
	for _, t := range d.Tabs {
		if t.Key == key {
			return t, true
		}
	}
	return Tab{}, false
}
