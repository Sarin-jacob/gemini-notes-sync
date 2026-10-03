// Package syncer moves meeting notes (Gemini, Zoom) from Drive into Outline.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sarin/gemini-notes-sync/internal/config"
	"github.com/sarin/gemini-notes-sync/internal/docx"
	"github.com/sarin/gemini-notes-sync/internal/gdrive"
	"github.com/sarin/gemini-notes-sync/internal/gemini"
	"github.com/sarin/gemini-notes-sync/internal/layout"
	"github.com/sarin/gemini-notes-sync/internal/note"
	"github.com/sarin/gemini-notes-sync/internal/outline"
	"github.com/sarin/gemini-notes-sync/internal/store"
	"github.com/sarin/gemini-notes-sync/internal/zoom"
)

const roleMain = "main"

// renderVersion is bumped whenever the generated Markdown changes, so notes
// synced by an older version are rewritten even if their Google Doc is unchanged.
// 2: work around Outline checklist import bugs (lost "Details" section).
// 3: "Source" line in the meeting header (Zoom support).
const renderVersion = 3

type Syncer struct {
	cfg   *config.Config
	drive *gdrive.Client
	ol    *outline.Client
	st    *store.Store
	tpl   *layout.Templates
	loc   *time.Location
	name  *regexp.Regexp
	drop  []*regexp.Regexp
	zdrop []*regexp.Regexp
	log   *slog.Logger

	collection outline.Collection
	renderKey  string
}

// renderKeyFor fingerprints everything that shapes the generated documents: the
// renderer version and the output settings. When it changes, every note is
// rewritten on the next pass even if its Google Doc is unchanged.
func renderKeyFor(cfg *config.Config) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "v%d\x00%s\x00%s\x00%s\x00%s\x00%q\x00%q\x00%s\x00%s\x00%q\x00%q\x00%s",
		renderVersion, cfg.Layout.Path, cfg.Layout.Title, cfg.Layout.ChildTitle,
		cfg.Content.Main, cfg.Content.Children, cfg.Content.DropLines, cfg.Sync.Timezone,
		cfg.Zoom.Main, cfg.Zoom.Children, cfg.Zoom.DropLines, cfg.Zoom.DateOrder))
	return hex.EncodeToString(sum[:8])
}

type Result struct {
	Notes, Synced, Unchanged, Conflicts, Failed int
}

func New(ctx context.Context, cfg *config.Config, drv *gdrive.Client, ol *outline.Client, st *store.Store, log *slog.Logger) (*Syncer, error) {
	tpl, err := layout.New(cfg.Layout.Path, cfg.Layout.Title, cfg.Layout.ChildTitle)
	if err != nil {
		return nil, err
	}
	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}
	s := &Syncer{cfg: cfg, drive: drv, ol: ol, st: st, tpl: tpl, loc: loc, log: log,
		name: regexp.MustCompile(cfg.Google.NotesNamePattern), renderKey: renderKeyFor(cfg)}
	for _, p := range cfg.Content.DropLines {
		s.drop = append(s.drop, regexp.MustCompile(p))
	}
	for _, p := range cfg.Zoom.DropLines {
		s.zdrop = append(s.zdrop, regexp.MustCompile(p))
	}
	return s, nil
}

// Item is a note file and the source it came from.
type Item struct {
	gdrive.File
	Source string // note.Gemini or note.Zoom
}

// Notes lists the notes in scope from every source, oldest first.
func (s *Syncer) Notes(ctx context.Context) ([]Item, error) {
	zoomRoots, err := s.drive.ResolveFolders(ctx, s.cfg.Zoom.Folders)
	if err != nil {
		return nil, fmt.Errorf("zoom folders: %w", err)
	}
	roots, err := s.drive.Roots(ctx, s.cfg.Google.FolderIDs)
	if err != nil {
		return nil, err
	}
	for id := range zoomRoots {
		delete(roots, id) // the Zoom drop folder is not a Gemini source
	}
	if len(roots) == 0 && len(zoomRoots) == 0 {
		return nil, errors.New("no folders are shared with the service account")
	}

	var items []Item
	files, err := s.drive.Notes(ctx, roots, s.name)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		items = append(items, Item{File: f, Source: note.Gemini})
	}
	if len(zoomRoots) > 0 {
		files, err := s.drive.Walk(ctx, zoomRoots, IsZoomFile)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			items = append(items, Item{File: f, Source: note.Zoom})
		}
	}

	if lb := s.cfg.Sync.Lookback; lb > 0 {
		cutoff := time.Now().Add(-lb)
		items = filter(items, func(it Item) bool { return it.Created.After(cutoff) })
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Created.Before(items[j].Created) })
	return items, nil
}

