// Package docx converts Word documents to Markdown. It covers what meeting
// summaries use: headings, bulleted and numbered lists, bold and italic text,
// hyperlinks, line breaks and simple tables. Images and other objects are skipped.
package docx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// node is a generic XML element.
type node struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Text    string     `xml:",chardata"`
	Nodes   []node     `xml:",any"`
}

func (n *node) attr(local string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

func (n *node) child(local string) *node {
	for i := range n.Nodes {
		if n.Nodes[i].XMLName.Local == local {
			return &n.Nodes[i]
		}
	}
	return nil
}

type converter struct {
	headings map[string]int    // styleId -> heading level (1-6)
	ordered  map[string]bool   // numId/ilvl -> numbered (vs bulleted)
	links    map[string]string // relationship ID -> URL
}

// ToMarkdown converts the bytes of a .docx file.
func ToMarkdown(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("not a .docx file: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	read := func(name string) (*node, error) {
		f, ok := files[name]
		if !ok {
			return nil, nil
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 64<<20))
		if err != nil {
			return nil, err
		}
		var n node
		if err := xml.Unmarshal(b, &n); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return &n, nil
	}

	doc, err := read("word/document.xml")
	if err != nil {
		return "", err
	}
	if doc == nil {
		return "", errors.New("not a .docx file: word/document.xml missing")
	}
	c := &converter{headings: map[string]int{}, ordered: map[string]bool{}, links: map[string]string{}}
	if styles, err := read("word/styles.xml"); err == nil && styles != nil {
		c.loadStyles(styles)
	}
	if num, err := read("word/numbering.xml"); err == nil && num != nil {
		c.loadNumbering(num)
	}
	if rels, err := read("word/_rels/document.xml.rels"); err == nil && rels != nil {
		for _, r := range rels.Nodes {
			if r.attr("TargetMode") == "External" {
				c.links[r.attr("Id")] = r.attr("Target")
			}
		}
	}

	body := doc.child("body")
	if body == nil {
		return "", errors.New("document has no body")
	}
	var blocks []block
	c.blocks(body.Nodes, &blocks)
	return render(blocks), nil
}

var headingName = regexp.MustCompile(`(?i)^heading\s*([1-6])$`)

func (c *converter) loadStyles(styles *node) {
	for _, s := range styles.Nodes {
		if s.XMLName.Local != "style" || s.attr("type") != "paragraph" {
			continue
		}
		id := s.attr("styleId")
		name := ""
		if n := s.child("name"); n != nil {
			name = n.attr("val")
		}
		switch {
		case strings.EqualFold(name, "Title"):
			c.headings[id] = 1
		case headingName.MatchString(name):
			c.headings[id], _ = strconv.Atoi(headingName.FindStringSubmatch(name)[1])
		default:
			if ppr := s.child("pPr"); ppr != nil {
				if ol := ppr.child("outlineLvl"); ol != nil {
					if lvl, err := strconv.Atoi(ol.attr("val")); err == nil && lvl < 6 {
						c.headings[id] = lvl + 1
					}
				}
			}
		}
	}
}

func (c *converter) loadNumbering(num *node) {
	abstract := map[string]map[string]bool{} // abstractNumId -> ilvl -> ordered
	for _, a := range num.Nodes {
		if a.XMLName.Local != "abstractNum" {
			continue
		}
		levels := map[string]bool{}
		for _, l := range a.Nodes {
			if l.XMLName.Local == "lvl" {
				f := l.child("numFmt")
				levels[l.attr("ilvl")] = f != nil && f.attr("val") != "bullet" && f.attr("val") != "none"
			}
		}
		abstract[a.attr("abstractNumId")] = levels
	}
	for _, n := range num.Nodes {
		if n.XMLName.Local != "num" {
			continue
		}
		if a := n.child("abstractNumId"); a != nil {
			for lvl, ordered := range abstract[a.attr("val")] {
				c.ordered[n.attr("numId")+"/"+lvl] = ordered
			}
		}
	}
}

type blockKind int

const (
	para blockKind = iota
	heading
	listItem
	table
)

type block struct {
	kind    blockKind
	level   int // heading level or list depth
	ordered bool
	text    string
	rows    [][]string
}

func (c *converter) blocks(nodes []node, out *[]block) {
	for i := range nodes {
		n := &nodes[i]
		switch n.XMLName.Local {
		case "p":
			if b, ok := c.paragraph(n); ok {
				*out = append(*out, b)
			}
		case "tbl":
			*out = append(*out, c.table(n))
		case "sdt":
			if content := n.child("sdtContent"); content != nil {
				c.blocks(content.Nodes, out)
			}
		}
	}
}

