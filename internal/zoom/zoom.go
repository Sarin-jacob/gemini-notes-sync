// Package zoom parses Zoom AI Companion meeting summaries exported as Word,
// Markdown, text or Google Docs. Zoom's export layout is not documented, so the
// parser looks for common markers ("Meeting summary for …", dates in several
// formats, an attendee line, a transcript section) and degrades gracefully to
// the file name and upload time when they are missing.
package zoom

import (
	"encoding/base64"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sarin/gemini-notes-sync/internal/mdutil"
	"github.com/sarin/gemini-notes-sync/internal/note"
)

// Tab keys as used in configuration.
const (
	Summary    = "summary"
	Transcript = "transcript"
)

// DefaultDropLines removes Zoom's AI disclaimers and feedback prompts.
var DefaultDropLines = []string{
	`(?i)ai-generated content may be inaccurate`,
	`(?i)^\W*(please )?rate (the accuracy of )?this summary`,
	`(?i)share (your )?feedback (about|on) (this|the) summary`,
}

// Meta is what the parser could tell about the meeting itself.
type Meta struct {
	Title    string
	Start    time.Time
	HasDate  bool // Start's date came from the note, not the upload time
	HasTime  bool // Start's time of day came from the note
	Untitled bool // a personal meeting room or generic "Zoom Meeting"
}

type Options struct {
	Location  *time.Location
	DayFirst  bool // read 03/04/2026 as 3 April rather than March 4
	ExtraDrop []*regexp.Regexp
}

