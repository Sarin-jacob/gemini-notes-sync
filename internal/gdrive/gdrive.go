// Package gdrive reads Gemini notes from Google Drive as a service account.
package gdrive

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"golang.org/x/oauth2/google"
	docs "google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

const (
	folderMime = "application/vnd.google-apps.folder"
	docMime    = "application/vnd.google-apps.document"
)

type Client struct {
	drive *drive.Service
	docs  *docs.Service
	http  *http.Client
}

// File is a Gemini notes document found in Drive.
type File struct {
	ID       string
	Name     string
	Folder   string // folder path below the shared root, e.g. "Google Meet/abc-defg-hij - 2026/09/27"
	Created  time.Time
	Modified time.Time
	Link     string
}

func New(ctx context.Context, credentialsFile string) (*Client, error) {
	key, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read service account key: %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, key, drive.DriveReadonlyScope, docs.DocumentsReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	drv, err := drive.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, err
	}
	dcs, err := docs.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, err
	}
	return &Client{drive: drv, docs: dcs, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

// Identity returns the service account e-mail as Drive sees it.
func (c *Client) Identity(ctx context.Context) (string, error) {
	about, err := c.drive.About.Get().Fields("user(emailAddress)").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return about.User.EmailAddress, nil
}

// Roots returns the configured folder IDs, or every folder shared with the account.
func (c *Client) Roots(ctx context.Context, configured []string) (map[string]string, error) {
	roots := map[string]string{}
	if len(configured) > 0 {
		for _, id := range configured {
			f, err := c.drive.Files.Get(id).Fields("id, name").Context(ctx).Do()
			if err != nil {
				return nil, fmt.Errorf("folder %s: %w", id, err)
			}
			roots[f.Id] = f.Name
		}
		return roots, nil
	}
	err := c.drive.Files.List().
		Q("mimeType = '"+folderMime+"' and sharedWithMe = true and trashed = false").
		Fields("nextPageToken, files(id, name)").Context(ctx).
		Pages(ctx, func(p *drive.FileList) error {
			for _, f := range p.Files {
				roots[f.Id] = f.Name
			}
			return nil
		})
	return roots, err
}

// Notes walks the roots recursively and returns Google Docs whose name matches.
// A folder shared on its own and also nested in another root is visited once.
func (c *Client) Notes(ctx context.Context, roots map[string]string, name *regexp.Regexp) ([]File, error) {
	type dir struct{ id, path string }
	var queue []dir
	for id, n := range roots {
		queue = append(queue, dir{id, n})
	}
	seen := map[string]bool{}
	var out []File
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if seen[d.id] {
			continue
		}
		seen[d.id] = true
		err := c.drive.Files.List().
			Q(fmt.Sprintf("'%s' in parents and trashed = false", d.id)).
			Fields("nextPageToken, files(id, name, mimeType, createdTime, modifiedTime, webViewLink)").
			PageSize(200).Context(ctx).
			Pages(ctx, func(p *drive.FileList) error {
				for _, f := range p.Files {
					switch {
					case f.MimeType == folderMime:
						queue = append(queue, dir{f.Id, d.path + "/" + f.Name})
					case f.MimeType == docMime && name.MatchString(f.Name):
						created, _ := time.Parse(time.RFC3339, f.CreatedTime)
						modified, _ := time.Parse(time.RFC3339, f.ModifiedTime)
						out = append(out, File{ID: f.Id, Name: f.Name, Folder: d.path, Created: created, Modified: modified, Link: f.WebViewLink})
					}
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", d.path, err)
		}
	}
	return out, nil
}

// ExportMarkdown returns the whole document (all tabs) as Markdown.
func (c *Client) ExportMarkdown(ctx context.Context, id string) (string, error) {
	resp, err := c.drive.Files.Export(id, "text/markdown").Context(ctx).Download()
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// ImageURIs returns the full-resolution content URIs of every inline image, in
// document order across all tabs — the same order the Markdown export numbers
// them (image1, image2, …). The URIs are short-lived; download promptly.
func (c *Client) ImageURIs(ctx context.Context, id string) ([]string, error) {
	doc, err := c.docs.Documents.Get(id).IncludeTabsContent(true).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var uris []string
	var walkTab func(t *docs.Tab)
	walkTab = func(t *docs.Tab) {
		if dt := t.DocumentTab; dt != nil && dt.Body != nil {
			for _, objID := range inlineObjectIDs(dt.Body.Content) {
				obj, ok := dt.InlineObjects[objID]
				if !ok || obj.InlineObjectProperties == nil || obj.InlineObjectProperties.EmbeddedObject == nil ||
					obj.InlineObjectProperties.EmbeddedObject.ImageProperties == nil {
					uris = append(uris, "")
					continue
				}
				uris = append(uris, obj.InlineObjectProperties.EmbeddedObject.ImageProperties.ContentUri)
			}
		}
		for _, child := range t.ChildTabs {
			walkTab(child)
		}
	}
	for _, t := range doc.Tabs {
		walkTab(t)
	}
	return uris, nil
}

func inlineObjectIDs(content []*docs.StructuralElement) []string {
	var ids []string
	for _, el := range content {
		switch {
		case el.Paragraph != nil:
			for _, pe := range el.Paragraph.Elements {
				if pe.InlineObjectElement != nil {
					ids = append(ids, pe.InlineObjectElement.InlineObjectId)
				}
			}
		case el.Table != nil:
			for _, row := range el.Table.TableRows {
				for _, cell := range row.TableCells {
					ids = append(ids, inlineObjectIDs(cell.Content)...)
				}
			}
		case el.TableOfContents != nil:
			ids = append(ids, inlineObjectIDs(el.TableOfContents.Content)...)
		}
	}
	return ids
}

// Download fetches an image content URI.
func (c *Client) Download(ctx context.Context, uri string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", uri, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download image: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	return b, resp.Header.Get("Content-Type"), err
}