func (c *converter) paragraph(p *node) (block, bool) {
	b := block{kind: para}
	if ppr := p.child("pPr"); ppr != nil {
		if st := ppr.child("pStyle"); st != nil {
			if lvl, ok := c.headings[st.attr("val")]; ok {
				b.kind, b.level = heading, lvl
			}
		}
		if ol := ppr.child("outlineLvl"); ol != nil && b.kind == para {
			if lvl, err := strconv.Atoi(ol.attr("val")); err == nil && lvl < 6 {
				b.kind, b.level = heading, lvl+1
			}
		}
		if np := ppr.child("numPr"); np != nil && b.kind == para {
			ilvl, numID := "0", ""
			if l := np.child("ilvl"); l != nil {
				ilvl = l.attr("val")
			}
			if id := np.child("numId"); id != nil {
				numID = id.attr("val")
			}
			if numID != "" && numID != "0" {
				b.kind = listItem
				b.level, _ = strconv.Atoi(ilvl)
				b.ordered = c.ordered[numID+"/"+ilvl]
			}
		}
	}
	var segs []segment
	c.inline(p.Nodes, "", &segs)
	b.text = strings.TrimSpace(joinSegments(segs))
	if b.kind == heading {
		b.text = strings.Trim(b.text, "* ") // headings don't need emphasis
	}
	return b, b.text != ""
}

type segment struct {
	text         string
	bold, italic bool
	link         string
}

func (c *converter) inline(nodes []node, link string, segs *[]segment) {
	for i := range nodes {
		n := &nodes[i]
		switch n.XMLName.Local {
		case "r":
			c.run(n, link, segs)
		case "hyperlink":
			target := c.links[n.attr("id")]
			c.inline(n.Nodes, target, segs)
		case "smartTag", "ins", "fldSimple":
			c.inline(n.Nodes, link, segs)
		}
	}
}

func (c *converter) run(r *node, link string, segs *[]segment) {
	s := segment{link: link}
	if rpr := r.child("rPr"); rpr != nil {
		s.bold = on(rpr.child("b"))
		s.italic = on(rpr.child("i"))
	}
	var sb strings.Builder
	for _, n := range r.Nodes {
		switch n.XMLName.Local {
		case "t":
			sb.WriteString(n.Text)
		case "tab":
			sb.WriteString(" ")
		case "br", "cr":
			sb.WriteString("\n")
		}
	}
	s.text = sb.String()
	if s.text != "" {
		*segs = append(*segs, s)
	}
}

// on reports whether a toggle property such as <w:b/> is set.
func on(n *node) bool {
	if n == nil {
		return false
	}
	v := n.attr("val")
	return v == "" || v == "1" || v == "true" || v == "on"
}

// joinSegments renders runs, merging neighbours with identical formatting and
// keeping spaces outside emphasis markers.
func joinSegments(segs []segment) string {
	var merged []segment
	for _, s := range segs {
		if l := len(merged) - 1; l >= 0 && merged[l].bold == s.bold && merged[l].italic == s.italic && merged[l].link == s.link {
			merged[l].text += s.text
			continue
		}
		merged = append(merged, s)
	}
	var sb strings.Builder
	for _, s := range merged {
		core := strings.TrimSpace(s.text)
		if core == "" {
			sb.WriteString(s.text)
			continue
		}
		lead := s.text[:strings.Index(s.text, core)]
		trail := s.text[len(lead)+len(core):]
		core = strings.ReplaceAll(core, "\n", "  \n")
		if s.link != "" {
			core = "[" + core + "](" + s.link + ")"
		}
		switch {
		case s.bold && s.italic:
			core = "***" + core + "***"
		case s.bold:
			core = "**" + core + "**"
		case s.italic:
			core = "*" + core + "*"
		}
		sb.WriteString(lead + core + trail)
	}
	return sb.String()
}

func (c *converter) table(t *node) block {
	b := block{kind: table}
	for _, tr := range t.Nodes {
		if tr.XMLName.Local != "tr" {
			continue
		}
		var row []string
		for _, tc := range tr.Nodes {
			if tc.XMLName.Local != "tc" {
				continue
			}
			var parts []string
			var inner []block
			c.blocks(tc.Nodes, &inner)
			for _, ib := range inner {
				if ib.kind != table {
					parts = append(parts, ib.text)
				}
			}
			cell := strings.Join(parts, " ")
			cell = strings.ReplaceAll(strings.ReplaceAll(cell, "  \n", " "), "|", `\|`)
			row = append(row, cell)
		}
		b.rows = append(b.rows, row)
	}
	return b
}

func render(blocks []block) string {
	var sb strings.Builder
	for i, b := range blocks {
		if i > 0 {
			// Keep consecutive list items together; separate everything else.
			if b.kind == listItem && blocks[i-1].kind == listItem {
				sb.WriteString("\n")
			} else {
				sb.WriteString("\n\n")
			}
		}
		switch b.kind {
		case heading:
			sb.WriteString(strings.Repeat("#", b.level) + " " + b.text)
		case listItem:
			marker := "- "
			if b.ordered {
				marker = "1. "
			}
			sb.WriteString(strings.Repeat("  ", b.level) + marker + b.text)
		case table:
			writeTable(&sb, b.rows)
		default:
			sb.WriteString(b.text)
		}
	}
	return strings.TrimSpace(sb.String()) + "\n"
}

func writeTable(sb *strings.Builder, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r))
	}
	line := func(cells []string) {
		sb.WriteString("|")
		for i := 0; i < width; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			sb.WriteString(" " + cell + " |")
		}
	}
	line(rows[0])
	sb.WriteString("\n|" + strings.Repeat(" --- |", width))
	for _, r := range rows[1:] {
		sb.WriteString("\n")
		line(r)
	}
}