// IsZoomFile accepts the formats the Zoom drop folder supports.
func IsZoomFile(f gdrive.File) bool {
	switch strings.ToLower(path.Ext(f.Name)) {
	case ".docx", ".md", ".markdown", ".txt":
		return true
	}
	return f.MimeType == gdrive.DocMime || f.MimeType == gdrive.DocxMime || f.MimeType == "text/markdown" || f.MimeType == "text/plain"
}

// Run performs one sync pass.
func (s *Syncer) Run(ctx context.Context) (Result, error) {
	var res Result
	items, err := s.Notes(ctx)
	if err != nil {
		return res, err
	}
	res.Notes = len(items)
	if s.collection.ID == "" {
		if s.collection, err = s.ol.FindCollection(ctx, s.cfg.Outline.Collection); err != nil {
			return res, err
		}
	}
	tree, err := s.ol.Tree(ctx, s.collection.ID)
	if err != nil {
		return res, err
	}
	touched := map[string]bool{}
	for _, it := range items {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		log := s.log.With("note", it.Name, "source", it.Source)
		outcome, err := s.syncNote(ctx, it, &tree, touched, log)
		switch {
		case err != nil:
			res.Failed++
			log.Error("sync failed", "err", err)
		case outcome == unchanged:
			res.Unchanged++
		case outcome == conflict:
			res.Synced++
			res.Conflicts++
		default:
			res.Synced++
		}
	}
	if len(touched) > 0 {
		if err := s.rebuildIndexes(ctx, touched); err != nil {
			return res, fmt.Errorf("rebuild indexes: %w", err)
		}
	}
	return res, nil
}

type outcome int

const (
	unchanged outcome = iota
	written
	conflict
)

// Plan describes what a sync would write for one note, without touching Outline.
type Plan struct {
	File     gdrive.File
	Source   string
	HasTime  bool // the meeting time of day is known (not just the date)
	Meeting  layout.Meeting
	Path     []string
	Title    string
	Main     string            // tab key used for the meeting document
	Children map[string]string // tab key -> document title, in content order
	Order    []string
	Images   int
	Doc      *note.Doc
}

// Prepare fetches and parses a note and renders its layout.
func (s *Syncer) Prepare(ctx context.Context, it Item) (*Plan, error) {
	f := it.File
	var (
		doc            *note.Doc
		m              layout.Meeting
		main, children = s.cfg.Content.Main, s.cfg.Content.Children
		hasTime        bool
	)
	switch it.Source {
	case note.Zoom:
		md, err := s.zoomMarkdown(ctx, f)
		if err != nil {
			return nil, err
		}
		opt := zoom.Options{Location: s.loc, DayFirst: s.cfg.Zoom.DateOrder == "dmy", ExtraDrop: s.zdrop}
		var meta zoom.Meta
		doc, meta = zoom.Parse(md, f.Name, f.Created, opt)
		// Without a time of day, a generic title beats "Meeting at 00:00".
		m = layout.Meeting{Source: "Zoom", Title: meta.Title, Untitled: meta.Untitled && meta.HasTime, Date: meta.Start}
		hasTime = meta.HasTime
		main, children = s.cfg.Zoom.Main, s.cfg.Zoom.Children
	default:
		md, err := s.drive.ExportMarkdown(ctx, f.ID)
		if err != nil {
			return nil, fmt.Errorf("export: %w", err)
		}
		doc = gemini.Parse(md, s.drop)
		title, start, untitled, ok := gemini.ParseName(f.Name, s.loc)
		if !ok {
			start = f.Created.In(s.loc)
		}
		m = layout.Meeting{Source: "Gemini", Title: title, Untitled: untitled, Date: start, MeetCode: layout.MeetCode(f.Folder)}
		hasTime = true // from the file name, or the creation time as a fallback
	}
	m.RawTitle, m.Attendees, m.EventID, m.SeriesID = f.Name, doc.Attendees, doc.EventID, doc.SeriesID
	m.DriveID, m.DriveURL, m.Folder = f.ID, f.Link, f.Folder

	p := &Plan{File: f, Source: it.Source, HasTime: hasTime, Meeting: m, Doc: doc, Images: len(doc.Images), Children: map[string]string{}}
	var err error
	if p.Path, err = s.tpl.Path(m); err != nil {
		return nil, err
	}
	if p.Title, err = s.tpl.Title(m); err != nil {
		return nil, err
	}

	// The configured main tab, or the first available configured child if it is missing.
	for _, key := range append([]string{main}, children...) {
		if _, ok := doc.Tab(key); ok {
			p.Main = key
			break
		}
	}
	if p.Main == "" {
		if len(doc.Tabs) == 0 {
			return nil, errors.New("export contains no content")
		}
		p.Main = doc.Tabs[0].Key
	}
	for _, key := range children {
		tab, ok := doc.Tab(key)
		if !ok || key == p.Main || strings.TrimSpace(tab.Body) == "" {
			continue
		}
		ct, err := s.tpl.ChildTitle(layout.Child{Meeting: m, Tab: tab.Name, MainTitle: p.Title})
		if err != nil {
			return nil, err
		}
		p.Children[key] = ct
		p.Order = append(p.Order, key)
	}
	return p, nil
}

