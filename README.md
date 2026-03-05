<p align="center">
  <img src="docs/logo.png" alt="UDL" width="300">
</p>

<h1 align="center">UDL</h1>

<p align="center"><em>
Usenet Download Layer<br>
Usenet Data Liberator<br>
Unpacks Decrypts Loads<br>
Unattended Download Loop<br>
Unified Download Logic<br>
Usenet Decode Line<br>
Usenet Download Launcher<br>
Ultimate Data Leecher<br>
Unrar Deobfuscate Librarify<br>
Usenet Daemon Lite<br>
</em></p>

A single Go binary that replaces Sonarr + Radarr + NZBGet for Usenet-based media
automation. CLI-first, daemon mode, opinionated defaults.

## Install

```bash
go build -o udl ./cmd/udl
```

Requires `par2cmdline` for PAR2 verify/repair:

```bash
brew install par2cmdline
```

## Agent-Optimized CLI

The CLI is designed for deterministic, non-interactive use. Every command takes
TMDB IDs (the universal movie/TV identifier), and every list command outputs
TMDB IDs as the first column so output can be piped into the next command. This
makes UDL ideal for scripting and LLM agent workflows — no guessing, no
interactive prompts, no internal database IDs, just TMDB IDs from search to
grab to remove. Some commands also accept titles for convenience (e.g. `tv delete "Industry"`).

**Workflow: search TMDB, then add by TMDB ID, then check releases — same ID throughout.**

```bash
$ udl movie search "Dog"
TMDB ID  TITLE          YEAR
838240   Dog            2022
1025468  The Dog        2024

$ udl movie add 838240
added: Dog (2022) [tmdb=838240]
  -> release found and enqueued for download

$ udl movie releases 838240
#  TITLE                          QUALITY        SIZE      SCORE
1  Dog.2022.1080p.BluRay-GROUP    Bluray-1080p   8.5 GB    1200

$ udl movie grab 838240 1
grabbed: Dog (2022) [tmdb=838240]
  release: Dog.2022.1080p.BluRay-GROUP
  quality: Bluray-1080p
```

Every command uses the same TMDB ID — no internal database IDs exposed.
Queue shows `movie:<tmdb-id>` and `episode:<tmdb-id>:S01E02` — matching the retry syntax.

## Usage

```bash
udl daemon                   # start daemon (foreground)
udl status                   # check daemon status

# Movies — TMDB ID is the only identifier you need
udl movie search "Title"     # search TMDB, shows TMDB IDs
udl movie add <tmdb-id>      # add by TMDB ID
udl movie list               # list movies (shows TMDB IDs)
udl movie releases <tmdb-id> # search indexers for releases
udl movie grab <tmdb-id> <#> # grab release # for a movie
udl movie remove <tmdb-id>   # remove from monitoring

# TV — same pattern, all by TMDB ID
udl tv search "Title"        # search TMDB for series
udl tv add <tmdb-id>         # add by TMDB ID
udl tv list                  # list series (shows TMDB IDs)
udl tv releases <tmdb-id> -s 1 -e 1  # search indexers for episode releases
udl tv grab <tmdb-id> -s 1 -e 1 <#>  # grab release # for an episode
udl tv remove <tmdb-id>      # remove from monitoring
udl tv delete <title-or-id>  # delete files for a series, season, or episode
udl tv monitor <tmdb-id>     # show/change season monitoring
udl tv refresh               # refresh episode metadata from TMDB

# Delete & re-download
udl tv delete "Industry" -s 4 -e 8           # dry-run single episode
udl tv delete "Industry" -s 4 -e 8 --execute # delete file, reset to wanted
udl tv delete "Industry" -s 4 -e 8 --execute --search  # + blocklist old NZB & re-search
udl tv delete "Industry" -s 4 --execute      # delete entire season

# Queue & history
udl queue                                    # show queue
udl queue retry                              # retry all failed
udl queue retry movie:838240                 # retry failed movie by TMDB ID
udl queue retry episode:94997:S02E01         # retry failed episode
udl queue pause                              # pause all downloads
udl queue resume                             # resume all downloads
udl queue clear                              # clear all queued entries
udl history                                  # show download history
udl blocklist                                # show blocklisted releases
udl blocklist clear                          # clear all blocklist entries
udl blocklist remove <id>                    # remove specific entry

# Library management
udl library import <dir>              # identify and import media (dry-run)
udl library import <dir> --execute    # actually perform the import
udl library cleanup                   # find orphan/misnamed files (dry-run)
udl library cleanup --execute         # rename misnamed, delete orphans
udl library verify                    # read-only DB/disk consistency check
udl library prune                     # delete files for unmonitored episodes
udl library prune-incomplete          # find stale download dirs (dry-run)

# Plex integration
udl plex servers             # list Plex friend servers
udl plex check <tmdb-id>     # check if friends have it (by TMDB ID)
udl plex cleanup             # show unwatched old media (dry-run)
udl plex cleanup --execute   # delete unwatched media older than 90 days
udl plex cleanup --days 30   # shorter age threshold
```

