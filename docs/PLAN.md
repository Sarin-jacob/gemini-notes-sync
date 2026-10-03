# gemini-notes-sync — Feasibility & Plan

A Dockerised Go service that watches the Google Drive **Meet Recordings** folder, takes the
Gemini "Notes by Gemini" docs (and optionally transcripts/recordings), cleans them up, and
publishes them into an **Outline** collection with a user-defined hierarchy, naming and
cross-links.

---

## 1. Feasibility

**Verdict: feasible.** Both ends have stable, documented APIs that cover what we need.
The main risks are the content format (Gemini's doc layout changes over time) and
authentication ergonomics inside Docker, both manageable.

### 1.1 Source: Google Drive / Docs

| Need | How | Notes |
|---|---|---|
| Find the folder | Drive v3 `files.list` with `name = 'Meet Recordings' and mimeType = folder`, or configured folder ID | Configured ID is more robust (folder name is localised). |
| List notes | `files.list` with `'<folderId>' in parents and trashed = false`, filter on `mimeType = application/vnd.google-apps.document` and name pattern `… - Notes by Gemini` | Name pattern also localised → make it configurable. |
| Detect new/changed | Either poll `modifiedTime > lastSync`, or the Drive **Changes API** (`changes.getStartPageToken` / `changes.list`) | Polling is simpler and fine for this volume; Changes API is an optimisation for later. |
| Get content | **Option A:** `files.export` with `mimeType=text/markdown` (supported for Google Docs). **Option B:** Docs API `documents.get?includeTabsContent=true` → our own Markdown renderer | Newer Gemini docs put **Notes** and **Transcript** in separate *tabs* of one doc. Need to confirm what the Markdown export does with tabs (spike S1). Option B gives full control (per-tab, person/date smart chips, links) at the cost of writing a converter. |
| Related files | Transcript doc / recording video in the same folder, linked from the notes doc's header | Linked by Drive URL in the doc body, and by matching title + timestamp. |

**Auth — decision: service account, folder shared as Viewer**

The service authenticates as a service account (SA) and can read **only** what the user
shares with it: the Meet Recordings folder, as Viewer. New files Gemini creates in the
folder inherit that permission, so sync keeps working without further action. No
interactive login, no token expiry, no access to the rest of the user's Drive.

- Cost: free. A Cloud project, a service account, its key and the Drive API need no
  billing account; Drive API usage is free within quota (far above our needs).
- Setup (documented in README):
  1. Create a Cloud project → enable **Google Drive API** (+ Docs API if spike S1 picks Option B).
  2. Create a service account (no roles needed) → create a JSON key → put it in `/data`.
  3. In Drive, share the Meet Recordings folder with the SA e-mail as **Viewer**.
- Scope requested by the client: `drive.readonly` — effective access is still limited to
  what was shared with the SA.
- Caveats:
  - Workspace orgs created after ~2024 enforce `iam.disableServiceAccountKeyCreation`
    by default; an admin would need to lift it for this project. Not an issue for
    personal accounts.
  - Workspace admins may block sharing outside the domain (SA e-mails are
    `*.iam.gserviceaccount.com`).
  - The JSON key is a secret: mount read-only, never bake into the image.
- Fallback (not built unless needed): OAuth user consent with `drive.readonly`.

### 1.2 Destination: Outline

Outline's API (`POST {base}/api/<method>`, `Authorization: Bearer <api key>`) covers:

| Need | Endpoint |
|---|---|
| Resolve collection | `collections.list` / `collections.info` |
| Existing tree | `collections.documents` (returns nested tree) |
| Create | `documents.create` (`title`, `text` markdown, `collectionId`, `parentDocumentId`, `publish`) |
| Update | `documents.update` (`id`, `title`, `text`) |
| Move/re-parent | `documents.move` |
| Lookup | `documents.info`, `documents.search` |

- **Nesting:** Outline has no folders — hierarchy = parent documents. Path segments from
  the template become "container" documents that we create on demand (idempotently).
- **Linking:** docs link to each other with `/doc/<slug>-<urlId>` URLs. Outline renders
  backlinks automatically, so "previous/next meeting" and "series index" links come almost
  free once we know the target IDs (two-pass write where needed).
- **Limits to keep in mind:** title max length (~100 chars → truncate in templates),
  API rate limits (configurable on self-hosted; implement back-off on 429), very long
  transcripts (keep transcripts as a separate child doc or skip them by default).

### 1.3 Risks & mitigations

| Risk | Mitigation |
|---|---|
| Gemini changes its doc layout / wording; localisation | Cleanup is rule-based and configurable (regex + section rules), with a shipped default profile and golden-file tests from real samples. |
| User edits the note in Outline, then source changes | Store a hash of what *we* last wrote; if Outline content differs from it, apply `conflict_policy` (`skip` default, `overwrite`, `append_update`). |
| User renames/moves the doc in Outline | We track by Outline doc ID, not by path; `respect_manual_moves: true` by default. |
| SA key leaked | Key mounted read-only from `/data`, never in image or logs; SA only has Viewer on one folder, so blast radius is that folder. Rotate by creating a new key. |
| Duplicates after state loss | Embed a hidden marker (Drive file ID) in each doc and re-adopt via `documents.search` on rebuild. |

---

## 1.4 Spike S1 findings (2026-10-01, 4 real notes)

- **Location:** notes live in *two* trees — `Meet Recordings/` (flat) and
  `Google Meet/<meet-code> - <yyyy/mm/dd HH:MM TZ>/` (per-meeting subfolder, newer).
  → Sync must walk all shared roots **recursively**; folder layout is not a reliable signal.
- **Naming:** `<Title> - <yyyy/mm/dd HH:MM TZ> - Notes by Gemini`; ad-hoc meetings are
  titled `Meeting started <yyyy/mm/dd HH:MM TZ>`. Meeting code available from the
  subfolder name when present.
- **One doc, three tabs:** `Quick notes`, `Full notes`, `Transcript` (no separate transcript file).
- **Drive Markdown export works and includes all tabs**, each as an H1 (`# **✍️ Quick notes**`,
  `# **📝 Full notes**`, `# **📖 Transcript**`). Docs API with `includeTabsContent` also works.
  **Decision: Option A** (Markdown export, split on tab H1s); Docs API kept as fallback.
- **Images:** Full notes "Details" embeds screen captures as base64 data URIs in
  reference-style links (`![][image1]` … `[image1]: <data:image/png;base64,…>`), making
  exports 1–1.5 MB. → Extract, upload via Outline `attachments.create`, rewrite refs.
- **Quick notes ≈ condensed duplicate of Full notes** → make tab selection configurable
  (default: Full notes; Quick notes optional; Transcript off / child doc).
- **Noise to strip (default profile):**
  - `*Please rate the new Quick notes tab … short survey*`
  - `*You should review Gemini's notes … Get tips and learn how Gemini takes notes*`
  - `*How is the quality of these specific notes? Take a short survey*`
  - `*This editable transcript was computer generated …*`
  - Bold wrapping inside headings (`## **Summary**`), empty headings (`## `), `\-` escapes,
    transcript anchors `{#00:01:48}`, emoji prefixes on tab titles, odd `## Aligned`
    level under `### Decisions`.
- **Useful metadata in the header block** (to lift into our info block, not keep raw):
  `Invited` (names + mailto), `Attachments` (Calendar event link → `eid` gives a stable
  event/series id usable for grouping recurring meetings), `Meeting records` (transcript link).
- Next steps are task-list items: `- [ ] \[Names\] Title: description` → keep as Outline checklists.

## 1.5 Spike S2 findings (2026-10-01, self-hosted Outline at docs.jell0.online)

All checks passed (`cmd/outlinespike`):

- **Nesting:** `documents.create` with `parentDocumentId` builds arbitrary depth (root → year → month → note).
- **Title limit:** hard **100 characters** (HTTP 400 `validation_error` above that) → titles
  are truncated at render time, rune-safe, with an ellipsis.
- **Links:** `documents.create` returns a relative `url` (`/doc/<slug>-<urlId>`); linking
  with it creates a real backlink (`documents.list?backlinkDocumentId=` confirmed).
- **Move:** `documents.move` re-parents without changing the URL → links survive layout changes.
- **Update:** `documents.update` replaces text in place.
- **Images:** `attachments.create` (preset `documentAttachment`) → multipart POST to the
  returned `uploadUrl` (relative on this instance = local storage, needs the Bearer header)
  → embed `![](/api/attachments.redirect?id=…)`; round-trip verified byte-for-byte.
- **Long text:** ~78 KB transcripts accepted; Outline normalises a few characters, so the
  stored text ≠ sent text → conflict detection must compare against **what Outline returns
  after the write**, not what we sent.

## 1.6 Decisions (2026-10-01)

| Topic | Decision |
|---|---|
| Auth | Service account, Viewer on `Meet Recordings` + `Google Meet`, walked recursively |
| Content | **Format:** Drive Markdown export, split per tab |
| Document shape | **Quick notes = the main document.** Full notes and Transcript become **child documents** of it, linked from its header |
| Hierarchy | `Year / Month / <note>` by default (e.g. `2026 / 09 September / 2026-09-04 — Tata Hitachi - Documentation`) |
| Conflicts | Keep Outline edits: if the Outline doc differs from what we last wrote, skip and log |

| Images | **Fetched in full resolution through the Docs API**, not taken from the Markdown export. The export shrinks screenshots to their on-page size (653×367 PNG), while `inlineObjects[].imageProperties.contentUri` returns the original (1280×720 JPEG, which is also smaller in bytes). The nth `![][imageN]` in the export maps to the nth `inlineObjectElement` in the tab body. `contentUri` is a short-lived signed URL, so it is fetched right after `documents.get`. |
| Folder documents | Every container document (year, month, series…) that exists only to hold children gets an **auto-generated index** as its body. The index is a list of links to its children, newest first, each with date and title. It is regenerated whenever a child is added, renamed or moved. If the user writes their own content in a container, we keep it and only replace the text between our `<!-- index -->` markers. Fallback when markers don't survive Outline's storage: a fixed `## Index` heading section. Spike in Phase 1. |

| Zoom (2026-10-04) | Zoom does not save AI Companion summaries to Drive (they live in Zoom, are e-mailed to the host, and expire after 90 days by default; export is Word or PDF; the summaries API needs an admin S2S app and is unreliable). So: a **Drive drop folder** shared with the SA (`zoom.folders`), accepting .docx (own converter, `internal/docx`), .md/.txt and Google Docs. Same Year/Month tree, header says "Source: Zoom". Files stay in place; a re-upload (new Drive ID, same title + start) reuses the meeting's documents. Parser (`internal/zoom`) is heuristic until a real export is available. |

Resulting tree per meeting:

```
2026/
  09 September/
    2026-09-04 — Tata Hitachi - Documentation      ← Quick notes + info block
      Full notes                                   ← summary, decisions, details, screenshots
      Transcript
```

---

## 2. Feature spec

### 2.1 Sync
- Poll interval (default 10 min) + `--once` mode for cron/one-shot.
- Optional lookback window (e.g. only docs from the last N days) and a backfill mode.
- Idempotent: re-running never creates duplicates.
- Handles: new doc, updated doc (Gemini sometimes writes notes minutes after the meeting),
  renamed doc, trashed doc (`on_delete: ignore | archive | delete`).
- Dry-run mode printing the planned Outline tree + diffs.

### 2.2 Cleanup ("remove unwanted stuff")
Pipeline of ordered, configurable transforms applied to an intermediate Markdown AST
(goldmark), not raw strings:

- **Drop sections** by heading (e.g. "Suggested next steps" kept, footer removed).
- **Drop boilerplate lines** by regex — defaults include Gemini's disclaimers such as
  "You should review Gemini's notes…", "Please provide feedback about using Gemini…",
  "Get tips and learn how Gemini takes notes".
- **Strip header noise**: invited list, attachments block, "Meeting records" block —
  each individually toggleable; useful bits (attendees, date) are lifted into a
  front-matter-style info block instead.
- **Normalise**: heading levels, empty bullets, duplicate blank lines, Google redirect
  URLs (`google.com/url?q=` → real URL), smart-chip artefacts.
- **Transcript handling**: `omit | child_document | link_only`.
- User-defined extra rules in config.

### 2.3 Structure & naming templates
Go `text/template` with a curated function set, evaluated against a metadata object:

```
.Title        meeting title (with " - Notes by Gemini" and timestamp stripped)
.RawTitle     original Drive file name
.Date         time.Time of meeting (parsed from title, fallback createdTime)
.Series       normalised title used to group recurring meetings
.Attendees    []string
.DriveID, .DriveURL, .TranscriptURL, .RecordingURL
```

Functions: `date "2006-01"`, `monthName`, `isoWeek`, `slug`, `trunc 80`, `lower`, `default`,
`regexReplace`.

Examples:

```yaml
path: "{{ .Date | date \"2006\" }}/{{ .Date | date \"01 January\" }}"
title: "{{ .Date | date \"2006-01-02\" }} — {{ .Title }}"
```

```yaml
path: "Meetings/{{ .Series }}"        # one parent doc per recurring meeting
title: "{{ .Date | date \"Jan 2\" }}"
```

Plus **routing rules**: first-match list of `{ match: <regex on title/attendee>, path, title, collection }`
so e.g. "1:1 …" meetings go to a different subtree or collection.

### 2.4 Linking
- Header block in each note: date, attendees, links to Drive original, transcript, recording.
- **Series linking**: container doc per series gets an auto-maintained index list;
  each note gets "← previous / next →" links (updated when a new instance arrives).
- Optional transcript child doc linked from the note.
- Outline backlinks do the rest.

---

## 3. Architecture

```
            +-----------+     +-----------+     +------------+     +-----------+
 Drive  --> | Source    | --> | Transform | --> | Planner    | --> | Outline   | --> Outline
 (poll)     | (list,    |     | (md AST,  |     | (template, |     | Sink      |
            |  export)  |     |  rules)   |     |  diff vs   |     | (create/  |
            +-----------+     +-----------+     |  state)    |     |  update)  |
                                                +------------+     +-----------+
                                                       ^ |
                                                       | v
                                                  SQLite state
```

### 3.1 Repo layout

```
cmd/gemini-notes-sync/main.go       CLI: run | once | check | dry-run | reset
internal/config/                    YAML + env loading, validation
internal/drive/                     service-account auth, list, export
internal/gemini/                    parse Gemini doc: title/date, sections, metadata
internal/transform/                 markdown AST + cleanup rules
internal/layout/                    templates, routing rules, path resolution
internal/outline/                   thin typed API client w/ retry + rate-limit
internal/store/                     SQLite (modernc.org/sqlite, CGO-free)
internal/syncer/                    orchestration, conflict policy, linking pass
testdata/                           anonymised real Gemini docs + golden outputs
Dockerfile, docker-compose.yml, config.example.yaml
```

### 3.2 State (SQLite, in `/data`)

```
docs(drive_id PK, outline_id, outline_url_id, series, meeting_time,
     drive_modified, source_hash, written_hash, path, status, updated_at)
containers(path PK, outline_id, index_hash)   -- template-created parent docs + hash of last written index
kv(key PK, value)                        -- page tokens, schema version
```

### 3.3 Key libraries
- `google.golang.org/api/drive/v3`, `docs/v1`, `golang.org/x/oauth2`
- `github.com/yuin/goldmark` (parse) + a small Markdown renderer for the AST
- `modernc.org/sqlite` (no CGO → static binary, `distroless/static` image)
- `gopkg.in/yaml.v3`, `log/slog`
- Outline: hand-written client (small surface, no maintained Go SDK needed)

### 3.4 Config sketch

```yaml
google:
  credentials_file: /data/service-account.json   # mounted read-only
  folder_ids: []          # empty = every folder shared with the SA, walked recursively
  notes_name_pattern: "(?i)notes by gemini"
outline:
  base_url: https://outline.example.com
  api_key: ${OUTLINE_API_KEY}
  collection: "Meetings"
sync:
  interval: 10m
  lookback: 720h
  conflict_policy: skip  # skip | overwrite | append_update
  on_delete: ignore
layout:
  path: "{{ .Date | date \"2006\" }}/{{ .Date | date \"01 January\" }}"
  title: "{{ .Date | date \"2006-01-02\" }} — {{ .Title }}"
  routes:
    - match: { title: "(?i)^1:1" }
      path: "1:1s/{{ .Series }}"
cleanup:
  profile: gemini-default
  tabs:
    main: quick_notes                       # becomes the meeting document
    children: [full_notes, transcript]       # nested under it, in this order
  drop_sections: ["Meeting records"]
  drop_lines: []
linking:
  series: true
  prev_next: true
```

### 3.5 Docker
- Multi-stage build → `gcr.io/distroless/static:nonroot`, ~15 MB.
- `/data` volume: config, service-account key (read-only), SQLite.
- `docker compose run --rm sync check` verifies the key, folder access and Outline token.
- Healthcheck via `/healthz` on a small HTTP server (also exposes `/metrics` later).

---

## 4. Milestones

**Phase 0 — Spikes (de-risk, ~1–2 days)**
- S1: ✅ done — see §1.4.
- S2: ✅ done — see §1.5.
- S3: Confirm the SA can see the shared folder and that newly created notes inherit access (create a test meeting).

**Phase 1 — MVP** (implemented 2026-10-01; awaiting first live sync)
- Config, service-account auth, Drive listing + export, default cleanup profile,
  path/title templates, container docs with auto-index, full-res image upload, create/update with state, `once` + loop, Dockerfile.

**Phase 2 — Quality**
- Routing rules, conflict policy, series index + prev/next links, transcript modes,
  dry-run diff output, golden tests for cleanup.

**Phase 3 — Polish**
- Drive Changes API, on-delete handling, state rebuild from Outline markers,
  healthz/metrics, README with setup guides for service-account setup.

---

## 5. Open questions for the user

1. Google account type: personal (Google One AI Premium) or Workspace? Are you an admin?
2. Self-hosted Outline or getoutline.com? Version?
3. Transcripts: skip, link, or import as child docs?
4. When a note was edited in Outline and the Drive doc changes — whose edits win?
5. Preferred default hierarchy (by date, by series, or both via routes)?
6. Only your own meetings, or also notes shared to you by others (they don't live in
   your Meet Recordings folder)?