// zoomMarkdown fetches a file from the Zoom drop folder as Markdown.
func (s *Syncer) zoomMarkdown(ctx context.Context, f gdrive.File) (string, error) {
	if f.MimeType == gdrive.DocMime {
		md, err := s.drive.ExportMarkdown(ctx, f.ID)
		if err != nil {
			return "", fmt.Errorf("export: %w", err)
		}
		return md, nil
	}
	data, err := s.drive.DownloadFile(ctx, f.ID)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if f.MimeType == gdrive.DocxMime || strings.EqualFold(path.Ext(f.Name), ".docx") {
		md, err := docx.ToMarkdown(data)
		if err != nil {
			return "", fmt.Errorf("convert %s: %w", f.Name, err)
		}
		return md, nil
	}
	return string(data), nil
}

func (s *Syncer) syncNote(ctx context.Context, it Item, tree *[]*outline.Node, touched map[string]bool, log *slog.Logger) (outcome, error) {
	f := it.File
	modified := f.Modified.UTC().Format(time.RFC3339Nano)
	prev, key, known, err := s.st.NoteState(f.ID)
	if err != nil {
		return 0, err
	} else if known && prev == modified && key == s.renderKey {
		return unchanged, nil
	}

	p, err := s.Prepare(ctx, it)
	if err != nil {
		return 0, err
	}
	if !known && it.Source == note.Zoom {
		// A summary uploaded again as a new file updates the meeting's existing documents.
		if earlier, ok, err := s.st.FindMeeting(it.Source, p.Title, p.Meeting.Date); err != nil {
			return 0, err
		} else if ok && earlier != f.ID {
			if err := s.st.CopyDocs(earlier, f.ID); err != nil {
				return 0, err
			}
			log.Info("re-uploaded summary; updating the existing documents")
		}
	}
	parentID, chain, err := s.ensurePath(ctx, tree, p.Path)
	if err != nil {
		return 0, fmt.Errorf("containers: %w", err)
	}
	for _, id := range chain {
		touched[id] = true
	}

	refs, err := s.st.Docs(f.ID)
	if err != nil {
		return 0, err
	}
	roles := append([]string{roleMain}, p.Order...)
	titles := map[string]string{roleMain: p.Title}
	for k, t := range p.Children {
		titles[k] = t
	}

	// Pass 1: make sure every document exists (so all URLs are known for linking)
	// and find documents edited in Outline since we last wrote them.
	skip := map[string]bool{}
	for _, role := range roles {
		ref, ok := refs[role]
		if ok {
			cur, err := s.ol.Info(ctx, ref.OutlineID)
			switch {
			case errors.Is(err, outline.ErrNotFound):
				log.Warn("document was deleted in Outline; recreating", "role", role)
				ok = false
			case err != nil:
				return 0, err
			case s.cfg.Sync.ConflictPolicy == "skip" && hashDoc(cur) != ref.WrittenHash:
				log.Warn("document was edited in Outline; leaving it alone", "role", role, "url", s.cfg.Outline.BaseURL+ref.URL)
				skip[role] = true
			}
		}
		if !ok {
			parent := parentID
			if role != roleMain {
				parent = refs[roleMain].OutlineID
			}
			d, err := s.ol.Create(ctx, s.collection.ID, parent, titles[role], "_Syncing…_")
			if err != nil {
				return 0, fmt.Errorf("create %s: %w", role, err)
			}
			ref = store.DocRef{OutlineID: d.ID, URL: d.URL, WrittenHash: hashDoc(d)}
			if err := s.st.PutDoc(f.ID, role, ref); err != nil {
				return 0, err
			}
			log.Info("created document", "role", role, "url", s.cfg.Outline.BaseURL+d.URL)
		}
		refs[role] = ref
	}

	images, err := s.uploadImages(ctx, p, refs[roleMain].OutlineID, skip, log)
	if err != nil {
		return 0, err
	}

	// Pass 2: render with final links and write.
	for _, role := range roles {
		if skip[role] {
			continue
		}
		text := s.render(p, role, refs, images)
		d, err := s.ol.Update(ctx, refs[role].OutlineID, titles[role], text)
		if err != nil {
			return 0, fmt.Errorf("update %s: %w", role, err)
		}
		ref := refs[role]
		ref.WrittenHash = hashDoc(d)
		if err := s.st.PutDoc(f.ID, role, ref); err != nil {
			return 0, err
		}
	}
	if err := s.st.MarkSynced(f.ID, it.Source, modified, p.Title, s.renderKey, p.Meeting.Date); err != nil {
		return 0, err
	}
	log.Info("synced", "title", p.Title, "url", s.cfg.Outline.BaseURL+refs[roleMain].URL)
	if len(skip) > 0 {
		return conflict, nil
	}
	return written, nil
}

