// Command outlinespike is a throwaway Phase 0 probe of the Outline API: nested
// documents, update, move, cross-links/backlinks, title limit, image upload and
// a long transcript. Everything is created under one "spike" root document in the
// target collection; run with -cleanup to delete it again.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type client struct {
	base, key string
	http      *http.Client
}

// call POSTs a JSON body to /api/<method> and decodes the "data" field into out.
func (c *client) call(method string, body, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.base+"/api/"+method, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d: %s", method, resp.StatusCode, truncate(string(raw), 300))
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	return json.Unmarshal(env.Data, out)
}

type doc struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	URLID    string `json:"urlId"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	ParentID string `json:"parentDocumentId"`
}

func (c *client) create(title, text, collection, parent string) doc {
	var d doc
	body := map[string]any{"title": title, "text": text, "collectionId": collection, "publish": true}
	if parent != "" {
		body["parentDocumentId"] = parent
	}
	if err := c.call("documents.create", body, &d); err != nil {
		log.Fatalf("create %q: %v", title, err)
	}
	fmt.Printf("  created %-40q %s%s\n", truncate(title, 40), c.base, d.URL)
	return d
}

func main() {
	cleanup := flag.Bool("cleanup", false, "delete the spike root document and exit")
	flag.Parse()

	env := loadEnv(".env")
	c := &client{base: strings.TrimRight(env["outline_url"], "/"), key: env["OUTLINE_API_KEY"], http: &http.Client{Timeout: 60 * time.Second}}

	var auth struct {
		User struct{ Name, Email string } `json:"user"`
		Team struct{ Name string }        `json:"team"`
	}
	if err := c.call("auth.info", map[string]any{}, &auth); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("auth: %s (team %s)\n", auth.User.Name, auth.Team.Name)

	var cols []struct{ ID, Name string }
	if err := c.call("collections.list", map[string]any{"limit": 100}, &cols); err != nil {
		log.Fatal(err)
	}
	var colID string
	for _, col := range cols {
		if strings.EqualFold(col.Name, env["target_collection"]) {
			colID = col.ID
		}
	}
	if colID == "" {
		log.Fatalf("collection %q not found among %d collections", env["target_collection"], len(cols))
	}
	fmt.Println("collection:", env["target_collection"], colID)

	const rootTitle = "zz-spike (safe to delete)"
	if *cleanup {
		var found []doc
		c.call("documents.search_titles", map[string]any{"query": rootTitle, "collectionId": colID}, &found)
		for _, d := range found {
			if d.Title == rootTitle {
				if err := c.call("documents.delete", map[string]any{"id": d.ID}, nil); err != nil {
					log.Fatal(err)
				}
				fmt.Println("deleted", d.ID)
			}
		}
		return
	}

	fmt.Println("\n[1] nesting")
	root := c.create(rootTitle, "Created by cmd/outlinespike.", colID, "")
	year := c.create("2026", "", colID, root.ID)
	month := c.create("09 September", "", colID, year.ID)

	fmt.Println("\n[2] title limit")
	for _, n := range []int{100, 101, 150} {
		var d doc
		err := c.call("documents.create", map[string]any{"title": strings.Repeat("x", n), "text": "", "collectionId": colID, "parentDocumentId": root.ID, "publish": true}, &d)
		fmt.Printf("  title len %d: err=%v stored len=%d\n", n, err, len(d.Title))
	}

	fmt.Println("\n[3] links + backlinks")
	a := c.create("2026-09-04 — Meeting A", "First meeting.", colID, month.ID)
	b := c.create("2026-09-07 — Meeting B", fmt.Sprintf("← previous: [%s](%s)", a.Title, a.URL), colID, month.ID)
	var backlinks []doc
	if err := c.call("documents.list", map[string]any{"backlinkDocumentId": a.ID}, &backlinks); err != nil {
		fmt.Println("  backlinks query failed:", err)
	} else {
		fmt.Printf("  backlinks of A: %d (want 1, from B=%v)\n", len(backlinks), len(backlinks) == 1 && backlinks[0].ID == b.ID)
	}

	fmt.Println("\n[4] update + move")
	var upd doc
	if err := c.call("documents.update", map[string]any{"id": a.ID, "text": fmt.Sprintf("First meeting.\n\nnext →: [%s](%s)", b.Title, b.URL)}, &upd); err != nil {
		fmt.Println("  update failed:", err)
	} else {
		fmt.Println("  update ok, text now", len(upd.Text), "chars")
	}
	if err := c.call("documents.move", map[string]any{"id": b.ID, "collectionId": colID, "parentDocumentId": year.ID}, nil); err != nil {
		fmt.Println("  move failed:", err)
	} else {
		var info doc
		c.call("documents.info", map[string]any{"id": b.ID}, &info)
		fmt.Printf("  move ok, parent is year doc: %v, url unchanged: %v\n", info.ParentID == year.ID, info.URL == b.URL)
	}

	fmt.Println("\n[5] image upload")
	if img, err := firstImage("spike-out/04.md"); err != nil {
		fmt.Println("  skipped:", err)
	} else if url, err := c.upload(img, "spike.png", "image/png", root.ID); err != nil {
		fmt.Println("  upload failed:", err)
	} else {
		fmt.Printf("  uploaded %d bytes -> %s\n", len(img), url)
		c.create("Image test", fmt.Sprintf("Screenshot:\n\n![screenshot](%s)", url), colID, root.ID)
	}

	fmt.Println("\n[6] long transcript")
	for _, f := range []string{"spike-out/04.md", "spike-out/02.md"} {
		t, err := transcript(f)
		if err != nil {
			fmt.Println("  skipped:", err)
			continue
		}
		var d doc
		err = c.call("documents.create", map[string]any{"title": "Transcript " + f, "text": t, "collectionId": colID, "parentDocumentId": root.ID, "publish": true}, &d)
		fmt.Printf("  %s: %d chars -> err=%v stored=%d chars\n", f, len(t), err, len(d.Text))
	}

	fmt.Printf("\nDone. Inspect under %q, then run with -cleanup.\n", rootTitle)
}

// upload creates an attachment and pushes the bytes to the returned upload URL.
func (c *client) upload(data []byte, name, contentType, docID string) (string, error) {
	var res struct {
		UploadURL  string               `json:"uploadUrl"`
		Form       map[string]string    `json:"form"`
		Attachment struct{ URL string } `json:"attachment"`
	}
	if err := c.call("attachments.create", map[string]any{"name": name, "contentType": contentType, "size": len(data), "documentId": docID, "preset": "documentAttachment"}, &res); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range res.Form {
		w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("file", name)
	fw.Write(data)
	w.Close()
	u := res.UploadURL
	if strings.HasPrefix(u, "/") {
		u = c.base + u
	}
	req, _ := http.NewRequest("POST", u, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if strings.HasPrefix(u, c.base) {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return res.Attachment.URL, nil
}

var dataURI = regexp.MustCompile(`<data:image/png;base64,([A-Za-z0-9+/=]+)>`)

func firstImage(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := dataURI.FindSubmatch(b)
	if m == nil {
		return nil, fmt.Errorf("no png data URI in %s", path)
	}
	return base64.StdEncoding.DecodeString(string(m[1]))
}

// transcript returns the Transcript tab of an exported note, without image definitions.
func transcript(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := string(b)
	i := strings.Index(s, "# **📖 Transcript**")
	if i < 0 {
		return "", fmt.Errorf("no transcript tab in %s", path)
	}
	s = s[i:]
	if j := strings.Index(s, "\n[image1]:"); j >= 0 {
		s = s[:j]
	}
	return s, nil
}

func loadEnv(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && !strings.HasPrefix(k, "#") {
			m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
