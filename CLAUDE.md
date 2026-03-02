# CLAUDE.md

## Project

UDL (Usenet Download Layer) — a single Go binary replacing Sonarr + Radarr + NZBGet for Usenet-based media automation. CLI-first, daemon-mode, opinionated defaults.

**Genesis:** "Combine sonarr, radarr and nzbget into a single Go CLI and daemon. Start super simple with a single NZB indexer and only usenet download, as few config parameters as possible, single config file. Only interface is the CLI — TUI to be added later."

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
```

## Architecture

- **Single binary**, single config (`~/.config/udl/config.toml`), single db (`~/.config/udl/udl.db`)
- **CLI ↔ Daemon** via `net/rpc` over Unix socket (`~/.config/udl/udl.sock`)
- **Daemon** runs: scheduler (RSS sync every 15m, search sweep every 6h), downloader (polls queue every 5s)
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
    scheduler.go          RSS sync + search sweep loops, normalize() for TV matching
    searcher.go           Release scoring, cleanTitle() for movie matching, year validation
  newznab/                Newznab API client (search, RSS, NZB fetch)
  nntp/
    conn.go               NNTP protocol (connect, auth, body fetch)
    pool.go               Connection pooling per provider
    engine.go             Multi-file download coordination
  nzb/                    NZB XML parser
  parser/                 Release title parser (regex: title, year, S/E, quality, group)
  quality/                Quality tier enum (SDTV→Remux-2160p), profiles, ShouldGrab()
  organize/               File renaming + import (hardcoded Plex-compatible naming)
  postprocess/            PAR2 (shells out to par2cmdline), RAR (rardecode), cleanup
  tmdb/                   TMDB API wrapper (movies, TV, TVDB/IMDB cross-refs)
  yenc/                   yEnc binary-to-text decoder with CRC32 verification
```

## Key Dependencies

- Go 1.24, `modernc.org/sqlite` (pure Go, no CGo), `spf13/cobra`, `cyruzin/golang-tmdb`
- `nwaples/rardecode` v2 for RAR, `golang.org/x/text` for unicode normalization
- External: `par2cmdline` (brew install) for PAR2 verify/repair

## Test Environment

Existing Sonarr/Radarr/NZBGet runs natively via Homebrew on this Mac:
- Radarr: localhost:7878, config at `~/mediaserver/.config/radarr/`
- Sonarr: localhost:8989, config at `~/mediaserver/.config/sonarr/`
- NZBGet: localhost:6789, config at `/opt/homebrew/etc/nzbget.conf`
- Library: `/Users/jokull/Plex/media/{tv,movies}`
- Downloads: `/Volumes/Plex/downloads/`
- See [CURRENT-SETUP.md](CURRENT-SETUP.md) for full details and API keys

## Conventions

- **Quality profiles:** 720p, 1080p (default), 4k, remux — each sets min/preferred/upgrade_until
- **File naming is hardcoded** — folders use spaces, filenames use dots: `Movie.Year.Quality.ext`, `Show.S01E01.Title.Quality.ext`
- **No torrents, no web UI, no custom naming templates** — these are permanent scope exclusions
- **Title matching:** `cleanTitle()` in searcher.go strips to pure alphanumeric lowercase (Radarr's approach), exact equality. `normalize()` in scheduler.go is looser for RSS TV matching.
- **Year validation:** movie searches reject releases with wrong parsed year

## Planning Documents

- [OVERVIEW.md](OVERVIEW.md) — philosophy, feature overview
- [ARCHITECTURE.md](ARCHITECTURE.md) — system design, components, database schema, RPC methods
- [PIPELINE.md](PIPELINE.md) — download + post-processing pipeline design
- [RESEARCH.md](RESEARCH.md) — Go library ecosystem analysis, dependency decisions
- [CURRENT-SETUP.md](CURRENT-SETUP.md) — existing local Sonarr/Radarr/NZBGet setup

## Indexers Configured

UDL config (`~/.config/udl/config.toml`): DOGnzb, omgwtf (omgwtf hitting daily limits)

Radarr has additional indexers that could be added: Nzb.su, NZBgeek, NZBFinder.ws (marked as expired keys in UDL config — verify if still active in Radarr)