// ensurePath finds or creates the container documents for path and returns the
// innermost one's ID plus the IDs of every container on the way.
func (s *Syncer) ensurePath(ctx context.Context, tree *[]*outline.Node, path []string) (string, []string, error) {
	level := tree
	parentID := ""
	var chain []string
	for _, seg := range path {
		var node *outline.Node
		for _, n := range *level {
			if n.Title == seg {
				node = n
				break
			}
		}
		if node == nil {
			d, err := s.ol.Create(ctx, s.collection.ID, parentID, seg, "")
			if err != nil {
				return "", nil, fmt.Errorf("create %q: %w", seg, err)
			}
			s.log.Info("created container", "title", seg, "url", s.cfg.Outline.BaseURL+d.URL)
			node = &outline.Node{ID: d.ID, Title: d.Title, URL: d.URL}
			*level = append(*level, node)
		}
		parentID = node.ID
		chain = append(chain, node.ID)
		level = &node.Children
	}
	return parentID, chain, nil
}

// FullResImages reports how many full-resolution images the Docs API returns
// for a note; it must equal Plan.Images for them to be used.
func (s *Syncer) FullResImages(ctx context.Context, f gdrive.File) (int, error) {
	uris, err := s.drive.ImageURIs(ctx, f.ID)
	return len(uris), err
}

var imageRefRe = regexp.MustCompile(`!\[([^\]]*)\]\[(image\d+)\]`)

// uploadImages uploads the images referenced by the tabs being written and
// returns their Outline URLs keyed by reference ("image3"). Full-resolution
// originals come from the Docs API; the export's downscaled copies are a fallback.
func (s *Syncer) uploadImages(ctx context.Context, p *Plan, docID string, skip map[string]bool, log *slog.Logger) (map[string]string, error) {
	needed := map[string]bool{}
	for _, role := range append([]string{p.Main}, p.Order...) {
		if (role == p.Main && skip[roleMain]) || skip[role] {
			continue
		}
		tab, _ := p.Doc.Tab(role)
		for _, m := range imageRefRe.FindAllStringSubmatch(tab.Body, -1) {
			needed[m[2]] = true
		}
	}
	urls := map[string]string{}
	if len(needed) == 0 {
		return urls, nil
	}

	var uris []string
	var err error
	if p.File.MimeType == gdrive.DocMime {
		uris, err = s.drive.ImageURIs(ctx, p.File.ID)
	}
	if err != nil {
		log.Warn("could not read full-resolution images; using export copies", "err", err)
	} else if uris != nil && len(uris) != len(p.Doc.Images) {
		log.Warn("image count mismatch between Docs API and export; using export copies", "docs", len(uris), "export", len(p.Doc.Images))
		uris = nil
	}

	for ref := range needed {
		var n int
		fmt.Sscanf(ref, "image%d", &n)
		var data []byte
		ctype := ""
		if n >= 1 && n <= len(uris) && uris[n-1] != "" {
			data, ctype, err = s.drive.Download(ctx, uris[n-1])
			if err != nil {
				log.Warn("full-resolution download failed; using export copy", "image", ref, "err", err)
				data = nil
			}
		}
		if data == nil {
			img, ok := p.Doc.Images[ref]
			if !ok {
				continue
			}
			data, ctype = img.Data, img.MIME
		}
		if ctype == "" || !strings.HasPrefix(ctype, "image/") {
			ctype = http.DetectContentType(data)
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		url, err := s.st.Attachment(sha)
		if err != nil {
			return nil, err
		}
		if url == "" {
			ext := strings.TrimPrefix(ctype, "image/")
			url, err = s.ol.Upload(ctx, docID, fmt.Sprintf("%s-%s.%s", p.Meeting.Date.Format("2006-01-02"), ref, ext), ctype, data)
			if err != nil {
				return nil, fmt.Errorf("upload %s: %w", ref, err)
			}
			if err := s.st.PutAttachment(sha, url); err != nil {
				return nil, err
			}
		}
		urls[ref] = url
	}
	return urls, nil
}

func hashDoc(d outline.Document) string {
	sum := sha256.Sum256([]byte(d.Title + "\x00" + strings.TrimSpace(d.Text)))
	return hex.EncodeToString(sum[:])
}

func filter[T any](xs []T, keep func(T) bool) []T {
	var out []T
	for _, x := range xs {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}
