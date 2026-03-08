# CLAUDE.md

## Project

UDL (Usenet Download Layer) — a single Go binary replacing Sonarr + Radarr + NZBGet for Usenet-based media automation. CLI-first, daemon-mode, opinionated defaults.

## Quick Reference

```bash
go build -o udl ./cmd/udl     # build
go test ./... -count=1         # all tests
./udl daemon                   # start daemon (foreground)
./udl status                   # check daemon
./udl movie add "Title"        # add movie
./udl movie search "Title"     # search indexers
./udl movie list               # list movies
./udl tv add "Title"           # add TV series
./udl queue                    # show download queue

# Deploy: Claude unloads + builds, human signs + loads
launchctl unload ~/Library/LaunchAgents/com.udl.daemon.plist
go build -o ~/bin/udl ./cmd/udl
# HUMAN STEP (Keychain prompt):
codesign --force --sign "UDL" ~/bin/udl && launchctl load ~/Library/LaunchAgents/com.udl.daemon.plist
```

## Architecture

- **Single binary**, single config (`~/.config/udl/config.toml`), single db (`~/.config/udl/udl.db`)
- **CLI ↔ Daemon** via `net/rpc` over Unix socket (`~/.config/udl/udl.sock`)
- **Web UI** at `udl.plex.uno` (Caddy reverse proxy → localhost:9876), htmx + SSE
- **Daemon** runs: episode search (air-date-driven, 2m tick), movie search sweep (6h), downloader (polls queue every 5s)
- **Download pipeline:** fetch NZB → parse → NNTP segment download → yEnc decode → PAR2 verify/repair → RAR extract → cleanup → import to library

## Package Map

```
cmd/udl/main.go          CLI entry (cobra commands, thin RPC wrappers)
internal/
  config/                 TOML config loading + validation
  database/               SQLite schema, models, CRUD (WAL mode, foreign keys)
  daemon/
    daemon.go             RPC service (AddMovie, ListSeries, SearchMovie, etc.)
    downloader.go         Download queue processor
    scheduler.go          Air-date-driven episode search + movie search sweep
    searcher.go           Release scoring, cleanTitle() for movie matching, year validation
  newznab/                Newznab API client (search, NZB fetch)
  nntp/
    conn.go               NNTP protocol (connect, auth, body fetch)
    pool.go               Connection pooling per provider
    engine.go             Multi-file download coordination
  nzb/                    NZB XML parser
  parser/                 Release title parser (regex: title, year, S/E, quality, group)
  quality/                Quality tier enum (SDTV→Remux-2160p), profiles, ShouldGrab()
  organize/               File renaming + import (hardcoded Plex-compatible naming)
  par2/                   PAR2 binary parser (FileDesc packets, hash16k matching)
  postprocess/            PAR2 rename + verify (par2cmdline), RAR (rardecode), cleanup
  tmdb/                   TMDB API wrapper (movies, TV, TVDB/IMDB cross-refs)
  plex/                   Plex friend-server availability check
  migrate/                Sonarr/Radarr import commands
  web/                    Embedded HTTP server (htmx templates, SSE queue updates)
  yenc/                   yEnc binary-to-text decoder with CRC32 verification
```

## Key Dependencies

- Go 1.24, `modernc.org/sqlite` (pure Go, no CGo), `spf13/cobra`, `cyruzin/golang-tmdb`
- `nwaples/rardecode` v2 for RAR, `golang.org/x/text` for unicode normalization
- External: `par2cmdline` (brew install) for PAR2 verify/repair

## Production Environment

- **Binary:** `~/bin/udl`, LaunchAgent `com.udl.daemon.plist`
- **Library:** `/Users/jokull/Plex/media/{tv,movies}`
- **Downloads:** `/Volumes/Plex/downloads/` (external exFAT volume)
- **Logs:** `~/Library/Logs/udl.log`
- **Old Sonarr/Radarr/NZBGet:** unloaded, configs preserved at `~/mediaserver/.config/{radarr,sonarr}/`
- See [CURRENT-SETUP.md](CURRENT-SETUP.md) for API keys and legacy setup details

## Code Signing & Deploy

The binary accesses `/Volumes/Plex` (removable volume) which requires macOS TCC permission.
A self-signed "UDL" certificate in the login keychain provides a stable signing identity so
TCC grants persist across rebuilds (ad-hoc `--sign -` pins to CDHash which changes every build).

**Deploy flow — Claude builds & unloads, human signs & loads:**
1. Claude: `launchctl unload ~/Library/LaunchAgents/com.udl.daemon.plist`
2. Claude: `go build -o ~/bin/udl ./cmd/udl`
3. **Human runs in terminal:** `codesign --force --sign "UDL" ~/bin/udl && launchctl load ~/Library/LaunchAgents/com.udl.daemon.plist`

The codesign step requires Keychain access to the private key which triggers a macOS dialog —
this cannot be automated from Claude Code's sandbox without storing the login password in
plaintext (`security set-key-partition-list`), which we don't do.

## Conventions

- **Quality profiles:** 720p, 1080p (default), 4k, remux — each sets min/preferred/upgrade_until
- **File naming is hardcoded** — folders use spaces, filenames use dots: `Movie.Year.Quality.ext`, `Show.S01E01.Title.Quality.ext`
- **No torrents, no custom naming templates** — permanent scope exclusions
- **Title matching:** `cleanTitle()` in searcher.go strips to pure alphanumeric lowercase, exact equality
- **Year validation:** movie searches reject releases with wrong parsed year

## Planning Documents

- [OVERVIEW.md](OVERVIEW.md) — philosophy, feature overview
- [ARCHITECTURE.md](ARCHITECTURE.md) — system design, components, database schema, RPC methods
- [PIPELINE.md](PIPELINE.md) — download + post-processing pipeline design
- [RESEARCH.md](RESEARCH.md) — Go library ecosystem analysis, dependency decisions
- [CURRENT-SETUP.md](CURRENT-SETUP.md) — legacy Sonarr/Radarr/NZBGet setup

## Indexers

Config: `~/.config/udl/config.toml` — DOGnzb, omgwtf, Nzb.su (3 active). omgwtf frequently hits daily API limits.
