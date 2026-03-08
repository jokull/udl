# Web UI Gaps & Roadmap

Audit of CLI functionality vs web UI at `udl.plex.uno`. Updated 2026-03-06.

## CLI → Web UI Coverage Matrix

| CLI Command | Web Route | Status |
|---|---|---|
| `udl status` | `GET /status` | Done |
| `udl movie search` | `GET /add?q=...` | Done |
| `udl movie add` | `POST /add/movie/{tmdbID}` | Done |
| `udl movie list` | `GET /movies` | Done |
| `udl movie releases` | `GET /releases/movie/{id}` | Done |
| `udl movie grab` | `POST /releases/movie/{id}/grab/{index}` | Done |
| `udl movie info` | — | **Missing** |
| `udl movie remove` | `POST /wanted/remove/movie/{id}` | Partial (wanted page only, no /movies action) |
| `udl movie delete` | — | **Missing** |
| `udl tv search` | `GET /add?q=...` | Done |
| `udl tv add` | `POST /add/tv/{tmdbID}` | Done |
| `udl tv list` | `GET /series` | Done |
| `udl tv episodes` | `GET /series/{id}` | Done |
| `udl tv releases` | `GET /releases/episode/{id}` | Done |
| `udl tv grab` | `POST /releases/episode/{id}/grab/{index}` | Done |
| `udl tv info` | — | **Missing** |
| `udl tv remove` | — | **Missing** |
| `udl tv delete` | — | **Missing** |
| `udl tv refresh` | — | **Missing** |
| `udl tv monitor` | `POST /series/{id}/season/{season}/toggle` | Done (toggle only, no --all/--none/--latest) |
| `udl queue` | `GET /` | Done (SSE live) |
| `udl queue pause` | `POST /queue/pause` | Done |
| `udl queue resume` | `POST /queue/resume` | Done |
| `udl queue retry` | `POST /queue/retry/{category}/{id}` | Done |
| `udl queue evict` | `POST /queue/evict/{category}/{id}` | Done |
| `udl queue clear` | — | **Missing** |
| `udl wanted` | `GET /wanted` | Done |
| `udl schedule` | `GET /schedule` | Done |
| `udl history` | `GET /history` | Partial (no filters: --type, --event, --tmdb, --limit) |
| `udl search-trigger` | `POST /wanted/search/{category}/{id}` | Partial (per-item + search-all, no by-title) |
| `udl blocklist` | — | **Missing** |
| `udl blocklist remove` | — | **Missing** |
| `udl blocklist clear` | — | **Missing** |
| `udl plex servers` | — | **Missing** |
| `udl plex check` | (inline in `/releases` page) | Partial (shows in release search, no standalone) |
| `udl plex cleanup` | — | **Missing** |
| `udl library import` | — | **Missing** |
| `udl library cleanup` | — | **Missing** |
| `udl library verify` | — | **Missing** |
| `udl library prune` | — | **Missing** |
| `udl library prune-incomplete` | — | **Missing** |
| `udl config show` | `GET /status` (partial) | Done (shows profile, indexers, providers) |
| `udl config check` | — | **Missing** |
| `udl migrate radarr/sonarr` | — | N/A (one-time, CLI-only is fine) |

## Summary: 22 Done/Partial, 16 Missing

---

## Missing Flows (Ranked by Impact)

### 1. Movie & Series Remove/Delete (High — common maintenance)

**CLI:** `udl movie remove`, `udl movie delete`, `udl tv remove`, `udl tv delete`

These are the most-used maintenance actions with no web equivalent.

**Web UI needs:**

**Movie remove** — button on `/movies` page rows and on movie detail page:
- "Remove" drops from DB (optionally keeps files via checkbox)
- Confirmation with movie title

**Movie delete** — button on movie detail / movies page for downloaded items:
- "Delete file & re-search" resets to wanted, blocklists old NZB, triggers search
- "Delete file only" resets to wanted without searching
- Show file path + size in confirmation
- RPC: `Service.MovieDelete` (already exists)

