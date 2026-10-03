// Package mdutil holds small Markdown clean-up helpers shared by the parsers.
package mdutil

import (
	"regexp"
	"strings"
)

var (
	blockStartRe = regexp.MustCompile(`^\s*([*+-] |\d+[.)] |#|>|---)`)
	blankRunsRe  = regexp.MustCompile(`\n{3,}`)
	headingRe    = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*$`)
)

// TrimBreaks drops hard line breaks (two trailing spaces) that don't join two
// lines of one paragraph.
func TrimBreaks(lines []string) []string {
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

// Tidy collapses runs of blank lines and trims the result.
func Tidy(s string) string {
	return strings.TrimSpace(blankRunsRe.ReplaceAllString(s, "\n\n"))
}

// ShiftHeadings re-levels headings so the shallowest becomes H2 (the document
// title is H1) and strips emphasis wrapped around whole headings.
func ShiftHeadings(lines []string) []string {
	minLevel := 7
	for _, l := range lines {
		if m := headingRe.FindStringSubmatch(l); m != nil {
			minLevel = min(minLevel, len(m[1]))
		}
	}
	for i, l := range lines {
		m := headingRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		level := min(max(len(m[1])-(minLevel-2), 2), 6)
		text := strings.TrimSpace(strings.Trim(m[2], "*_"))
		lines[i] = strings.Repeat("#", level) + " " + text
	}
	return lines
}
