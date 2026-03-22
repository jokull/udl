# Architecture

## Pre-Beta Robustness Roadmap (2026-03)

This section is authoritative for reliability hardening work in pre-beta.

### Immediate priorities (in progress)

1. Control-plane correctness
   - Queue pause/resume must be real (no-op RPC removed).
   - State-changing RPC/web operations must fail loudly on persistence errors.

2. Safer destructive operations
   - Any delete operation that touches media files must verify target paths are inside configured library roots.
   - Refuse deletion on path escape, symlink surprises, or non-absolute paths.

3. Backpressure and failure containment
   - Reduce DB/UI polling pressure and add request-level throttling where needed.
   - Prevent worker retry storms on transient post-processing failures.

### Next priorities (pre-beta)

1. Explicit media state machine
   - Centralize all legal status transitions (`wanted -> queued -> downloading -> post_processing -> downloaded|failed`).
   - Reject invalid transitions in one place.

2. Durable execution state split
   - Introduce a dedicated `downloads` execution table (attempts, lease owner, heartbeat, retry policy, phase timestamps).
   - Keep `movies`/`episodes` as desired-state + current best-known media metadata.

3. Migration discipline
   - Move from ad-hoc `ALTER TABLE` checks to versioned, transactional migrations (`schema_migrations`).
   - Enforce startup compatibility checks before daemon serves requests.

### Candidate libraries / systems

- River (Postgres jobs): https://github.com/riverqueue/river
- Asynq (Redis jobs): https://github.com/hibiken/asynq
- NATS JetStream (durable queue semantics): https://docs.nats.io/nats-concepts/jetstream
- Retry/backoff primitives:
  - https://github.com/cenkalti/backoff
  - https://github.com/hashicorp/go-retryablehttp
  - https://pkg.go.dev/golang.org/x/time/rate

### Acceptance criteria for this phase

- `queue pause` and `queue resume` visibly alter downloader behavior.
- Completion path does not report success if DB transaction failed.
- Deletes outside configured library roots are blocked and logged.
- No behavior regressions in `go test ./...`.

## Config File

Single TOML file at `~/.config/udl/config.toml` (or `$UDL_CONFIG`).

```toml
# Library paths — the only structural config
[library]
tv = "/Users/jokull/Plex/media/tv"
movies = "/Users/jokull/Plex/media/movies"

# Working directories
[paths]
incomplete = "/Volumes/Plex/downloads/incomplete"   # active downloads
complete = "/Volumes/Plex/downloads/complete"        # post-processed, before import
# Database lives at ~/.config/udl/udl.db (SQLite)

# Quality — pick a profile or customize
# Profiles: "720p", "1080p", "4k", "remux"
[quality]
profile = "1080p"
# Override individual values if needed:
# upgrade_until = "Remux-1080p"

# TMDB API key (required — get one at https://www.themoviedb.org/settings/api)
[tmdb]
apikey = "your-tmdb-api-key"

# Usenet providers
[[usenet.providers]]
name = "newshosting"
host = "news.newshosting.com"
port = 563
tls = true
username = "..."
password = "..."
connections = 30
level = 0          # primary

[[usenet.providers]]
name = "tweaknews"
host = "newshosting.tweaknews.eu"
port = 563
tls = true
username = "..."
password = "..."
connections = 20
level = 0

[[usenet.providers]]
name = "usenet.farm"
host = "news.usenet.farm"
port = 443
tls = true
username = "..."
password = "..."
connections = 8
level = 1          # fill/backup

# Newznab indexers
[[indexers]]
name = "nzbgeek"
url = "https://api.nzbgeek.info"
apikey = "..."

# Daemon settings
[daemon]
rss_interval = "15m"
```

That's it. No naming templates, no download client config, no indexer categories,
no tags, no custom formats, no root folder mapping.

## Components