**Series remove** — button on `/series` list and `/series/{id}`:
- Removes series from monitoring
- RPC: `Service.RemoveSeries` (already exists)

**TV delete** — per-episode and per-season on `/series/{id}`:
- Delete episode file, reset to wanted
- Season-level bulk delete
- Optional re-search checkbox
- RPC: `Service.TVDelete` (already exists, supports --season/--episode)

### 2. Movie & Series Detail Pages (High — enriches existing pages)

**CLI:** `udl movie info`, `udl tv info`

**Web UI needs: `/movies/{id}` and enhanced `/series/{id}`**

**Movie detail page:**
- TMDB poster, overview, genres, runtime, rating, original language
- Current quality vs preferred quality badge
- File path and size (if downloaded)
- Download history for this item (filter by TMDB ID)
- Blocklisted releases for this item
- Active download progress inline
- Actions: Delete file, Search indexers, Browse releases, Remove movie
- Link to TMDB page

**Series detail enhancements:**
- Already has season/episode view — add:
- Per-episode "Browse releases" button (links to `/releases/episode/{id}`)
- Per-episode "Delete file" button for downloaded episodes
- Series-level stats: X/Y episodes downloaded, disk usage
- "Remove series" button
- "Refresh metadata" button (→ `Service.RefreshSeries`)
- Download history filtered to this series

### 3. Blocklist Management (Medium — needed when releases keep failing)

**CLI:** `udl blocklist`, `udl blocklist remove <id>`, `udl blocklist clear`

**Web UI needs: `/blocklist` page**
- Table: Release Title, Media (linked to movie/series), Reason, Date
- "Remove" button per entry (allows re-grabbing that release)
- "Clear All" button with confirmation
- Filter by media type (movie/episode)
- Entry point: link from movie/series detail pages to filtered view
- RPC: `Service.Blocklist`, `Service.BlocklistRemove`, `Service.BlocklistClear` (all exist)

### 4. Queue Clear (Medium — operational)

**CLI:** `udl queue clear [--unmonitored]`

**Web UI needs:**
- "Clear All" button on queue page with confirmation dialog
- Checkbox option: "Only unmonitored episodes"
- RPC: `Service.ClearQueue` (already exists)

### 5. History Filters (Medium — useful for debugging)

**CLI:** `udl history --type movie --event failed --tmdb 12345 --limit 100`

**Web UI needs: filter controls on `/history`**
- Dropdown: All / Movies / Episodes
- Dropdown: All / Grabbed / Completed / Failed
- Optional: TMDB ID search box
- Pagination or "load more"
- RPC: `Service.History` already supports all these filters

### 6. TV Monitor Bulk Actions (Low — convenience)

**CLI:** `udl tv monitor --all`, `udl tv monitor --none`, `udl tv monitor --latest`

**Web UI currently:** toggle individual seasons only

**Web UI needs:** on `/series/{id}`:
- "Monitor All Seasons" button
- "Monitor Latest Only" button
- "Unmonitor All" button
- RPC: `Service.MonitorSeason` already exists with `Mode` field (all/none/latest/season)

### 7. TV Refresh (Low — occasional)

**CLI:** `udl tv refresh`

**Web UI needs:**
- "Refresh All Metadata" button on `/series` list page
- "Refresh" button on individual `/series/{id}` page
- RPC: `Service.RefreshSeries` (already exists, refreshes all)

### 8. Plex Integration Pages (Low — specialized)

**CLI:** `udl plex servers`, `udl plex check`, `udl plex cleanup`

**Plex check** is already shown inline on the releases page (shows Plex hit with server name + quality). Standalone check not needed.

**Web UI needs:**
- `/plex/cleanup` page: table of unwatched media older than N days, dry-run view, "Execute" button
- Plex server list on `/status` page
- RPC: `Service.PlexServers`, `Service.PlexCleanup` (both exist)

