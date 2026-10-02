package syncer

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sarin/gemini-notes-sync/internal/gemini"
	"github.com/sarin/gemini-notes-sync/internal/outline"
	"github.com/sarin/gemini-notes-sync/internal/store"
)

// timestampLinkRe matches Gemini's links into the transcript, e.g. [00:01:48](#00:01:48).
var timestampLinkRe = regexp.MustCompile(`\]\(#(\d{1,2}:\d{2}:\d{2})\)`)

// render produces the Markdown body for one of a note's documents.
func (s *Syncer) render(p *Plan, role string, refs map[string]store.DocRef, images map[string]string) string {
	tabKey := role
	if role == roleMain {
		tabKey = p.Main
	}
	tab, _ := p.Doc.Tab(tabKey)

	body := imageRefRe.ReplaceAllStringFunc(tab.Body, func(m string) string {
		sm := imageRefRe.FindStringSubmatch(m)
		if url, ok := images[sm[2]]; ok {
			return fmt.Sprintf("![%s](%s)", sm[1], url)
		}
		return ""
	})

	// Point transcript timestamps at the transcript document (or the Google Doc).
	transcript := p.Doc.TranscriptURL
	if ref, ok := refs[gemini.Transcript]; ok {
		transcript = ref.URL
	}
	if transcript == "" {
		transcript = p.File.Link
	}
	body = timestampLinkRe.ReplaceAllString(body, "]("+transcript+")")

	var header []string
	if role == roleMain {
		m := p.Meeting
		header = append(header, "**When:** "+m.Date.Format("Mon, 2 Jan 2006 · 15:04 MST"))
		if len(m.Attendees) > 0 {
			header = append(header, "**Attendees:** "+strings.Join(m.Attendees, ", "))
		}
		var links []string
		for _, key := range p.Order {
			if ref, ok := refs[key]; ok {
				t, _ := p.Doc.Tab(key)
				links = append(links, fmt.Sprintf("[%s](%s)", t.Name, ref.URL))
			}
		}
		links = append(links, fmt.Sprintf("[Google Doc](%s)", p.File.Link))
		if p.Doc.CalendarURL != "" {
			links = append(links, fmt.Sprintf("[Calendar event](%s)", p.Doc.CalendarURL))
		}
		header = append(header, "**Links:** "+strings.Join(links, " · "))
	} else {
		header = append(header, fmt.Sprintf("**Meeting:** [%s](%s) · [Google Doc](%s)", p.Title, refs[roleMain].URL, p.File.Link))
	}
	// Separate lines with hard breaks so the header renders as one compact block.
	return strings.Join(header, "  \n") + "\n\n---\n\n" + outlineSafe(body) + "\n"
}

var (
	checkItemRe     = regexp.MustCompile(`^[-*+] (\[[ xX]\] .*)$`)
	escapedAssignee = regexp.MustCompile(`^(\[[ xX]\] )\\\[([^\]]*?)\\\]`)
)

// outlineSafe works around two bugs in Outline's Markdown import (seen in 1.10.1):
//   - a checklist with two or more items that follows any bullet list turns that
//     earlier list into a checklist and drops the list after it; one-item
//     checklists are fine, so consecutive items get alternating bullet markers,
//     which makes each its own list (they still render as one checklist);
//   - an escaped "\[" at the start of a checklist item becomes "undefined[".
func outlineSafe(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	markers := [2]string{"- ", "* "}
	n := 0 // position within the current run of checklist items
	for i := 0; i < len(lines); i++ {
		m := checkItemRe.FindStringSubmatch(lines[i])
		if m == nil {
			// A blank line between two checklist items does not end the run.
			if strings.TrimSpace(lines[i]) == "" && n > 0 && i+1 < len(lines) && checkItemRe.MatchString(lines[i+1]) {
				continue
			}
			n = 0
			out = append(out, lines[i])
			continue
		}
		item := escapedAssignee.ReplaceAllString(m[1], "${1}[$2]")
		out = append(out, markers[n%2]+item)
		n++
	}
	return strings.Join(out, "\n")
}

const indexHeading = "## Index"

// rebuildIndexes rewrites the auto-generated index of each container document.
// A container still holding only our index (or nothing) is replaced wholesale;
// if the user added their own text, only the "## Index" section is replaced.
func (s *Syncer) rebuildIndexes(ctx context.Context, ids map[string]bool) error {
	tree, err := s.ol.Tree(ctx, s.collection.ID)
	if err != nil {
		return err
	}
	nodes := map[string]*outline.Node{}
	var walk func([]*outline.Node)
	walk = func(ns []*outline.Node) {
		for _, n := range ns {
			nodes[n.ID] = n
			walk(n.Children)
		}
	}
	walk(tree)
	times, err := s.st.MeetingTimes()
	if err != nil {
		return err
	}

	for id := range ids {
		node, ok := nodes[id]
		if !ok {
			continue
		}
		index := buildIndex(node.Children, times)
		cur, err := s.ol.Info(ctx, id)
		if err != nil {
			return err
		}
		prev, err := s.st.IndexHash(id)
		if err != nil {
			return err
		}
		text := index
		if strings.TrimSpace(cur.Text) != "" && hashDoc(cur) != prev {
			text = replaceSection(cur.Text, indexHeading, index)
		}
		if strings.TrimSpace(text) == strings.TrimSpace(cur.Text) {
			continue
		}
		d, err := s.ol.Update(ctx, id, cur.Title, text)
		if err != nil {
			return err
		}
		if err := s.st.PutIndexHash(id, hashDoc(d)); err != nil {
			return err
		}
		s.log.Info("updated index", "title", cur.Title, "entries", len(node.Children))
	}
	return nil
}

// buildIndex lists children newest first. Meeting documents are ordered by
// meeting time and prefixed with their date; other children (e.g. month
// containers like "09 September") follow in descending title order.
func buildIndex(children []*outline.Node, times map[string]time.Time) string {
	sorted := append([]*outline.Node(nil), children...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, iok := times[sorted[i].ID]
		tj, jok := times[sorted[j].ID]
		switch {
		case iok && jok:
			return ti.After(tj)
		case iok != jok:
			return iok
		default:
			return sorted[i].Title > sorted[j].Title
		}
	})
	var sb strings.Builder
	sb.WriteString(indexHeading + "\n\n")
	if len(sorted) == 0 {
		sb.WriteString("_Nothing here yet._\n")
	}
	for _, n := range sorted {
		date := ""
		if t, ok := times[n.ID]; ok {
			date = t.Format("Mon 2 Jan 2006") + " · "
		}
		fmt.Fprintf(&sb, "- %s[%s](%s)\n", date, escapeLinkText(n.Title), n.URL)
	}
	return sb.String()
}

// replaceSection swaps the section starting at heading (up to the next heading of
// the same or higher level) for section, or appends section when absent.
func replaceSection(text, heading, section string) string {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start < 0 {
		return strings.TrimRight(text, "\n") + "\n\n" + section
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "# ") || strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, strings.TrimRight(section, "\n"), "")
	out = append(out, lines[end:]...)
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

func escapeLinkText(s string) string {
	return strings.NewReplacer("[", `\[`, "]", `\]`).Replace(s)
}
