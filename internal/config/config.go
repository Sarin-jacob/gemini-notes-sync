// Package config loads the YAML configuration, expanding ${VAR} references from
// the environment (and from an optional .env file next to it).
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Google  Google  `yaml:"google"`
	Outline Outline `yaml:"outline"`
	Sync    Sync    `yaml:"sync"`
	Layout  Layout  `yaml:"layout"`
	Content Content `yaml:"content"`
}

type Google struct {
	CredentialsFile  string   `yaml:"credentials_file"`
	FolderIDs        []string `yaml:"folder_ids"`         // empty: every folder shared with the service account
	NotesNamePattern string   `yaml:"notes_name_pattern"` // regex on the Drive file name
}

type Outline struct {
	BaseURL    string `yaml:"base_url"`
	APIKey     string `yaml:"api_key"`
	Collection string `yaml:"collection"` // name or ID
}

type Sync struct {
	Interval       time.Duration `yaml:"interval"`
	Lookback       time.Duration `yaml:"lookback"`        // 0: no limit
	ConflictPolicy string        `yaml:"conflict_policy"` // skip | overwrite
	StatePath      string        `yaml:"state_path"`
	Timezone       string        `yaml:"timezone"` // IANA name used to read meeting times from file names
	HealthAddr     string        `yaml:"health_addr"`
}

type Layout struct {
	Path       string `yaml:"path"`        // template; "/" separates container documents
	Title      string `yaml:"title"`       // template for the main meeting document
	ChildTitle string `yaml:"child_title"` // template for Full notes / Transcript documents
}

type Content struct {
	Main      string   `yaml:"main"`       // tab used as the meeting document
	Children  []string `yaml:"children"`   // tabs written as nested documents, in order
	DropLines []string `yaml:"drop_lines"` // extra regexes; matching lines are removed
}

const (
	DefaultPath       = `{{ .Date | date "2006" }}/{{ .Date | date "01 January" }}`
	DefaultTitle      = `{{ .Date | date "2006-01-02" }} — {{ if .Untitled }}Meeting at {{ .Date | date "15:04" }}{{ else }}{{ .Title }}{{ end }}`
	DefaultChildTitle = `{{ .Tab }} — {{ .MainTitle }}`
)

func defaults() Config {
	return Config{
		Google: Google{
			CredentialsFile:  "key.json",
			NotesNamePattern: `(?i)notes by gemini`,
		},
		Sync: Sync{
			Interval:       10 * time.Minute,
			ConflictPolicy: "skip",
			StatePath:      "data/state.db",
			Timezone:       "Local",
			HealthAddr:     ":8080",
		},
		Layout:  Layout{Path: DefaultPath, Title: DefaultTitle, ChildTitle: DefaultChildTitle},
		Content: Content{Main: "quick_notes", Children: []string{"full_notes", "transcript"}},
	}
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads path, loading <dir>/.env first (without overriding real env vars).
func Load(path string) (*Config, error) {
	if err := loadDotEnv(filepath.Join(filepath.Dir(path), ".env")); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var missing []string
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines[i] = envRef.ReplaceAllStringFunc(line, func(m string) string {
			name := envRef.FindStringSubmatch(m)[1]
			v, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
	}
	expanded := strings.Join(lines, "\n")
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	cfg := defaults()
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, cfg.validate()
}

func (c *Config) validate() error {
	var errs []error
	if c.Outline.BaseURL == "" {
		errs = append(errs, errors.New("outline.base_url is required"))
	}
	c.Outline.BaseURL = strings.TrimRight(c.Outline.BaseURL, "/")
	if c.Outline.APIKey == "" {
		errs = append(errs, errors.New("outline.api_key is required"))
	}
	if c.Outline.Collection == "" {
		errs = append(errs, errors.New("outline.collection is required"))
	}
	if _, err := regexp.Compile(c.Google.NotesNamePattern); err != nil {
		errs = append(errs, fmt.Errorf("google.notes_name_pattern: %w", err))
	}
	for _, p := range c.Content.DropLines {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("content.drop_lines %q: %w", p, err))
		}
	}
	if c.Sync.ConflictPolicy != "skip" && c.Sync.ConflictPolicy != "overwrite" {
		errs = append(errs, fmt.Errorf("sync.conflict_policy must be skip or overwrite, got %q", c.Sync.ConflictPolicy))
	}
	if _, err := c.Location(); err != nil {
		errs = append(errs, fmt.Errorf("sync.timezone: %w", err))
	}
	if c.Sync.Interval < time.Minute {
		errs = append(errs, errors.New("sync.interval must be at least 1m"))
	}
	return errors.Join(errs...)
}

func (c *Config) Location() (*time.Location, error) {
	return time.LoadLocation(c.Sync.Timezone)
}

// loadDotEnv sets KEY=VALUE pairs from path for keys not already in the environment.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		k = strings.TrimSpace(k)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	return sc.Err()
}
