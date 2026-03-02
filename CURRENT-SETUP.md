# Current Local Setup

Documentation of the existing Sonarr/Radarr/NZBGet setup on this Mac, used as
reference and test environment.

## Deployment Model

All media services run **natively on macOS via Homebrew** (not Docker). Docker is
only used for supporting services (FlareSolverr, deildu-proxy, Overseerr).

## Services

| Service | Port | Install | Config Location |
|---------|------|---------|-----------------|
| Sonarr | 8989 | Homebrew | `~/mediaserver/.config/sonarr/` |
| Radarr | 7878 | Homebrew | `~/mediaserver/.config/radarr/` |
| NZBGet | 6789 | Homebrew | `/opt/homebrew/etc/nzbget.conf` |
| Jackett | 9117 | Homebrew | `~/mediaserver/.config/jackett/` |
| FlareSolverr | 8191 | Docker | `~/mediaserver/docker-compose.yml` |

## Usenet Providers

Three providers in tiered priority:

| Provider | Host | Connections | Level | Role |
|----------|------|-------------|-------|------|
| Newshosting | `news.newshosting.com:563` | 30 | 0 | Primary |
| Tweaknews | `newshosting.tweaknews.eu:563` | 20 | 0 | Primary |
| Usenet.Farm | `news.usenet.farm:443` | 8 | 1 | Fill/backup |

All use TLS encryption.

## Directory Structure

```
/Volumes/Plex/downloads/
├── intermediate/          # NZBGet: active downloads (InterDir)
├── completed/             # NZBGet: finished downloads (DestDir)
│   ├── Movies/
│   ├── Series/
│   ├── Music/
│   └── Software/
├── nzb/                   # NZBGet: incoming NZB watch folder
├── queue/                 # NZBGet: internal state
├── tmp/                   # NZBGet: temp files
└── scripts/               # NZBGet: post-processing scripts

/Users/jokull/Plex/media/
├── tv/                    # Sonarr: TV library root
└── movies/                # Radarr: Movie library root
```

## NZBGet Key Settings

- `ParCheck=auto` — only par-check when damage detected
- `ParRepair=yes`, `ParQuick=yes` — quick repair using download-time CRC tracking
- `Unpack=yes`, `UnpackCleanupDisk=yes` — extract then delete archives
- `ArticleCache=512MB`, `DirectWrite=yes` — performance optimizations
- `HealthCheck=park` — parks failed downloads instead of deleting
- `DupeCheck=yes` — prevent duplicate downloads
- `KeepHistory=30` days

## NZBGet Categories

| Category | Used By |
|----------|---------|
| Movies | Radarr |
| Series | Sonarr |
| Music | (manual) |
| Software | (manual) |

## API Access (for testing)

```bash
# NZBGet JSON-RPC
curl -u user:bdzrcw http://localhost:6789/jsonrpc \
  -d '{"method":"status","params":[]}'

# Sonarr API
curl -H "X-Api-Key: e7f9db9cdf1c4222be391dea71175351" \
  http://localhost:8989/api/v3/series

# Radarr API
curl -H "X-Api-Key: f6066280a2ed4cb5989fd16f92d2c1c9" \
  http://localhost:7878/api/v3/movie

# Jackett
curl "http://localhost:9117/api/v2.0/indexers/all/results?apikey=46njq0g1z8a8nzg9ks6zz079xh3ngoo8&q=test"
```

## Data Flow (current)

```
Sonarr/Radarr
  → search Jackett (Newznab API) → indexers
  → select release
  → send NZB to NZBGet (JSON-RPC append)
  → NZBGet downloads via NNTP
  → NZBGet post-processes (PAR2 → RAR → cleanup)
  → files land in /Volumes/Plex/downloads/completed/{Category}/
  → Sonarr/Radarr detect completion (poll NZBGet history)
  → import: rename + move to /Users/jokull/Plex/media/{tv,movies}/
```

## What UDL Replaces

| Current | UDL |
|---------|-----|
| Sonarr (TV monitoring + import) | `udl tv` commands + daemon RSS sync + import |
| Radarr (Movie monitoring + import) | `udl movie` commands + daemon RSS sync + import |
| NZBGet (download + post-process) | Built-in NNTP engine + post-processing pipeline |
| Jackett (indexer proxy) | Direct Newznab client (Jackett was mainly needed for torrent indexers) |

Jackett becomes unnecessary since UDL talks Newznab directly to indexers, and we
don't need torrent indexer support.
