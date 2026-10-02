# CLAUDE.md

## Project

UDL (Usenet Download Layer) — a single Go binary replacing Sonarr + Radarr + NZBGet for Usenet-based media automation. CLI-first, daemon-mode, opinionated defaults.

## Quick Reference

```bash
go build -o udl ./cmd/udl     # build
go test ./... -count=1         # all tests
go test -race ./... -count=1   # race detector
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
- **Plex download pipeline:** probe → verified 2 MiB range blocks into a persistent `.part` (+ `.meta.json` resume sidecar) → exact-size check → rename → import. Interruptions resume from the last verified block; the partial is only discarded on terminal failures or after the retry budget is spent.
- **Failure taxonomy (`internal/failure`):** every failure carries a class — `content`,
  `manual`, `missing`, `transport`, `permission`, `local`, `client`, `unknown`. Only
  `content`/`manual` block a release permanently; `local`/`client` record nothing at all
  (our fault is not the release's), and the rest cool down and lapse. `BlamesSource()`
  decides whether a failure counts against a friend, `BlamesUs()` whether it spends the
  retry budget. Classify from typed errors (`rangefetch.HTTPError`, `newznab.Error.Kind`,
  `nntp.IsArticleNotFound`), never from message text.
- **Friend-server selection:** derived from `server_attempts` (Beta-smoothed success rate + measured MB/s over 30 days), ranked reliability-band first then speed, with a derived transport-failure circuit breaker and 10% exploration. `[[plex.servers]]` in config carries only deny/prefer/bias overrides.

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
    serverrank.go         Plex friend ranking (reliability band, speed, exploration, breaker)
    doctor.go             `udl doctor` — health checks plus parked/partial/log findings
    plexprobe.go          `udl plex probe` — measures each friend through the real transport
  failure/                Failure classes and what each one justifies (blocking, cooldown,
                          whose fault) — shared by blocklist, retry budget and reputation
  logging/                Log rotation: truncate-in-place, because launchd holds the fd
  newznab/                Newznab API client (search, NZB fetch)
  nntp/
    conn.go               NNTP protocol (connect, auth, body fetch)
    pool.go               Connection pooling per provider
    engine.go             Multi-file download coordination
  nzb/                    NZB XML parser
  parser/                 Release title parser (regex: title, year, S/E, quality, group)
  quality/                Quality tier enum (SDTV→Remux-2160p), profiles, ShouldGrab()
  rangefetch/             Validated, resumable HTTP byte-range transport shared by
                          shadow streaming and Plex downloads (strict 206/Content-Range
                          checks, resource identity, per-block resume sidecar)
  organize/               File renaming + import (hardcoded Plex-compatible naming)
  par2/                   PAR2 binary parser (FileDesc packets, hash16k matching)
  postprocess/            PAR2 rename + verify (par2), RAR (rardecode), cleanup
  tmdb/                   TMDB API wrapper (movies, TV, TVDB/IMDB cross-refs)
  plex/                   Plex friend-server availability check
  migrate/                Sonarr/Radarr import commands
  web/                    Embedded HTTP server (htmx templates, SSE queue updates)
  yenc/                   yEnc binary-to-text decoder with CRC32 verification
```

## Key Dependencies

- Go 1.24, `modernc.org/sqlite` (pure Go, no CGo), `spf13/cobra`, `cyruzin/golang-tmdb`
- `nwaples/rardecode` v2 for RAR, `golang.org/x/text` for unicode normalization
- External: `par2` (brew install) for PAR2 verify/repair

## Production Environment

- **Binary:** `~/bin/udl`, LaunchAgent `com.udl.daemon.plist`
- **Library:** `/Users/jokull/Plex/media/{tv,movies}`
- **Downloads:** `/Volumes/Plex/downloads/` (external exFAT volume)
- **Logs:** `~/Library/Logs/udl.log` (bounded: the daemon truncates it past 64 MiB every
  15 min, keeping an 8 MiB `.1`. launchd owns the descriptor, so rotation truncates in
  place — a rename would leave launchd appending to the rotated file)
- **Old Sonarr/Radarr/NZBGet:** unloaded, configs preserved at `~/mediaserver/.config/{radarr,sonarr}/`
- See [CURRENT-SETUP.md](CURRENT-SETUP.md) for API keys and legacy setup details

## Code Signing & Deploy

The binary accesses `/Volumes/Plex` (removable volume) which requires macOS TCC permission.
A self-signed "UDL" certificate in the login keychain provides a stable signing identity so
TCC grants persist across rebuilds (ad-hoc `--sign -` pins to CDHash which changes every build).

**Deploy flow — Claude stages, human signs, Claude activates:**
1. Claude: `./scripts/deploy.sh build` (builds `~/bin/udl.new`, never the live path)
2. **Human runs in terminal:** `codesign --force --sign "UDL" ~/bin/udl.new`
3. Claude: `./scripts/deploy.sh activate` (atomic swap + restart + health check)

**Never build directly onto `~/bin/udl`.** That path is shared by the daemon and
every shadow agent serving an NFS mount to Plex. A build there leaves an
unsigned/ad-hoc binary where a `KeepAlive` restart can exec it, and an ad-hoc
signature has lost the TCC grant for `/Volumes/Plex` — the shadow would come up
denied and break playback. `activate` refuses to install an ad-hoc binary for
the same reason.

Checking on the box (`./scripts/deploy.sh status` does all of this, with
timeouts so a hung mount cannot stall you):
- Note that `timeout` is a shell function in an interactive session, **not** a
  program: a script that execs it fails with 127 and misreports. Use
  `/usr/bin/perl -e 'alarm N; exec @ARGV'` inside scripts.

The codesign step requires Keychain access to the private key which triggers a macOS dialog —
this cannot be automated from Claude Code's sandbox without storing the login password in
plaintext (`security set-key-partition-list`), which we don't do.

## Diagnosing a live install

`udl doctor` runs everything below and attaches the fix to each finding. By hand:

```bash
udl status                 # daemon, queue, failed (24h), blocklisted (active), parked
udl doctor                 # all checks + parked items, partials, cooling friends, log size
udl queue                  # downloads, including failed ones with their error
udl blocklist              # blocks in force (--all includes lapsed cooldowns)
udl plex probe             # measure friends now instead of waiting for traffic
tail -f ~/Library/Logs/udl.log
```

Traps this codebase has already fallen into, worth remembering:

- **The HTTP User-Agent.** NZBFinder answers Go's default `Go-http-client/1.1` with a
  Cloudflare 403 that looks like a dead release. Every outbound request goes through
  `internal/httpclient` (or `newznab.Client`, which is the indexer choke point).
- **Timestamp types.** `modernc.org/sqlite` decodes a direct `TIMESTAMP` column reference
  as `time.Time`, but an aggregate like `MAX(created_at)` as text. Scan the first into
  `sql.NullTime` and the second into a string; parsing the wrong rendering fails silently
  because callers ignore the error.
- **Never delete what you did not create.** `library prune-incomplete` only removes
  directories whose name parses as one of the daemon's own layouts, and reports anything
  else instead of removing it.

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
