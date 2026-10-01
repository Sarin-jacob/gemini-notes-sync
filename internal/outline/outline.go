// Package outline is a minimal client for the Outline API.
package outline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxTitle is Outline's title length limit, in characters.
const MaxTitle = 100

var ErrNotFound = errors.New("outline: not found")

type Client struct {
	base, key string
	http      *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), key: apiKey, http: &http.Client{Timeout: 2 * time.Minute}}
}

type Document struct {
	ID               string `json:"id"`
	URL              string `json:"url"` // relative, e.g. /doc/title-AbC123
	Title            string `json:"title"`
	Text             string `json:"text"`
	ParentDocumentID string `json:"parentDocumentId"`
	CollectionID     string `json:"collectionId"`
}

// Node is an entry in a collection's document tree.
type Node struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	URL      string  `json:"url"`
	Children []*Node `json:"children"`
}

type Collection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// call POSTs body to /api/<method> and decodes the response's "data" into out,
// retrying on rate limits and server errors.
func (c *Client) call(ctx context.Context, method string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/api/"+method, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if attempt < 3 && ctx.Err() == nil {
				sleep(ctx, backoff(attempt, ""))
				continue
			}
			return fmt.Errorf("%s: %w", method, err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if out == nil {
				return nil
			}
			var env struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				return fmt.Errorf("%s: decode: %w", method, err)
			}
			return json.Unmarshal(env.Data, out)
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%s: %w", method, ErrNotFound)
		case (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < 5:
			sleep(ctx, backoff(attempt, resp.Header.Get("Retry-After")))
			if ctx.Err() != nil {
				return ctx.Err()
			}
		default:
			msg := string(raw)
			var e struct{ Message string }
			if json.Unmarshal(raw, &e) == nil && e.Message != "" {
				msg = e.Message
			}
			return fmt.Errorf("%s: HTTP %d: %s", method, resp.StatusCode, msg)
		}
	}
}

func backoff(attempt int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(retryAfter); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return time.Duration(1<<attempt) * time.Second
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (c *Client) AuthInfo(ctx context.Context) (user, team string, err error) {
	var res struct {
		User struct{ Name string } `json:"user"`
		Team struct{ Name string } `json:"team"`
	}
	err = c.call(ctx, "auth.info", map[string]any{}, &res)
	return res.User.Name, res.Team.Name, err
}

// FindCollection resolves a collection by ID or (case-insensitive) name.
func (c *Client) FindCollection(ctx context.Context, nameOrID string) (Collection, error) {
	for offset := 0; ; offset += 100 {
		var page []Collection
		if err := c.call(ctx, "collections.list", map[string]any{"limit": 100, "offset": offset}, &page); err != nil {
			return Collection{}, err
		}
		for _, col := range page {
			if col.ID == nameOrID || strings.EqualFold(col.Name, nameOrID) {
				return col, nil
			}
		}
		if len(page) < 100 {
			return Collection{}, fmt.Errorf("collection %q: %w", nameOrID, ErrNotFound)
		}
	}
}

func (c *Client) Tree(ctx context.Context, collectionID string) ([]*Node, error) {
	var nodes []*Node
	err := c.call(ctx, "collections.documents", map[string]any{"id": collectionID}, &nodes)
	return nodes, err
}

func (c *Client) Info(ctx context.Context, id string) (Document, error) {
	var d Document
	err := c.call(ctx, "documents.info", map[string]any{"id": id}, &d)
	return d, err
}

func (c *Client) Create(ctx context.Context, collectionID, parentID, title, text string) (Document, error) {
	body := map[string]any{"collectionId": collectionID, "title": Truncate(title), "text": text, "publish": true}
	if parentID != "" {
		body["parentDocumentId"] = parentID
	}
	var d Document
	err := c.call(ctx, "documents.create", body, &d)
	return d, err
}

func (c *Client) Update(ctx context.Context, id, title, text string) (Document, error) {
	var d Document
	err := c.call(ctx, "documents.update", map[string]any{"id": id, "title": Truncate(title), "text": text}, &d)
	return d, err
}

// Upload stores data as an attachment of documentID and returns the URL to embed.
func (c *Client) Upload(ctx context.Context, documentID, name, contentType string, data []byte) (string, error) {
	var res struct {
		UploadURL  string            `json:"uploadUrl"`
		Form       map[string]string `json:"form"`
		Attachment struct {
			URL string `json:"url"`
		} `json:"attachment"`
	}
	err := c.call(ctx, "attachments.create", map[string]any{
		"name": name, "contentType": contentType, "size": len(data),
		"documentId": documentID, "preset": "documentAttachment",
	}, &res)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range res.Form {
		if err := w.WriteField(k, v); err != nil {
			return "", err
		}
	}
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	fw.Write(data)
	if err := w.Close(); err != nil {
		return "", err
	}

	target := res.UploadURL
	local := strings.HasPrefix(target, "/")
	if local {
		target = c.base + target
	}
	req, err := http.NewRequestWithContext(ctx, "POST", target, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if local {
		// Outline's own storage needs the API key; S3-style presigned uploads must not get it.
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("upload %s: HTTP %d: %s", name, resp.StatusCode, raw)
	}
	return res.Attachment.URL, nil
}

// Truncate shortens s to Outline's title limit on a rune boundary.
func Truncate(s string) string {
	if utf8.RuneCountInString(s) <= MaxTitle {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:MaxTitle-1])) + "…"
}
