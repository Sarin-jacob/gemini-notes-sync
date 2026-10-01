// Command spike is a throwaway Phase 0 probe: it authenticates as the service
// account, walks the shared Meet folders recursively, lists their contents and
// exports a few Gemini notes both as Drive Markdown and as Docs API JSON so the
// two formats can be compared. Output goes to spike-out/ (gitignored).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/oauth2/google"
	docs "google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

const folderMime = "application/vnd.google-apps.folder"

type entry struct {
	f    *drive.File
	path string
}

func main() {
	keyFile := flag.String("key", "key.json", "service account JSON key")
	folderID := flag.String("folder", "", "root folder ID (default: every folder shared with the service account)")
	n := flag.Int("n", 5, "number of notes to export")
	outDir := flag.String("out", "spike-out", "output directory")
	flag.Parse()

	ctx := context.Background()
	key, err := os.ReadFile(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	creds, err := google.CredentialsFromJSON(ctx, key, drive.DriveReadonlyScope, docs.DocumentsReadonlyScope)
	if err != nil {
		log.Fatal(err)
	}
	drv, err := drive.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		log.Fatal(err)
	}
	dcs, err := docs.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		log.Fatal(err)
	}

	var roots []string
	if *folderID != "" {
		roots = []string{*folderID}
	} else {
		fl, err := drv.Files.List().
			Q("mimeType = '" + folderMime + "' and sharedWithMe = true and trashed = false").
			Fields("files(id, name)").Do()
		if err != nil {
			log.Fatalf("list shared folders: %v", err)
		}
		for _, f := range fl.Files {
			fmt.Printf("root: %s  %q\n", f.Id, f.Name)
			roots = append(roots, f.Id)
		}
	}

	files := walk(ctx, drv, roots)
	sort.Slice(files, func(i, j int) bool { return files[i].f.CreatedTime > files[j].f.CreatedTime })

	mimeCount := map[string]int{}
	fmt.Printf("\n%d files (newest first):\n", len(files))
	for _, e := range files {
		mimeCount[e.f.MimeType]++
		fmt.Printf("  %-16s %-12s %-45s %s\n", e.f.CreatedTime[:16], shortMime(e.f.MimeType), e.path, e.f.Name)
	}
	fmt.Println("\nBy type:")
	for m, c := range mimeCount {
		fmt.Printf("  %4d  %s\n", c, m)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	gemini := regexp.MustCompile(`(?i)gemini`)
	exported := 0
	for _, e := range files {
		f := e.f
		if exported >= *n {
			break
		}
		if f.MimeType != "application/vnd.google-apps.document" || !gemini.MatchString(f.Name) {
			continue
		}
		exported++
		base := filepath.Join(*outDir, fmt.Sprintf("%02d", exported))
		fmt.Printf("\n[%02d] %s\n", exported, f.Name)

		resp, err := drv.Files.Export(f.Id, "text/markdown").Download()
		if err != nil {
			fmt.Println("  markdown export failed:", err)
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			os.WriteFile(base+".md", b, 0o644)
			fmt.Printf("  markdown: %d bytes\n", len(b))
		}

		doc, err := dcs.Documents.Get(f.Id).IncludeTabsContent(true).Do()
		if err != nil {
			fmt.Println("  docs api failed:", err)
			continue
		}
		b, _ := json.MarshalIndent(doc, "", "  ")
		os.WriteFile(base+".docs.json", b, 0o644)
		for _, t := range doc.Tabs {
			printTab(t, "  tab: ")
		}
	}
}

// walk lists every non-folder file under roots, breadth-first, with its folder path.
func walk(ctx context.Context, drv *drive.Service, roots []string) []entry {
	type dir struct{ id, path string }
	var queue []dir
	for _, r := range roots {
		f, err := drv.Files.Get(r).Fields("id, name").Do()
		if err != nil {
			log.Fatalf("get root %s: %v", r, err)
		}
		queue = append(queue, dir{r, f.Name})
	}
	var files []entry
	seen := map[string]bool{}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if seen[d.id] {
			continue
		}
		seen[d.id] = true
		err := drv.Files.List().
			Q(fmt.Sprintf("'%s' in parents and trashed = false", d.id)).
			Fields("nextPageToken, files(id, name, mimeType, createdTime, modifiedTime, size)").
			PageSize(200).Pages(ctx, func(p *drive.FileList) error {
			for _, f := range p.Files {
				if f.MimeType == folderMime {
					queue = append(queue, dir{f.Id, d.path + "/" + f.Name})
					continue
				}
				files = append(files, entry{f, d.path})
			}
			return nil
		})
		if err != nil {
			log.Fatalf("list %s: %v", d.path, err)
		}
	}
	return files
}

func printTab(t *docs.Tab, prefix string) {
	fmt.Printf("%s%q\n", prefix, t.TabProperties.Title)
	for _, c := range t.ChildTabs {
		printTab(c, "  "+prefix)
	}
}

func shortMime(m string) string { return strings.TrimPrefix(m, "application/vnd.google-apps.") }
