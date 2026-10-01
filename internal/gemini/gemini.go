// Package gemini parses the Markdown export of a "Notes by Gemini" Google Doc:
// it splits the export into tabs, lifts the header metadata (attendees, calendar
// event, transcript link) out of the body and strips Gemini's boilerplate.
package gemini

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Tab keys as used in configuration.
const (
	QuickNotes = "quick_notes"
	FullNotes  = "full_notes"
	Transcript = "transcript"
)

type Tab struct {
	Key  string // e.g. "full_notes"
	Name string // e.g. "Full notes"
	Body string // cleaned Markdown; images remain as ![][imageN] references
}

// Image is an inline image definition from the export (downscaled; used as fallback).
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
	TranscriptURL string // link to the transcript tab in Google Docs
}

func (d *Doc) Tab(key string) (Tab, bool) {
	for _, t := range d.Tabs {
		if t.Key == key {
			return t, true
		}
	}
	return Tab{}, false
}

// DefaultDropLines removes Gemini's survey prompts and disclaimers.
var DefaultDropLines = []string{
	`(?i)rate the new quick notes tab`,
	`(?i)you should review gemini's notes`,
	`(?i)how is the quality of \**these specific notes`,
	`(?i)this editable transcript was computer generated`,
	`google\.qualtrics\.com`,
}

var (
	nameRe      = regexp.MustCompile(`^(.*?)\s*-?\s*(\d{4}/\d{2}/\d{2} \d{2}:\d{2})(?:\s+([A-Z][A-Za-z+\-0-9]{1,6}))?\s*-\s*Notes by Gemini\s*$`)
	imageDefRe  = regexp.MustCompile(`(?m)^\[(image\d+)\]:\s*<data:(image/[a-z+]+);base64,([A-Za-z0-9+/=]+)>\s*$`)
	headingRe   = regexp.MustCompile(`^(#{1,6})(?:\s+(.*?))?\s*$`)
	anchorRe    = regexp.MustCompile(`\s*\{#[^}]*\}\s*$`)
	boldWrapRe  = regexp.MustCompile(`^\*\*(.*?)\*\*$`)
	mailtoRe    = regexp.MustCompile(`\[([^\]]+)\]\(mailto:[^)]*\)`)
	dateLineRe  = regexp.MustCompile(`^(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]* \d{1,2}, \d{4}\s*$`)
	calendarRe  = regexp.MustCompile(`https://calendar\.google\.com/calendar/event\?eid=([A-Za-z0-9_\-]+)`)
	docLinkRe   = regexp.MustCompile(`\((https://docs\.google\.com/document/[^)\s]+)\)`)
	redirectRe  = regexp.MustCompile(`https?://(?:www\.)?google\.com/url\?q=([^&)\s]+)[^)\s]*`)
	blankRunsRe = regexp.MustCompile(`\n{3,}`)
	speakerRe   = regexp.MustCompile(`(?m)^\*\*([^*:\n]{1,60}):\*\*`)
)

// ParseName extracts the meeting title and start time from a Drive file name
// such as "Tata Hitachi - Documentation - 2026/09/04 20:50 IST - Notes by Gemini".
// Ad-hoc meetings are named "Meeting started <time>"; untitled reports those.
func ParseName(name string, loc *time.Location) (title string, start time.Time, untitled, ok bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return strings.TrimSpace(strings.TrimSuffix(name, "- Notes by Gemini")), time.Time{}, false, false
	}
	t, err := time.ParseInLocation("2006/01/02 15:04", m[2], loc)
	if err != nil {
		return m[1], time.Time{}, false, false
	}
	title = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(m[1]), "-"))
	untitled = title == "" || strings.EqualFold(title, "Meeting started")
	return title, t, untitled, true
}

// Parse splits and cleans an exported note. extraDrop are additional line regexes.
func Parse(markdown string, extraDrop []*regexp.Regexp) *Doc {
	d := &Doc{Images: map[string]Image{}}
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")

	for _, m := range imageDefRe.FindAllStringSubmatch(markdown, -1) {
		if data, err := base64.StdEncoding.DecodeString(m[3]); err == nil {
			d.Images[m[1]] = Image{MIME: m[2], Data: data}
		}
	}
	markdown = imageDefRe.ReplaceAllString(markdown, "")

	drop := make([]*regexp.Regexp, 0, len(DefaultDropLines)+len(extraDrop))
	for _, p := range DefaultDropLines {
		drop = append(drop, regexp.MustCompile(p))
	}
	drop = append(drop, extraDrop...)

	for i, raw := range splitTabs(markdown) {
		body := d.cleanTab(raw.lines, drop)
		if i == 0 && raw.preamble && body == "" {
			continue // only boilerplate before the first tab heading
		}
		d.Tabs = append(d.Tabs, Tab{Key: tabKey(raw.name), Name: raw.name, Body: body})
	}
	if len(d.Attendees) == 0 {
		// Ad-hoc meetings have no invite list; fall back to transcript speakers.
		if t, ok := d.Tab(Transcript); ok {
			seen := map[string]bool{}
			for _, m := range speakerRe.FindAllStringSubmatch(t.Body, -1) {
				if name := strings.TrimSpace(m[1]); !seen[name] {
					seen[name] = true
					d.Attendees = append(d.Attendees, name)
				}
			}
		}
	}
	return d
}

type rawTab struct {
	name     string
	lines    []string
	preamble bool // content before the first H1
}