var (
	imageDefRe     = regexp.MustCompile(`(?m)^\[(image\d+)\]:\s*<data:(image/[a-z+]+);base64,([A-Za-z0-9+/=]+)>\s*$`)
	summaryForRe   = regexp.MustCompile(`(?i)^(?:#{1,6}\s*)?[*_]*\s*(?:ai companion\s+)?meeting summary(?:\s+for|\s*:)\s*(.+?)\s*[*_]*\s*$`)
	h1Re           = regexp.MustCompile(`^#\s+(.+?)\s*$`)
	headingLevelRe = regexp.MustCompile(`^(#{1,6})\s`)
	boldLineRe     = regexp.MustCompile(`^\*\*([^*]{1,80}?)\*\*:?\s*$`)
	attendeesRe    = regexp.MustCompile(`(?i)^[*_]*(attendees|participants|invitees)[*_]*\s*:\s*[*_]*\s*(.+)$`)
	transcriptRe   = regexp.MustCompile(`(?i)^#{1,6}\s*[*_]*\s*(?:meeting\s+)?transcript\b`)
	untitledRe     = regexp.MustCompile(`(?i)^(zoom meeting|.+['’]s zoom meeting|(.+['’]s )?personal meeting room)$`)
	parenDateRe    = regexp.MustCompile(`\s*[(\[][^)\]]*\d{4}[^)\]]*[)\]]\s*$`)
	nameNoiseRe    = regexp.MustCompile(`(?i)\b(ai companion|meeting summary( for)?|summary|zoom docs?)\b`)

	month   = `(Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|June?|July?|Aug(?:ust)?|Sep(?:t(?:ember)?)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)\.?`
	isoRe   = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	slashRe = regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})/(\d{4})\b`)
	mdyRe   = regexp.MustCompile(`(?i)\b` + month + `\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
	dmyRe   = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+` + month + `,?\s+(\d{4})\b`)
	clockRe = regexp.MustCompile(`\b(\d{1,2})[:.](\d{2})(?::\d{2})?\s*([AaPp])?\.?[Mm]?\.?`)
)

// Parse cleans an exported summary. fileName and uploaded are fallbacks for
// the title and meeting time.
func Parse(md, fileName string, uploaded time.Time, opt Options) (*note.Doc, Meta) {
	if opt.Location == nil {
		opt.Location = time.Local
	}
	d := &note.Doc{Images: map[string]note.Image{}}
	md = strings.ReplaceAll(md, "\r\n", "\n")
	for _, m := range imageDefRe.FindAllStringSubmatch(md, -1) {
		if data, err := base64.StdEncoding.DecodeString(m[3]); err == nil {
			d.Images[m[1]] = note.Image{MIME: m[2], Data: data}
		}
	}
	md = imageDefRe.ReplaceAllString(md, "")

	drop := make([]*regexp.Regexp, 0, len(DefaultDropLines)+len(opt.ExtraDrop))
	for _, p := range DefaultDropLines {
		drop = append(drop, regexp.MustCompile(p))
	}
	drop = append(drop, opt.ExtraDrop...)

	var meta Meta
	var dateSources []string
	var lines []string
	seenContent, lastLevel := 0, 0
lines:
	for _, line := range strings.Split(md, "\n") {
		for _, re := range drop {
			if re.MatchString(line) {
				continue lines
			}
		}
		trimmed := strings.TrimSpace(line)
		early := seenContent < 15
		if trimmed != "" {
			seenContent++
		}
		switch {
		case early && meta.Title == "" && summaryForRe.MatchString(trimmed):
			meta.Title = summaryForRe.FindStringSubmatch(trimmed)[1]
			dateSources = append(dateSources, trimmed)
			continue
		case early && meta.Title == "" && h1Re.MatchString(trimmed):
			meta.Title = h1Re.FindStringSubmatch(trimmed)[1]
			dateSources = append(dateSources, trimmed)
			continue
		case early && attendeesRe.MatchString(trimmed) && len(d.Attendees) == 0:
			for _, a := range regexp.MustCompile(`\s*[,;]\s*`).Split(attendeesRe.FindStringSubmatch(trimmed)[2], -1) {
				if a = strings.Trim(a, " *_."); a != "" {
					d.Attendees = append(d.Attendees, a)
				}
			}
			continue
		}
		if early {
			dateSources = append(dateSources, trimmed)
		}
		if m := headingLevelRe.FindStringSubmatch(trimmed); m != nil {
			lastLevel = len(m[1])
		} else if m := boldLineRe.FindStringSubmatch(trimmed); m != nil && !strings.HasSuffix(m[1], ".") {
			// A bold line on its own is a section label, one level below the last heading.
			level := 2
			if lastLevel > 0 {
				level = min(lastLevel+1, 6)
			}
			line = strings.Repeat("#", level) + " " + m[1]
		}
		lines = append(lines, line)
	}

	meta.Title = cleanTitle(meta.Title)
	if meta.Title == "" {
		meta.Title = cleanTitle(nameNoiseRe.ReplaceAllString(strings.TrimSuffix(fileName, path.Ext(fileName)), " "))
	}
	meta.Untitled = meta.Title == "" || untitledRe.MatchString(meta.Title)
	if meta.Title == "" {
		meta.Title = "Zoom meeting"
	}

	meta.Start = uploaded.In(opt.Location)
	for _, src := range append(dateSources, fileName) {
		if t, hasTime, ok := findDate(src, opt); ok {
			meta.Start, meta.HasDate, meta.HasTime = t, true, hasTime
			break
		}
	}

	summary, transcript := lines, []string(nil)
	for i, l := range lines {
		if transcriptRe.MatchString(strings.TrimSpace(l)) {
			summary, transcript = lines[:i], lines[i+1:]
			break
		}
	}
	d.Tabs = append(d.Tabs, note.Tab{Key: Summary, Name: "Summary", Body: tidy(summary)})
	if body := tidy(transcript); body != "" {
		d.Tabs = append(d.Tabs, note.Tab{Key: Transcript, Name: "Transcript", Body: body})
	}
	d.Title, d.Start = meta.Title, meta.Start
	return d, meta
}

func tidy(lines []string) string {
	return mdutil.Tidy(strings.Join(mdutil.TrimBreaks(mdutil.ShiftHeadings(lines)), "\n"))
}

// cleanTitle strips emphasis, a trailing "(date)" and any date or separators.
func cleanTitle(s string) string {
	s = strings.Trim(s, " *_")
	s = parenDateRe.ReplaceAllString(s, "")
	for _, re := range []*regexp.Regexp{isoRe, slashRe, mdyRe, dmyRe} {
		s = re.ReplaceAllString(s, "")
	}
	s = clockRe.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return strings.Trim(s, " -–—:|_,")
}

// findDate returns the leftmost date in s, with the time of day if one follows.
func findDate(s string, opt Options) (time.Time, bool, bool) {
	best := -1
	var y, mo, day int
	end := 0
	try := func(re *regexp.Regexp, parse func(m []string) (int, int, int, bool)) {
		loc := re.FindStringSubmatchIndex(s)
		if loc == nil || (best >= 0 && loc[0] >= best) {
			return
		}
		m := make([]string, len(loc)/2)
		for i := range m {
			if loc[2*i] >= 0 {
				m[i] = s[loc[2*i]:loc[2*i+1]]
			}
		}
		if yy, mm, dd, ok := parse(m); ok {
			best, y, mo, day, end = loc[0], yy, mm, dd, loc[1]
		}
	}
	try(isoRe, func(m []string) (int, int, int, bool) { return atoi(m[1]), atoi(m[2]), atoi(m[3]), true })
	try(slashRe, func(m []string) (int, int, int, bool) {
		a, b := atoi(m[1]), atoi(m[2])
		if opt.DayFirst || a > 12 {
			a, b = b, a
		}
		return atoi(m[3]), a, b, true
	})
	try(mdyRe, func(m []string) (int, int, int, bool) { return atoi(m[3]), monthNum(m[1]), atoi(m[2]), true })
	try(dmyRe, func(m []string) (int, int, int, bool) { return atoi(m[3]), monthNum(m[2]), atoi(m[1]), true })
	if best < 0 || mo < 1 || mo > 12 || day < 1 || day > 31 {
		return time.Time{}, false, false
	}

	hour, minute, hasTime := 0, 0, false
	if m := clockRe.FindStringSubmatch(s[end:]); m != nil {
		h, mi := atoi(m[1]), atoi(m[2])
		switch strings.ToLower(m[3]) {
		case "p":
			if h < 12 {
				h += 12
			}
		case "a":
			if h == 12 {
				h = 0
			}
		}
		if h < 24 && mi < 60 {
			hour, minute, hasTime = h, mi, true
		}
	}
	return time.Date(y, time.Month(mo), day, hour, minute, 0, 0, opt.Location), hasTime, true
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func monthNum(s string) int {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	for i, m := range []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"} {
		if strings.HasPrefix(s, m) {
			return i + 1
		}
	}
	return 0
}
