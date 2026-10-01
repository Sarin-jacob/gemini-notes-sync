// Package layout renders the user's path and title templates for a meeting.
package layout

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/sarin/gemini-notes-sync/internal/outline"
)

// Meeting is the data available to all templates.
type Meeting struct {
	Title     string    // meeting title from the file name ("Meeting started" for ad-hoc calls)
	RawTitle  string    // Drive file name
	Untitled  bool      // ad-hoc meeting without a calendar title
	Date      time.Time // meeting start
	Attendees []string
	MeetCode  string // e.g. "abc-defg-hij", when the note sits in a per-meeting folder
	EventID   string // Google Calendar event instance ID
	SeriesID  string // Google Calendar recurring series ID (== EventID for one-off events)
	DriveID   string
	DriveURL  string
	Folder    string // Drive folder path of the note
}

// Child is the data for nested tab documents.
type Child struct {
	Meeting
	Tab       string // "Full notes", "Transcript"
	MainTitle string // rendered title of the meeting document
}

type Templates struct {
	path, title, child *template.Template
}

var funcs = template.FuncMap{
	"date":  func(layout string, t time.Time) string { return t.Format(layout) },
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"trim":  strings.TrimSpace,
	"trunc": func(n int, s string) string {
		if r := []rune(s); len(r) > n {
			return string(r[:n])
		}
		return s
	},
	"default": func(def, v string) string {
		if strings.TrimSpace(v) == "" {
			return def
		}
		return v
	},
	"replace": func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
	"regexReplace": func(pattern, repl, s string) (string, error) {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return "", err
		}
		return re.ReplaceAllString(s, repl), nil
	},
	"join": func(sep string, xs []string) string { return strings.Join(xs, sep) },
}

func New(path, title, child string) (*Templates, error) {
	var t Templates
	var err error
	if t.path, err = template.New("path").Funcs(funcs).Option("missingkey=error").Parse(path); err != nil {
		return nil, fmt.Errorf("layout.path: %w", err)
	}
	if t.title, err = template.New("title").Funcs(funcs).Option("missingkey=error").Parse(title); err != nil {
		return nil, fmt.Errorf("layout.title: %w", err)
	}
	if t.child, err = template.New("child_title").Funcs(funcs).Option("missingkey=error").Parse(child); err != nil {
		return nil, fmt.Errorf("layout.child_title: %w", err)
	}
	return &t, nil
}

// Path returns the container document titles from the collection root down.
func (t *Templates) Path(m Meeting) ([]string, error) {
	s, err := exec(t.path, m)
	if err != nil {
		return nil, fmt.Errorf("layout.path: %w", err)
	}
	var segs []string
	for _, seg := range strings.Split(s, "/") {
		if seg = strings.TrimSpace(seg); seg != "" {
			segs = append(segs, outline.Truncate(seg))
		}
	}
	return segs, nil
}

func (t *Templates) Title(m Meeting) (string, error) {
	s, err := exec(t.title, m)
	if err != nil {
		return "", fmt.Errorf("layout.title: %w", err)
	}
	return outline.Truncate(oneLine(s)), nil
}

func (t *Templates) ChildTitle(c Child) (string, error) {
	s, err := exec(t.child, c)
	if err != nil {
		return "", fmt.Errorf("layout.child_title: %w", err)
	}
	return outline.Truncate(oneLine(s)), nil
}

func exec(t *template.Template, data any) (string, error) {
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

var meetCodeRe = regexp.MustCompile(`\b([a-z]{3}-[a-z]{4}-[a-z]{3})\b`)

// MeetCode extracts a Meet code such as "abc-defg-hij" from a folder path.
func MeetCode(folder string) string {
	if m := meetCodeRe.FindStringSubmatch(folder); m != nil {
		return m[1]
	}
	return ""
}
