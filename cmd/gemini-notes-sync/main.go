// Command gemini-notes-sync copies "Notes by Gemini" documents from Google Drive
// into an Outline collection.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // IANA zones inside minimal container images

	"github.com/sarin/gemini-notes-sync/internal/config"
	"github.com/sarin/gemini-notes-sync/internal/gdrive"
	"github.com/sarin/gemini-notes-sync/internal/outline"
	"github.com/sarin/gemini-notes-sync/internal/store"
	"github.com/sarin/gemini-notes-sync/internal/syncer"
)

const usage = `usage: gemini-notes-sync [-config path] <command>

commands:
  run       sync now, then every sync.interval (default for the container)
  once      sync once and exit
  dry-run   show what would be written, without touching Outline
  check     verify Google and Outline access and the configuration
  healthcheck  exit 0 if a running instance reports healthy (for Docker)
`

func main() {
	configPath := flag.String("config", envOr("CONFIG", "config.yaml"), "path to config file")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "run"
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cmd == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := runCommand(ctx, cmd, *configPath, log); err != nil {
		log.Error(cmd+" failed", "err", err)
		os.Exit(1)
	}
}

func runCommand(ctx context.Context, cmd, configPath string, log *slog.Logger) error {
	switch cmd {
	case "run", "once", "dry-run", "check":
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	drv, err := gdrive.New(ctx, cfg.Google.CredentialsFile)
	if err != nil {
		return err
	}
	ol := outline.New(cfg.Outline.BaseURL, cfg.Outline.APIKey)

	if cmd == "check" {
		return check(ctx, cfg, drv, ol)
	}

	st, err := store.Open(cfg.Sync.StatePath)
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := syncer.New(ctx, cfg, drv, ol, st, log)
	if err != nil {
		return err
	}

	switch cmd {
	case "dry-run":
		return dryRun(ctx, s)
	case "once":
		return once(ctx, s, log, nil)
	}

	health := &healthState{}
	go serveHealth(ctx, cfg.Sync.HealthAddr, health, log)
	ticker := time.NewTicker(cfg.Sync.Interval)
	defer ticker.Stop()
	for {
		if err := once(ctx, s, log, health); err != nil && ctx.Err() == nil {
			log.Error("sync pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		case <-ticker.C:
		}
	}
}

func once(ctx context.Context, s *syncer.Syncer, log *slog.Logger, h *healthState) error {
	start := time.Now()
	res, err := s.Run(ctx)
	log.Info("sync pass done", "notes", res.Notes, "synced", res.Synced, "unchanged", res.Unchanged,
		"conflicts", res.Conflicts, "failed", res.Failed, "took", time.Since(start).Round(time.Millisecond))
	if h != nil {
		h.set(res, err)
	}
	if err == nil && res.Failed > 0 {
		err = fmt.Errorf("%d note(s) failed", res.Failed)
	}
	return err
}

func dryRun(ctx context.Context, s *syncer.Syncer) error {
	files, err := s.Notes(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d note(s) in scope\n", len(files))
	for _, f := range files {
		p, err := s.Prepare(ctx, f)
		if err != nil {
			fmt.Printf("\n✗ %s: %v\n", f.Name, err)
			continue
		}
		fmt.Printf("\n[%s] %s\n  → %s / %s\n", f.Source, f.Name, strings.Join(p.Path, " / "), p.Title)
		for _, key := range p.Order {
			fmt.Printf("      └ %s\n", p.Children[key])
		}
		images := fmt.Sprint(p.Images)
		if p.Images > 0 && f.MimeType == gdrive.DocMime {
			switch n, err := s.FullResImages(ctx, f.File); {
			case err != nil:
				images += fmt.Sprintf(" (full-res unavailable: %v)", err)
			case n == p.Images:
				images += " (full-res ✓)"
			default:
				images += fmt.Sprintf(" (full-res count %d ≠ export; export copies will be used)", n)
			}
		}
		fmt.Printf("  main section: %s · images: %s · attendees: %d\n", p.Main, images, len(p.Meeting.Attendees))
	}
	return nil
}

func check(ctx context.Context, cfg *config.Config, drv *gdrive.Client, ol *outline.Client) error {
	ok := true
	report := func(name string, err error, detail string) {
		if err != nil {
			ok = false
			fmt.Printf("✗ %-22s %v\n", name, err)
			return
		}
		fmt.Printf("✓ %-22s %s\n", name, detail)
	}

	id, err := drv.Identity(ctx)
	report("google credentials", err, id)
	roots, err := drv.Roots(ctx, cfg.Google.FolderIDs)
	if err == nil && len(roots) == 0 {
		err = fmt.Errorf("nothing shared with %s yet", id)
	}
	names := []string{}
	for _, n := range roots {
		names = append(names, n)
	}
	report("drive folders", err, strings.Join(names, ", "))
	if len(roots) > 0 {
		re := regexp.MustCompile(cfg.Google.NotesNamePattern) // validated by config.Load
		files, err := drv.Notes(ctx, roots, re)
		report("gemini notes", err, fmt.Sprintf("%d found", len(files)))
	}
	if len(cfg.Zoom.Folders) == 0 {
		fmt.Printf("- %-22s %s\n", "zoom folder", "not configured (zoom.folders)")
	} else {
		zroots, err := drv.ResolveFolders(ctx, cfg.Zoom.Folders)
		if err != nil {
			report("zoom folder", err, "")
		} else {
			var zn []string
			for _, n := range zroots {
				zn = append(zn, n)
			}
			files, err := drv.Walk(ctx, zroots, syncer.IsZoomFile)
			report("zoom folder", err, fmt.Sprintf("%s · %d file(s)", strings.Join(zn, ", "), len(files)))
		}
	}

	user, team, err := ol.AuthInfo(ctx)
	report("outline api key", err, fmt.Sprintf("%s (team %s)", user, team))
	col, err := ol.FindCollection(ctx, cfg.Outline.Collection)
	report("outline collection", err, col.Name)

	if !ok {
		return fmt.Errorf("checks failed")
	}
	return nil
}

type healthState struct {
	mu   sync.Mutex
	last time.Time
	res  syncer.Result
	err  error
}

func (h *healthState) set(res syncer.Result, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last, h.res, h.err = time.Now(), res, err
}

func serveHealth(ctx context.Context, addr string, h *healthState, log *slog.Logger) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		body := map[string]any{"last_sync": h.last, "result": h.res}
		if h.err != nil {
			body["error"] = h.err.Error()
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("health server", "err", err)
	}
}

func healthcheck() int {
	addr := envOr("HEALTH_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
