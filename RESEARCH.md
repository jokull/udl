# Research — Go Libraries & Ecosystem

## Dependency Decisions

### Definite Picks

| Need | Library | Stars | Why |
|------|---------|-------|-----|
| CLI framework | [spf13/cobra](https://github.com/spf13/cobra) | 43k | Industry standard, used by kubectl/docker/gh |
| Scheduler | [robfig/cron](https://github.com/robfig/cron) v3 | 14k | Proven, simple, cron expressions |
| SQLite | [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | — | Pure Go, no CGo, single static binary |
| RAR extraction | [nwaples/rardecode](https://github.com/nwaples/rardecode) v2 | 146 | Pure Go, RAR3+RAR5, multi-volume, active (Dec 2025) |
| TMDB client | [cyruzin/golang-tmdb](https://github.com/cyruzin/golang-tmdb) | 156 | Active (Jan 2026), v3+v4 API support |
| Newznab client | [mrobinsn/go-newznab](https://github.com/mrobinsn/go-newznab) | 19 | Most complete Go Newznab implementation |
| TOML config | stdlib or [BurntSushi/toml](https://github.com/BurntSushi/toml) | 4.6k | Simple, well-supported |

### Likely Picks (may fork/wrap)

| Need | Library | Stars | Notes |
|------|---------|-------|-------|
| NNTP client | [Tensai75/nntp](https://github.com/Tensai75/nntp) | 0 | Fork of chrisfarms, actively maintained (Feb 2026) |
| NNTP pool | [Tensai75/nntpPool](https://github.com/Tensai75/nntpPool) | 0 | Connection pooling, used in nzb-monkey-go |
| Extraction (high-level) | [golift/xtractr](https://github.com/golift/xtractr) | 47 | Queued extraction, built for *arr ecosystem |

### Build Ourselves

| Need | Approach | Rationale |
|------|----------|-----------|
| NZB parser | `encoding/xml` structs | ~50 lines, trivial XML format |
| yEnc decoder | Custom implementation | ~100 lines, simple byte-shift encoding |
| Release parser | Regex-based parser | Core domain logic, needs to be tailored |
| File renamer | `fmt.Sprintf` + path helpers | Opinionated = hardcoded = simple |

### Shell Out

| Need | External Tool | Rationale |
|------|--------------|-----------|
| PAR2 verify+repair | `par2cmdline` | Only battle-tested option; pure Go (gopar) is experimental |

### Stdlib

| Need | Library | Rationale |
|------|---------|-----------|
| Daemon IPC | `net/rpc` + `encoding/gob` | Unix socket, binary encoding, zero deps. Frozen since Go 1.8 but stable under Go 1 compat guarantee. Perfect for Go-only CLI↔daemon. |
| Structured logging | `log/slog` | Stdlib since Go 1.21, structured, zero deps |

### Skip / Defer

| Need | Decision | Rationale |
|------|----------|-----------|
| TVDB v4 client | Skip | TMDB only — has TV data + TVDB/IMDB cross-refs for Newznab |
| TUI | Defer | Phase 2 — bubbletea when CLI is stable |
| Web UI | Skip | Out of scope — CLI/TUI only |
| Torrent support | Skip | Usenet only, forever |
| gRPC / protobuf | Skip | Codegen overhead not justified for internal IPC |

## Existing Go Projects (Reference)

### autobrr (2,518 stars, very active)
- https://github.com/autobrr/autobrr
- Best reference for Go media automation architecture
- Uses: cobra, modernc.org/sqlite, Newznab RSS parsing
- NOT a downloader — pushes to SABnzbd/NZBGet/*arr
- Study for: project structure, Newznab integration patterns, SQLite usage

### nzb-monkey-go (112 stars, active)
- https://github.com/Tensai75/nzb-monkey-go
- NZB search tool with direct Usenet header search
- Uses: Tensai75/nntp, Tensai75/nntpPool
- Study for: NNTP connection management, NZB creation

### gonzbee (21 stars, abandoned)
- https://github.com/danielmorsing/gonzbee
- Most complete Go NZB downloader reference
- Has: NNTP client, yEnc decoder, PAR2 verifier, NZB parser
- Study for: download pipeline architecture, segment scheduling

### dashotv/flame (2 stars)
- https://github.com/dashotv/flame
- NZB + torrent download service
- Uses SABnzbd under the hood for NZB
- Study for: download management abstraction

## NNTP Protocol Essentials

For the download engine, we need these NNTP commands:

```
CONNECT host:port (TLS)
AUTHINFO USER <username>
AUTHINFO PASS <password>
ARTICLE <message-id>     # fetch full article (headers + body)
BODY <message-id>         # fetch body only (slightly more efficient)
STAT <message-id>         # check if article exists (no data transfer)
QUIT
```

Connection pooling strategy:
- One pool per provider
- Pool size = configured `connections` count
- Lazy connect — establish connections as needed, up to max
- Keepalive/reconnect on failure
- Segment scheduler distributes work across pool

## yEnc Format

yEnc is a simple binary-to-text encoding used for Usenet binary posts:
- Each byte is encoded as `(byte + 42) mod 256`
- Escape character `=` followed by `(byte + 42 + 64) mod 256` for special chars
- Header line: `=ybegin part=N line=128 size=XXXXX name=filename`
- Part header: `=ypart begin=N end=M`
- Trailer: `=yend size=XXXXX part=N pcrc32=XXXXXXXX`
- CRC32 checksum for integrity verification

Decoding is ~100 lines of Go. Performance matters at scale (decoding gigabytes) so
use a byte-level loop, avoid allocations, stream directly to output file.

## Newznab API Summary

Endpoints we need:

| Endpoint | Use |
|----------|-----|
| `t=caps` | Discover indexer capabilities on first connect |
| `t=search&q=...` | General text search (fallback) |
| `t=tvsearch&tvdbid=X&season=Y&ep=Z` | TV episode search |
| `t=movie&imdbid=X` | Movie search |
| `t=get&id=GUID` | Download NZB file |

Response is RSS 2.0 XML with `newznab:attr` extensions for size, category, tvdbid, imdb, etc.

API limits: typically 5-50 requests per 10 seconds depending on indexer tier.

## PAR2 Internals

PAR2 uses Reed-Solomon error correction:
- Original files split into data blocks (typically 512KB-4MB each)
- Recovery blocks computed via Reed-Solomon over GF(2^16)
- Any N recovery blocks can repair any N missing/damaged data blocks
- NZBGet optimization: knows exactly which segments failed during download,
  skips full verification and goes straight to targeted repair

For v1: shell out to `par2cmdline`. It's available via `brew install par2cmdline`
and handles all the complexity. Command interface:

```bash
par2 verify <par2file>              # check integrity
par2 repair <par2file>              # repair if possible
```

Exit codes: 0 = ok, 1 = repair needed + successful, 2 = repair needed + insufficient blocks

## Release Title Parsing

Example titles and what we extract:

```
"The.Last.of.Us.S01E03.1080p.WEB-DL.DD5.1.H.264-GROUP"
 → series="The Last of Us", season=1, episode=3, quality=WEBDL-1080p, group=GROUP

"Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-GROUP"
 → movie="Dune Part Two", year=2024, quality=WEBDL-2160p, group=GROUP

"Movie.Name.2024.1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1-GROUP"
 → movie="Movie Name", year=2024, quality=Remux-1080p, group=GROUP
```

Key patterns to detect:
- Season/episode: `S\d{2}E\d{2}`, `\d{1,2}x\d{2}`
- Quality source: `WEB-DL`, `WEBRip`, `BluRay`, `HDTV`, `DVDRip`, `REMUX`
- Resolution: `2160p`, `1080p`, `720p`, `480p`
- Year: `(19|20)\d{2}` (distinguishes movies from TV in ambiguous cases)
- Group: `-([A-Za-z0-9]+)$`

Start with a simple regex-based parser. Expand coverage iteratively by testing
against real releases from our indexers.