## Web UI

Enable the optional web dashboard by adding a `[web]` section to config:

```toml
[web]
port = 9876
bind = "127.0.0.1"
```

The dashboard shows library stats, active downloads with live progress (via SSE), queue status, wanted items, series schedule, and download history. Navigation: Queue, Movies, Series, Wanted, Schedule, History.

![UDL Dashboard](docs/dashboard.png)

Pages use htmx for dynamic updates — the queue refreshes automatically via server-sent events.

## Plex Cleanup

Reclaim disk space by deleting media that was never watched on your Plex server. Queries your owned Plex server's watch history and identifies items added more than N days ago with zero plays.

```bash
udl plex cleanup                 # dry-run — shows what would be deleted
udl plex cleanup --days 30       # items older than 30 days (default: 90)
udl plex cleanup --execute       # actually delete files and reset to "wanted"
udl plex cleanup --verbose       # also show kept items with reasons
```

Output:
```
ACTION  TYPE    TITLE                QUALITY       AGE   SIZE
delete  movie   Late Night (2024)    WEBDL-1080p   120d  4.2 GB
delete  series  The Bear (2022)      WEBDL-1080p   95d   18.7 GB
keep    movie   Dune Part Two (2024) WEBDL-1080p   45d   — (too recent)
keep    series  Severance (2022)     WEBDL-1080p   60d   — (watched)

would delete 2 items (22.9 GB), keep 2 — use --execute to apply
```

On `--execute`: files are deleted, database status is reset to "wanted" (so they can be re-grabbed later if needed), and a "cleaned" history event is recorded. Empty directories are cleaned up automatically. Requires `[plex] token` in config.

## Migrating from Sonarr/Radarr

UDL can import your monitored media directly from running Sonarr and Radarr instances. The migrate commands talk to their APIs, resolve all metadata via TMDB, and populate UDL's database with the correct status and file paths. No daemon required.

### 1. Back up

```bash
cp ~/.config/udl/udl.db ~/.config/udl/udl.db.pre-migrate
```

### 2. Dry-run

Preview what will be imported without writing anything:

```bash
udl migrate radarr --url http://localhost:7878 --apikey YOUR_RADARR_API_KEY
udl migrate sonarr --url http://localhost:8989 --apikey YOUR_SONARR_API_KEY
```

The Sonarr migration resolves TVDB IDs to TMDB IDs (one API call per series with 250ms rate limiting). For ~100 series this takes about 30 seconds.

### 3. Execute

```bash
udl migrate radarr --url http://localhost:7878 --apikey YOUR_RADARR_API_KEY --execute
udl migrate sonarr --url http://localhost:8989 --apikey YOUR_SONARR_API_KEY --execute
```

For each monitored movie/series:
- Skips items already in UDL (by TMDB ID)
- Movies with files are marked `downloaded` with quality and absolute file path
- Movies without files are left as `wanted`
- Episodes in season 0 (specials) are skipped
- Quality names are mapped automatically (WEB-DL, WEBRip, Bluray, Remux, etc.)

### 4. Verify and rename

```bash
udl library verify                          # should show 0 missing
udl library cleanup                         # dry-run: preview renames
udl library cleanup --rename --execute      # rename to UDL conventions
```

This renames files from Sonarr's `Show - S01E01 - Title Quality.mkv` to UDL's `Show.S01E01.Title.Quality.mkv` and restructures series folders to include the year (e.g., `The Bear` becomes `The Bear (2022)`). Trigger a Plex library scan afterward.

### 5. Decommission old services

Once you've confirmed UDL is working (check `udl status`), stop the old services:

```bash
# macOS
launchctl unload ~/Library/LaunchAgents/com.sonarr.agent.plist
launchctl unload ~/Library/LaunchAgents/com.radarr.agent.plist
launchctl unload ~/Library/LaunchAgents/homebrew.mxcl.nzbget.plist
```

Keep Jackett running if Overseerr uses it.

## Configuration

Single config file at `~/.config/udl/config.toml`:

