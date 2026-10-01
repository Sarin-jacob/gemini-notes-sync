// Command imgprobe fetches the first few inline images of a Gemini note via the
// Docs API contentUri and reports their real dimensions.
package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/oauth2/google"
	docs "google.golang.org/api/docs/v1"
	"google.golang.org/api/option"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: imgprobe <docId>")
	}
	ctx := context.Background()
	key, err := os.ReadFile("key.json")
	if err != nil {
		log.Fatal(err)
	}
	creds, err := google.CredentialsFromJSON(ctx, key, docs.DocumentsReadonlyScope, "https://www.googleapis.com/auth/drive.readonly")
	if err != nil {
		log.Fatal(err)
	}
	svc, err := docs.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		log.Fatal(err)
	}
	doc, err := svc.Documents.Get(os.Args[1]).IncludeTabsContent(true).Do()
	if err != nil {
		log.Fatal(err)
	}
	ts := creds.TokenSource
	tok, err := ts.Token()
	if err != nil {
		log.Fatal(err)
	}
	n := 0
	for _, t := range doc.Tabs {
		for id, obj := range t.DocumentTab.InlineObjects {
			if n >= 3 {
				return
			}
			n++
			uri := obj.InlineObjectProperties.EmbeddedObject.ImageProperties.ContentUri
			for _, auth := range []bool{false, true} {
				req, _ := http.NewRequest("GET", uri, nil)
				if auth {
					tok.SetAuthHeader(req)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					fmt.Println(id, "auth", auth, "err", err)
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
				fmt.Printf("%s auth=%v HTTP %d %d bytes %s %dx%d err=%v\n", id, auth, resp.StatusCode, len(b), format, cfg.Width, cfg.Height, err)
				if err == nil {
					os.WriteFile(filepath.Join("spike-out", fmt.Sprintf("orig-%d.%s", n, format)), b, 0o644)
					break
				}
			}
		}
	}
}