// splitTabs cuts the export at its level-1 headings, one per Docs tab.
// Content before the first H1 (older single-tab notes) becomes a "Notes" tab.
func splitTabs(md string) []rawTab {
	var tabs []rawTab
	cur := rawTab{name: "Notes", preamble: true}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "# ") {
			if !cur.preamble || strings.TrimSpace(strings.Join(cur.lines, "")) != "" {
				tabs = append(tabs, cur)
			}
			cur = rawTab{name: tabName(line[2:])}
			continue
		}
		cur.lines = append(cur.lines, line)
	}
	return append(tabs, cur)
}

// tabName turns "**✍️ Quick notes**" into "Quick notes".
func tabName(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.TrimFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	return s
}

func tabKey(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "_")
}

func (d *Doc) cleanTab(lines []string, drop []*regexp.Regexp) string {
	var out []string
	titleDropped := false
	prevOrig, prevNew := 0, 0 // previous heading level, before and after re-leveling
	type heading struct{ idx, level int }
	var headings []heading

lines:
	for _, line := range lines {
		for _, re := range drop {
			if re.MatchString(line) {
				continue lines
			}
		}
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "Invited "):
			d.addAttendees(trimmed)
			continue
		case strings.HasPrefix(trimmed, "Attachments "):
			d.setCalendar(trimmed)
			continue
		case strings.HasPrefix(trimmed, "Meeting records "):
			if m := docLinkRe.FindStringSubmatch(trimmed); m != nil && d.TranscriptURL == "" {
				d.TranscriptURL = m[1]
			}
			continue
		case dateLineRe.MatchString(trimmed):
			continue
		case trimmed != "" && strings.TrimSpace(mailtoRe.ReplaceAllString(trimmed, "")) == "":
			// A line made only of attendee mailto links (Quick notes header).
			d.addAttendees(trimmed)
			continue
		}

		if m := headingRe.FindStringSubmatch(line); m != nil {
			text := anchorRe.ReplaceAllString(m[2], "")
			bold := false
			if b := boldWrapRe.FindStringSubmatch(text); b != nil {
				text, bold = strings.TrimSpace(b[1]), true
			}
			if strings.Trim(text, "* ") == "" {
				continue
			}
			if !titleDropped {
				// The tab's first heading repeats the meeting title.
				titleDropped = true
				continue
			}
			level := len(m[1])
			newLevel := level
			if !bold && prevNew > 0 && level <= prevOrig {
				// Unbolded labels such as "## Aligned" under "### Decisions" are sub-headings.
				newLevel = prevNew + 1
			} else {
				prevOrig, prevNew = level, level
			}
			headings = append(headings, heading{len(out), newLevel})
			out = append(out, text) // prefix added after normalising levels
			continue
		}

		line = redirectRe.ReplaceAllStringFunc(line, func(u string) string {
			if q, err := url.QueryUnescape(redirectRe.FindStringSubmatch(u)[1]); err == nil {
				return q
			}
			return u
		})
		out = append(out, strings.TrimRight(line, " \t")+trailingBreak(line))
	}

	// Shift heading levels so the shallowest becomes H2 (H1 is the document title).
	minLevel := 6
	for _, h := range headings {
		minLevel = min(minLevel, h.level)
	}
	for _, h := range headings {
		level := min(max(h.level-(minLevel-2), 2), 6)
		out[h.idx] = strings.Repeat("#", level) + " " + out[h.idx]
	}

	body := strings.Join(trimBreaks(out), "\n")
	body = blankRunsRe.ReplaceAllString(body, "\n\n")
	return strings.TrimSpace(body)
}

var blockStartRe = regexp.MustCompile(`^\s*([*+-] |\d+[.)] |#|>|---)`)

// trimBreaks drops hard line breaks that don't join two lines of one paragraph;
// Gemini ends most bullets with one.
func trimBreaks(lines []string) []string {
	for i, l := range lines {
		if !strings.HasSuffix(l, "  ") {
			continue
		}
		next := ""
		if i+1 < len(lines) {
			next = lines[i+1]
		}
		if strings.TrimSpace(next) == "" || blockStartRe.MatchString(next) {
			lines[i] = strings.TrimRight(l, " ")
		}
	}
	return lines
}

// trailingBreak preserves Markdown hard line breaks (two trailing spaces).
func trailingBreak(line string) string {
	if strings.HasSuffix(line, "  ") && strings.TrimSpace(line) != "" {
		return "  "
	}
	return ""
}

func (d *Doc) addAttendees(line string) {
	seen := map[string]bool{}
	for _, a := range d.Attendees {
		seen[a] = true
	}
	for _, m := range mailtoRe.FindAllStringSubmatch(line, -1) {
		if !seen[m[1]] {
			d.Attendees = append(d.Attendees, m[1])
			seen[m[1]] = true
		}
	}
}

func (d *Doc) setCalendar(line string) {
	m := calendarRe.FindStringSubmatch(line)
	if m == nil || d.CalendarURL != "" {
		return
	}
	d.CalendarURL = m[0]
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(m[1], "="))
	if err != nil {
		return
	}
	// The eid decodes to "<eventId> <calendarId>"; recurring instances are "<seriesId>_<start>".
	id, _, _ := strings.Cut(string(raw), " ")
	d.EventID = id
	d.SeriesID, _, _ = strings.Cut(id, "_")
}
