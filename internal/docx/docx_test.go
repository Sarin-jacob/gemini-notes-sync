package docx

import (
	"archive/zip"
	"bytes"
	"testing"
)

const ns = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

// build assembles a minimal .docx from its parts.
func build(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestToMarkdown(t *testing.T) {
	data := build(t, map[string]string{
		"word/document.xml": `<?xml version="1.0"?><w:document ` + ns + `><w:body>
<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr><w:r><w:t>Weekly Sync</w:t></w:r></w:p>
<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:rPr><w:b/></w:rPr><w:t>Quick recap</w:t></w:r></w:p>
<w:p><w:r><w:t xml:space="preserve">The team </w:t></w:r><w:r><w:rPr><w:b/></w:rPr><w:t>agreed</w:t></w:r><w:r><w:t xml:space="preserve"> on the </w:t></w:r><w:hyperlink r:id="rId9"><w:r><w:t>plan</w:t></w:r></w:hyperlink><w:r><w:t>.</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>First</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="1"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:rPr><w:i/></w:rPr><w:t>Nested</w:t></w:r></w:p>
<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="2"/></w:numPr></w:pPr><w:r><w:t>Step one</w:t></w:r></w:p>
<w:p></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Who</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>What</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>Ada</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>a|b</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
<w:p><w:r><w:rPr><w:b w:val="0"/></w:rPr><w:t>Not bold</w:t></w:r><w:r><w:br/><w:t>next line</w:t></w:r></w:p>
</w:body></w:document>`,
		"word/styles.xml": `<?xml version="1.0"?><w:styles ` + ns + `>
<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/></w:style>
<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/></w:style>
</w:styles>`,
		"word/numbering.xml": `<?xml version="1.0"?><w:numbering ` + ns + `>
<w:abstractNum w:abstractNumId="10"><w:lvl w:ilvl="0"><w:numFmt w:val="bullet"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="bullet"/></w:lvl></w:abstractNum>
<w:abstractNum w:abstractNumId="11"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum>
<w:num w:numId="1"><w:abstractNumId w:val="10"/></w:num>
<w:num w:numId="2"><w:abstractNumId w:val="11"/></w:num>
</w:numbering>`,
		"word/_rels/document.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId9" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="https://example.com/plan" TargetMode="External"/>
</Relationships>`,
	})

	got, err := ToMarkdown(data)
	if err != nil {
		t.Fatal(err)
	}
	want := `# Weekly Sync

## Quick recap

The team **agreed** on the [plan](https://example.com/plan).

- First
  - *Nested*
1. Step one

| Who | What |
| --- | --- |
| Ada | a\|b |

Not bold` + "  " + `
next line
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNotDocx(t *testing.T) {
	if _, err := ToMarkdown([]byte("plain text")); err == nil {
		t.Error("expected an error for non-zip input")
	}
}