```
┌─────────────────────────────────────────────────────┐
│                    UDL Binary                        │
│                                                     │
│  ┌──────────┐  ┌───────────┐  ┌──────────────────┐ │
│  │   CLI    │  │  Daemon   │  │    Scheduler     │ │
│  │ (cobra)  │  │  (main    │  │ (robfig/cron or  │ │
│  │          │  │   loop)   │  │  gocron)         │ │
│  └────┬─────┘  └─────┬─────┘  └────────┬─────────┘ │
│       │              │                  │           │
│  ┌────┴──────────────┴──────────────────┴─────────┐ │
│  │              Core Engine                        │ │
│  │                                                 │ │
│  │  ┌─────────────┐  ┌────────────────────────┐   │ │
│  │  │  Library     │  │  Newznab Client        │   │ │
│  │  │  Manager     │  │  (RSS + Search)        │   │ │
│  │  │  (wanted,    │  │                        │   │ │
│  │  │   existing)  │  └────────┬───────────────┘   │ │
│  │  └──────┬──────┘           │                    │ │
│  │         │           ┌──────┴──────┐             │ │
│  │         │           │  Release    │             │ │
│  │         │           │  Parser     │             │ │
│  │         │           │  + Scorer   │             │ │
│  │         │           └──────┬──────┘             │ │
│  │         │                  │                    │ │
│  │  ┌──────┴──────────────────┴──────────────┐     │ │
│  │  │          Download Engine               │     │ │
│  │  │                                        │     │ │
│  │  │  ┌──────────┐  ┌────────┐ ┌────────┐  │     │ │
│  │  │  │ NNTP     │  │  NZB   │ │ yEnc   │  │     │ │
│  │  │  │ Pool     │  │ Parser │ │ Decode │  │     │ │
│  │  │  └──────────┘  └────────┘ └────────┘  │     │ │
│  │  └──────────────────┬─────────────────────┘     │ │
│  │                     │                           │ │
│  │  ┌──────────────────┴─────────────────────┐     │ │
│  │  │       Post-Processing Pipeline         │     │ │
│  │  │                                        │     │ │
│  │  │  PAR2 rename → PAR2 verify/repair →    │     │ │
│  │  │  RAR extract → cleanup → rename+move   │     │ │
│  │  └────────────────────────────────────────┘     │ │
│  └─────────────────────────────────────────────────┘ │
│                                                     │
│  ┌─────────────────────────────────────────────────┐ │
│  │              Metadata Clients                   │ │
│  │  TMDB (movies + TV) — may skip TVDB entirely    │ │
│  └─────────────────────────────────────────────────┘ │
│                                                     │
│  ┌─────────────────────────────────────────────────┐ │
│  │              State (SQLite)                      │ │
│  │  wanted items, download queue, history,          │ │
│  │  library index, indexer state                    │ │
│  └─────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────┘
```

## CLI Commands

```bash
# Daemon
udl daemon              # start daemon (foreground)
udl daemon --detach     # start daemon (background)
udl status              # show daemon status, queue, speeds

# Movies
udl movie add "Dune Part Two"         # search TMDB, add to wanted
udl movie add --imdb tt15239678       # add by IMDB ID
udl movie list                        # list wanted + downloaded movies
udl movie search "Dune"               # manual indexer search, pick result

# TV
udl tv add "Severance"                # search TMDB/TVDB, add series
udl tv add --tvdb 371980              # add by TVDB ID
udl tv list                           # list monitored series
udl tv search "Severance S02E01"      # manual indexer search

# Queue
udl queue                             # show download queue
udl queue pause                       # pause all downloads
udl queue resume                      # resume
udl history                           # show completed downloads

# Config
udl config check                      # validate config file
udl config path                       # print config file path
```

## Database Schema (SQLite)

```sql
-- Media library
CREATE TABLE movies (
    id INTEGER PRIMARY KEY,
    tmdb_id INTEGER UNIQUE NOT NULL,
    imdb_id TEXT,
    title TEXT NOT NULL,
    year INTEGER,
    status TEXT NOT NULL DEFAULT 'wanted',  -- wanted, downloading, downloaded
    quality TEXT,                            -- quality of current file
    file_path TEXT,                          -- path to imported file
    added_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE series (
    id INTEGER PRIMARY KEY,
    tvdb_id INTEGER UNIQUE,
    tmdb_id INTEGER UNIQUE,
    imdb_id TEXT,
    title TEXT NOT NULL,
    year INTEGER,
    status TEXT NOT NULL DEFAULT 'monitored', -- monitored, ended, unmonitored
    added_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE episodes (
    id INTEGER PRIMARY KEY,
    series_id INTEGER REFERENCES series(id),
    season INTEGER NOT NULL,
    episode INTEGER NOT NULL,
    title TEXT,
    air_date DATE,
    status TEXT NOT NULL DEFAULT 'wanted',   -- wanted, downloading, downloaded, skipped
    quality TEXT,
    file_path TEXT,
    UNIQUE(series_id, season, episode)
);

-- Download management
CREATE TABLE downloads (
    id INTEGER PRIMARY KEY,
    nzb_url TEXT,
    nzb_name TEXT NOT NULL,
    title TEXT NOT NULL,
    category TEXT NOT NULL,                  -- movie, episode
    media_id INTEGER NOT NULL,              -- references movies.id or episodes.id
    status TEXT NOT NULL DEFAULT 'queued',   -- queued, downloading, post_processing, importing, completed, failed
    progress REAL DEFAULT 0,
    size_bytes INTEGER,
    downloaded_bytes INTEGER DEFAULT 0,
    error TEXT,
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Indexer state
CREATE TABLE indexers (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    apikey TEXT NOT NULL,
    last_rss_at TIMESTAMP
);

-- History / audit
CREATE TABLE history (
    id INTEGER PRIMARY KEY,
    media_type TEXT NOT NULL,               -- movie, episode
    media_id INTEGER NOT NULL,
    event TEXT NOT NULL,                    -- grabbed, downloaded, imported, failed, upgraded
    source TEXT,                            -- release name
    quality TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

## Quality Tiers (hardcoded order, low to high)

```
SDTV
WEBDL-480p
DVD
HDTV-720p
WEBDL-720p
Bluray-720p
HDTV-1080p
WEBDL-1080p
Bluray-1080p
WEBDL-2160p
Bluray-2160p
Remux-1080p
Remux-2160p
```

## Quality Profiles

Pick a profile or set individual values. Profile provides the base; individual values override.

| Profile | Min | Preferred | Upgrade Until | Description |
|---------|-----|-----------|---------------|-------------|
| `720p` | HDTV-720p | WEBDL-720p | Bluray-720p | Small files, fast downloads |
| **`1080p`** | WEBDL-720p | WEBDL-1080p | Bluray-1080p | **The sweet spot (recommended)** |
| `4k` | WEBDL-1080p | WEBDL-2160p | Bluray-2160p | Best quality WEB/Bluray |
| `remux` | Bluray-1080p | Remux-1080p | Remux-2160p | Untouched Bluray, huge files |

Three knobs:
- `min`: reject anything below this tier
- `preferred`: actively search for this quality
- `upgrade_until`: stop upgrading once you have this or better

## Metadata Strategy

**TMDB only.** TMDB has both movie and TV data, including TVDB/IMDB cross-references.

- Movies: search TMDB → get IMDB ID → pass `imdbid` to Newznab `t=movie`
- TV: search TMDB → get TVDB ID from external_ids → pass `tvdbid` to Newznab `t=tvsearch`
- Episode lists: TMDB provides season/episode data
- Cache all metadata in SQLite to minimize API calls

## Daemon Lifecycle

```
Start
  │
  ├── Load config
  ├── Open/migrate database
  ├── Initialize NNTP connection pools (lazy — connect on first download)
  ├── Start scheduler
  │     ├── RSS sync (every rss_interval, default 15m)
  │     │     ├── Query each indexer RSS feed
  │     │     ├── Parse releases through release parser
  │     │     ├── Match against wanted items
  │     │     ├── Score and select best releases
  │     │     └── Enqueue downloads
  │     └── Cleanup (daily: prune old history, check stale downloads)
  │
  ├── Process download queue (continuous)
  │     ├── Pick highest priority queued item
  │     ├── Fetch NZB from indexer
  │     ├── Parse NZB → list of files → list of segments
  │     ├── Download segments via NNTP pool
  │     ├── Decode yEnc → write to incomplete dir
  │     ├── Post-process (PAR2 → RAR → cleanup)
  │     ├── Identify media files
  │     ├── Rename + move to library
  │     └── Update database
  │
  └── Listen for CLI commands (Unix domain socket, net/rpc)