```toml
[library]
movies = "/path/to/movies"
tv = "/path/to/tv"

[paths]
incomplete = "/path/to/downloads/incomplete"
complete = "/path/to/downloads/complete"

[quality]
profile = "1080p"   # 720p, 1080p, 4k, remux

[tmdb]
apikey = "your-tmdb-api-key"

[[usenet.providers]]
name = "primary"
host = "news.example.com"
port = 563
tls = true
username = "user"
password = "pass"
connections = 30

[[indexers]]
name = "MyIndexer"
url = "https://indexer.example.com"
apikey = "key"

[plex]
token = "your-plex-token"  # optional, enables friend library checking + cleanup

[seerr]
url = "https://requests.example.com"  # optional, Overseerr/Jellyseerr URL
apikey = "your-seerr-api-key"         # auto-approves requests and adds to UDL

[web]
port = 9876          # optional, enables web dashboard
bind = "127.0.0.1"   # default: localhost only
```

## Library Import

UDL can scan an existing media directory, identify files via TMDB, and import them
into the library with canonical naming. Handles edge cases that trip up simpler parsers:

- **Year-in-title movies**: `2001.A.Space.Odyssey.1968.1080p.BluRay` → title="2001 A Space Odyssey", year=1968
- **Edition tags**: `Blade.Runner.The.Final.Cut.1982.1080p.BluRay` → title="Blade Runner", edition="The Final Cut"
- **Noise tokens**: `Movie.PROPER.REPACK.2024.1080p.WEB-DL` → title="Movie"
- **Year-as-season** (Plex annual shows): `S2024E02` → season=2024, episode=2
- **Diacritic folding**: Shōgun → Shogun, Fóstbræður → Fostbraedur (ð→d, þ→th, æ→ae, ō→o)
- **Fuzzy year matching**: accepts ±1 year offset between release and TMDB (production vs release year)

Tested against a real Plex library: 104/107 movies and 1067/1072 TV episodes identified
correctly. Remaining gaps are TMDB coverage issues, not parser failures.

## File Naming

Hardcoded, Plex-compatible. Folders use spaces (human-readable), filenames use dots (scene-style).

**Movies:**
```
{library}/Movie Title (Year)/Movie.Title.Year.Quality.ext
```
Example:
```
movies/Die Hard (1988)/Die.Hard.1988.Bluray-1080p.mkv
movies/Dune Part Two (2024)/Dune.Part.Two.2024.WEBDL-1080p.mkv
```

**TV:**
```
{library}/Show Name (Year)/Season 01/Show.Name.S01E01.Episode.Title.Quality.ext
```
Example:
```
tv/The Bear (2022)/Season 01/The.Bear.S01E01.System.WEBDL-1080p.mkv
tv/Severance (2022)/Season 02/Severance.S02E01.Hello.Ms.Cobel.WEBDL-1080p.mkv
```

Quality tags: `SDTV`, `HDTV-720p`, `WEBDL-1080p`, `Bluray-1080p`, `WEBDL-2160p`, `Bluray-2160p`, `Remux-1080p`, `Remux-2160p`.

No configurable naming templates. This is the way.

## Download Pipeline

```
Search indexers → Score & filter releases (or LLM pick) → Fetch NZB
  → NNTP segment download (connection pooling, retry, backoff)
  → yEnc decode → PAR2 verify/repair → RAR extract
  → Magic byte detection (handles obfuscated filenames)
  → Import to library with canonical naming
```

Failed downloads are automatically blocklisted so re-search picks a different release.
Stuck downloads (>2h) are auto-reset. Disk space is checked before starting (2x + 1GB).

## Testing

```bash
go test ./... -count=1                            # all tests
go test ./internal/parser/ -v                     # parser edge cases
go test ./internal/daemon/ -run TestPipeline -v   # integration tests
go test ./internal/daemon/ -run TestLibrary -v    # library management tests
```

## Architecture

- **Single binary**, single config (`~/.config/udl/config.toml`), single SQLite database (`~/.config/udl/udl.db`)
- **CLI ↔ Daemon** via `net/rpc` over Unix socket
- **Scheduler** runs air-date-driven episode search (every 2m) and movie search sweeps (every 6h)
- **Plex integration** checks friend libraries before downloading (optional)
- **Seerr integration** auto-approves Overseerr/Jellyseerr requests and adds them to the library
- **LLM-assisted release selection** — when `codex` or `claude` CLI is in PATH, asks the LLM to pick the best release instead of pure score-ordering. Falls back to scoring when unavailable or on error
- **Quality profiles:** 720p, 1080p (default), 4k, remux — each with min/preferred/upgrade tiers
- **Original language tracking** — stores TMDB original language for movies and series, displayed in CLI and web UI

## Design Principles

- Usenet only — no torrents
- Optional web dashboard — CLI is the primary interface
- No custom naming templates
- Minimal config surface — provider credentials, indexer keys, library paths
- Failed releases are blocklisted, not retried
- Atomic file imports (write to tmp, fsync, rename)
