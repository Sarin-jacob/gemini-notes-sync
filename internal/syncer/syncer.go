// Package syncer moves Gemini notes from Drive into Outline.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sarin/gemini-notes-sync/internal/config"
	"github.com/sarin/gemini-notes-sync/internal/gdrive"
	"github.com/sarin/gemini-notes-sync/internal/gemini"
	"github.com/sarin/gemini-notes-sync/internal/layout"
	"github.com/sarin/gemini-notes-sync/internal/outline"
	"github.com/sarin/gemini-notes-sync/internal/store"
)

const roleMain = "main"

// renderVersion is bumped whenever the generated Markdown changes, so notes
// synced by an older version are rewritten even if their Google Doc is unchanged.
// 2: work around Outline checklist import bugs (lost "Details" section).
const renderVersion = 2

type Syncer struct {
	cfg   *config.Config
	drive *gdrive.Client
	ol    *outline.Client
	st    *store.Store
	tpl   *layout.Templates
	loc   *time.Location
	name  *regexp.Regexp
	drop  []*regexp.Regexp
	log   *slog.Logger

	collection outline.Collection
	renderKey  string
}

// renderKeyFor fingerprints everything that shapes the generated documents: the
// renderer version and the output settings. When it changes, every note is
// rewritten on the next pass even if its Google Doc is unchanged.
func renderKeyFor(cfg *config.Config) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "v%d\x00%s\x00%s\x00%s\x00%s\x00%q\x00%q\x00%s",
		renderVersion, cfg.Layout.Path, cfg.Layout.Title, cfg.Layout.ChildTitle,
		cfg.Content.Main, cfg.Content.Children, cfg.Content.DropLines, cfg.Sync.Timezone))
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
	return s, nil
}

// Notes lists the Gemini notes in scope, oldest first.
func (s *Syncer) Notes(ctx context.Context) ([]gdrive.File, error) {
	roots, err := s.drive.Roots(ctx, s.cfg.Google.FolderIDs)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, errors.New("no folders are shared with the service account")
	}
	files, err := s.drive.Notes(ctx, roots, s.name)
	if err != nil {
		return nil, err
	}
	if lb := s.cfg.Sync.Lookback; lb > 0 {
		cutoff := time.Now().Add(-lb)
		files = filter(files, func(f gdrive.File) bool { return f.Created.After(cutoff) })
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Created.Before(files[j].Created) })
	return files, nil
}

// Run performs one sync pass.
func (s *Syncer) Run(ctx context.Context) (Result, error) {
	var res Result
	files, err := s.Notes(ctx)
	if err != nil {
		return res, err
	}
	res.Notes = len(files)
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
	for _, f := range files {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		log := s.log.With("note", f.Name)
		outcome, err := s.syncNote(ctx, f, &tree, touched, log)
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
	Meeting  layout.Meeting
	Path     []string
	Title    string
	Main     string            // tab key used for the meeting document
	Children map[string]string // tab key -> document title, in content order
	Order    []string
	Images   int
	Doc      *gemini.Doc
}

// Prepare exports and parses a note and renders its layout.
func (s *Syncer) Prepare(ctx context.Context, f gdrive.File) (*Plan, error) {
	md, err := s.drive.ExportMarkdown(ctx, f.ID)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}
	doc := gemini.Parse(md, s.drop)

	title, start, untitled, ok := gemini.ParseName(f.Name, s.loc)
	if !ok {
		start = f.Created.In(s.loc)
	}
	m := layout.Meeting{
		Title: title, RawTitle: f.Name, Untitled: untitled, Date: start,
		Attendees: doc.Attendees, MeetCode: layout.MeetCode(f.Folder),
		EventID: doc.EventID, SeriesID: doc.SeriesID,
		DriveID: f.ID, DriveURL: f.Link, Folder: f.Folder,
	}
	p := &Plan{File: f, Meeting: m, Doc: doc, Images: len(doc.Images), Children: map[string]string{}}
	if p.Path, err = s.tpl.Path(m); err != nil {
		return nil, err
	}
	if p.Title, err = s.tpl.Title(m); err != nil {
		return nil, err
	}

	// The configured main tab, or the first available configured child if it is missing.
	for _, key := range append([]string{s.cfg.Content.Main}, s.cfg.Content.Children...) {
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
	for _, key := range s.cfg.Content.Children {
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

func (s *Syncer) syncNote(ctx context.Context, f gdrive.File, tree *[]*outline.Node, touched map[string]bool, log *slog.Logger) (outcome, error) {
	modified := f.Modified.UTC().Format(time.RFC3339Nano)
	if prev, key, ok, err := s.st.NoteState(f.ID); err != nil {
		return 0, err
	} else if ok && prev == modified && key == s.renderKey {
		return unchanged, nil
	}

	p, err := s.Prepare(ctx, f)
	if err != nil {
		return 0, err
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
	if err := s.st.MarkSynced(f.ID, modified, p.Title, s.renderKey, p.Meeting.Date); err != nil {
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

	uris, err := s.drive.ImageURIs(ctx, p.File.ID)
	if err != nil {
		log.Warn("could not read full-resolution images; using export copies", "err", err)
	} else if len(uris) != len(p.Doc.Images) {
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