```

## Daemon IPC

**`net/rpc` with gob encoding over a Unix domain socket.**

Socket path: `~/.config/udl/udl.sock`

The stdlib `net/rpc` package is frozen (no new features since Go 1.8) but stable
forever under the Go 1 compatibility guarantee. For a Go-only CLI talking to its
own daemon, this is ideal: zero dependencies, binary encoding, trivial to wire up.

```go
// Daemon side
ln, _ := net.Listen("unix", sockPath)
rpc.Register(&DaemonService{})
rpc.Accept(ln)

// CLI side
client, _ := rpc.Dial("unix", sockPath)
var reply QueueReply
client.Call("Daemon.Queue", &QueueArgs{}, &reply)
```

### RPC Methods

```go
type DaemonService struct { ... }

// Movies
func (d *DaemonService) AddMovie(args *AddMovieArgs, reply *AddMovieReply) error
func (d *DaemonService) ListMovies(args *ListArgs, reply *MovieListReply) error
func (d *DaemonService) SearchMovie(args *SearchArgs, reply *SearchReply) error

// TV
func (d *DaemonService) AddSeries(args *AddSeriesArgs, reply *AddSeriesReply) error
func (d *DaemonService) ListSeries(args *ListArgs, reply *SeriesListReply) error
func (d *DaemonService) SearchEpisode(args *SearchArgs, reply *SearchReply) error

// Queue
func (d *DaemonService) Queue(args *QueueArgs, reply *QueueReply) error
func (d *DaemonService) PauseAll(args *Empty, reply *Empty) error
func (d *DaemonService) ResumeAll(args *Empty, reply *Empty) error
func (d *DaemonService) History(args *HistoryArgs, reply *HistoryReply) error

// Status
func (d *DaemonService) Status(args *Empty, reply *StatusReply) error
```

The CLI commands are thin wrappers: parse flags → dial socket → call RPC method →
format reply for terminal output. All logic lives in the daemon.

## Decisions Made

| Question | Answer |
|----------|--------|
| Metadata provider | TMDB only |
| Daemon IPC | `net/rpc` + gob over Unix socket |
| PAR2 | Shell out to `par2` (v1) |
| Multi-episode files | File under first episode |
| Project name | `udl` |

## Open Questions

1. **NNTP implementation depth?** Build native using Tensai75 libraries as a starting point,
   or start with a simpler approach and iterate?

2. **Database migrations?** Embed SQL migrations in the binary? Use a migration library?
   Or just `CREATE TABLE IF NOT EXISTS` for v1?

3. **Logging?** stdlib `log/slog` (structured logging, added in Go 1.21) seems right.
   Log to stderr when foreground, log to file when detached?