### 9. Library Management (Low — occasional maintenance)

**CLI:** `udl library import/cleanup/verify/prune/prune-incomplete`

These are rare maintenance operations. CLI is fine for most cases.

**Web UI would need: `/library` page with tabs**
- Verify: read-only consistency check, show missing/orphan/misnamed files
- Prune: list unmonitored episode files, execute deletion
- Import: scan directory, show matches, execute import
- Cleanup: show orphans/misnamed, fix actions
- All dry-run first, then "Execute" button
- RPC: `Service.LibraryPrune` exists; others would need new RPCs

### 10. Config Validation (Low — rare)

**CLI:** `udl config check`

**Web UI:** `/status` already shows active config. Could add a "Validate" button that runs config check and shows errors/warnings. Low priority — config rarely changes.

---

## Already Implemented (for reference)

| Feature | Route | Notes |
|---|---|---|
| Queue (live) | `GET /` | SSE progress, pause/resume, retry, evict |
| Movies list | `GET /movies` | Status filter tabs |
| Series list | `GET /series` | Links to detail pages |
| Series detail | `GET /series/{id}` | Episode table, season monitor toggle |
| Wanted | `GET /wanted` | Per-item search, batch search, remove, movie/episode filter |
| Schedule | `GET /schedule` | Upcoming episodes grouped by air date |
| History | `GET /history` | Last 50 events |
| Add media | `GET /add` | TMDB search for movies + TV, add button |
| Release browser | `GET /releases/{category}/{id}` | Scored releases, Plex hits, grab button |
| LLM pick | `GET /releases/{category}/{id}/llm` | SSE-streamed recommendation |
| Grab from Plex | `POST /releases/{category}/{id}/grab-plex` | Grabs from friend's server |
| Status dashboard | `GET /status` | Health checks, config, library stats |

---

## Implementation Priority

### Phase 1 — Daily Use Gaps
1. **Movie/Series Remove & Delete** — most common missing maintenance action
2. **Movie Detail Page** — `/movies/{id}` with poster, metadata, actions
3. **Series Detail Enhancements** — per-episode releases/delete buttons
4. **Blocklist Management** — `/blocklist` page

### Phase 2 — Operational Polish
5. **Queue Clear** — button + confirmation
6. **History Filters** — dropdowns on existing page
7. **TV Monitor Bulk Actions** — all/none/latest buttons
8. **TV Refresh** — button on series pages

### Phase 3 — Advanced (CLI is fine)
9. **Plex Cleanup** — dry-run + execute page
10. **Library Management** — verify/prune/import tabs
11. **Config Validation** — validate button on status page

---

## Technical Notes

### RPC Methods Available but Not Wired to Web

```
Service.MovieDelete        → movie delete + re-search
Service.TVDelete           → episode/season delete + re-search
Service.RemoveMovie        → drop movie from DB
Service.RemoveSeries       → drop series from DB
Service.Blocklist          → list blocklisted releases
Service.BlocklistRemove    → remove single entry
Service.BlocklistClear     → clear all entries
Service.ClearQueue         → clear queued/downloading items
Service.RefreshSeries      → refresh TMDB metadata for all series
Service.MovieInfo          → full movie details with history + blocklist
Service.SeriesInfo         → full series details with episode stats
Service.PlexServers        → list friend servers
Service.PlexCleanup        → unwatched media cleanup
Service.LibraryPrune       → delete files for unmonitored episodes
Service.History (filters)  → type/event/tmdb/limit filtering
Service.MonitorSeason      → all/none/latest modes (web only uses toggle)
```

All Phase 1 and Phase 2 features have backend RPC methods ready — only web handlers and templates needed.

### Navigation

Current:
```
Queue | Movies | Series | Wanted | Schedule | History | + Add | Status
```

Proposed additions:
```
Queue | Movies | Series | Wanted | Schedule | History | Blocklist | + Add | Status
```

Movie/series detail pages linked from existing list pages (click row → detail).
