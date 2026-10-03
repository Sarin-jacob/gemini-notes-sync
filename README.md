# gemini-notes-sync

Copies Google Meet **"Notes by Gemini"** documents, and **Zoom AI Companion**
summaries you drop into a Drive folder, into an
[Outline](https://www.getoutline.com) collection, cleaned up and organised:

```
gemini-notes (collection)
└─ 2026                                   ← auto index of its months
   └─ 09 September                        ← auto index of its meetings
      └─ 2026-09-04 — Project Kickoff     ← Quick notes + when/attendees/links
         ├─ Full notes — …                ← summary, decisions, details, screenshots
         └─ Transcript — …
```

- Removes Gemini's surveys, disclaimers and header noise; fixes heading levels.
- Screenshots are uploaded at full resolution (1280×720) rather than the export's thumbnails.
- Transcript timestamps in the notes link to the Transcript document.
- Folder documents get an index of their children, newest first.
- Never overwrites a document you edited in Outline (`conflict_policy: skip`).
- Path and titles are Go templates — see `config.example.yaml`.

## Setup

### 1. Google service account (free, read-only on one folder)

1. In [Google Cloud Console](https://console.cloud.google.com) create a project.
2. Enable the **Google Drive API** and **Google Docs API**.
3. IAM → Service accounts → create one (no roles) → Keys → Add key → JSON.
   Save it as `key.json` in this directory.
4. In Google Drive, share the **Meet Recordings** and **Google Meet** folders with the
   service account's e-mail (`…@….iam.gserviceaccount.com`) as **Viewer**.

The service account can only see what you share with it.

### 2. Outline

Create an API key (Settings → API) and a collection for the notes.

### 3. Configure

```bash
cp config.example.yaml config.yaml
```

Create `.env`:

```
OUTLINE_URL=https://outline.example.com
OUTLINE_API_KEY=ol_api_…
OUTLINE_COLLECTION=gemini-notes
```

Set `sync.timezone` in `config.yaml` to the zone your meeting times are in.

### 4. Run

```bash
docker compose run --rm sync check
```

```bash
docker compose run --rm sync dry-run
```

```bash
docker compose up -d
```

Or without Docker:

```bash
go run ./cmd/gemini-notes-sync once
```

### 5. Zoom summaries (optional)

Zoom keeps AI Companion summaries in Zoom (and deletes them after 90 days by
default); it does not save them to Drive. To sync them:

1. Create a Drive folder, e.g. **Zoom Notes**, and share it with the service
   account as **Viewer**.
2. Set `zoom.folders: ["Zoom Notes"]` in `config.yaml`.
3. After a meeting, open the summary in Zoom Docs → **…** → **Export** → **Word**,
   and upload the file to that folder (`.md`, `.txt` and Google Docs work too).

Zoom notes land in the same tree as Gemini notes, marked "Source: Zoom". The
title and date are read from the summary ("Meeting summary for …", or the first
heading), falling back to the file name and upload time. A "Transcript" section,
if present, becomes a child document. Uploading a summary again updates the
existing documents instead of creating new ones. If your Zoom writes dates
day-first, set `zoom.date_order: dmy`.

## Commands

| Command | |
|---|---|
| `run` | Sync now and then every `sync.interval` (container default). Serves `GET /healthz`. |
| `once` | One sync pass. |
| `dry-run` | Show the planned tree, titles and image status; writes nothing. |
| `check` | Verify Google credentials, shared folders, Outline key and collection. |

## How updates work

State is kept in `data/state.db`. A note is re-processed when its Drive
`modifiedTime` changes. Before updating an Outline document the current text is
compared with what was last written; if you changed it, the document is left alone
and a warning is logged. Deleting a document in Outline makes the next change to
its note recreate it. Changing `layout.path` only affects new notes; existing
documents are not moved.
