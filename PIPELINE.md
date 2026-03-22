# Download & Post-Processing Pipeline

The core of UDL — replacing NZBGet's download engine.

## Pipeline Stages

```
┌─────────────────────────────────────────────────────┐
│  1. NZB Acquisition                                 │
│     Fetch NZB from indexer via t=get                │
│     Parse XML → list of files → list of segments    │
│     Sort: media files first, PAR2 parity files last │
│     Create download job in SQLite                   │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│  2. Segment Download (NNTP)                         │
│     For each file, for each segment:                │
│       → Fetch article via NNTP BODY <message-id>    │
│       → Decode yEnc body → raw bytes                │
│       → Write to output file (DirectWrite mode)     │
│     Track: success/failure per segment              │
│     Retry failed segments on alternate providers    │
│     Health tracking: if too many failures, park job │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│  3. PAR2 Rename (fast, always runs)                 │
│     Read PAR2 file headers (not full data)          │
│     Map obfuscated filenames → original names       │
│     Rename files on disk                            │
│     ~2 seconds for a typical download               │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│  4. PAR2 Verify + Repair (conditional)              │
│     IF any segments failed during download:         │
│       → Run par2 verify <file.par2>                 │
│       → If damaged + repairable:                    │
│           → Download additional PAR2 blocks if needed│
│           → Run par2 repair <file.par2>             │
│       → If damaged + unrepairable:                  │
│           → Mark download as failed                 │
│     IF all segments succeeded:                      │
│       → Skip (NZBGet's "QuickPar" optimization)     │
│     Shell out to par2 for v1                  │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│  5. RAR Extraction                                  │
│     Detect RAR archives (multi-volume or single)    │
│     Extract using nwaples/rardecode                 │
│     Extract to _unpack/ subdirectory                │
│     On success: delete RAR source files             │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│  6. Cleanup                                         │
│     Delete: .par2, .sfv, .nfo, .txt, sample files   │
│     Delete: .rar, .r00, .r01, etc. (already done)   │
│     Keep: media files (.mkv, .mp4, .avi, .srt, etc) │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│  7. Import (rename + move to library)               │
│     Identify largest media file → the main file     │
│     Match to media entry (movie or episode)         │
│     Generate canonical filename:                    │
│       Movie: "Movie Name (Year) [WEBDL-1080p].mkv"  │
│       TV: "Show - S01E05 - Title [WEBDL-1080p].mkv" │
│     Create directory structure if needed            │
│     Hardlink if same filesystem, else copy+delete   │
│     Import subtitle files (.srt, .sub, .ass) too    │
│     Update database: status=downloaded, file_path=…  │
└─────────────────────────────────────────────────────┘
```

## NNTP Download Engine Design

### Connection Pool

```
Provider "newshosting" ──→ Pool of 30 NNTP connections
Provider "tweaknews"   ──→ Pool of 20 NNTP connections
Provider "usenet.farm" ──→ Pool of 8  NNTP connections
```

- Connections are lazy (created on demand, up to configured max)
- Auto-reconnect on disconnect
- Keepalive via periodic STAT commands (or just reconnect on failure)
- Each connection handles one segment at a time

### Segment Scheduler

```
NZB parsed → segment queue (priority ordered)
  │
  ├── Level 0 providers tried first (round-robin across pools)
  │   ├── Success → write decoded bytes to output file
  │   └── Failure (article not found) → mark segment for Level 1
  │
  └── Level 1 providers tried for failed segments
      ├── Success → write decoded bytes
      └── Failure → segment permanently failed
```

Segments within a file are downloaded roughly in order (enables streaming/DirectWrite)
but the scheduler can interleave segments from different files for better throughput.

### DirectWrite Mode

Instead of downloading each segment to a temp file and assembling later:
1. Pre-allocate the output file at the expected size
2. Write each decoded segment directly to its correct byte offset
3. Use sparse file support if available
4. Avoids a full copy step after download

This is how NZBGet achieves high performance. We replicate this approach.

### Health Tracking

For each download job, track:
- `total_articles` — from NZB
- `success_articles` — downloaded OK
- `failed_articles` — permanently failed (all providers)
- `health = success / total` (as permille, 1000 = 100%)
- `critical_health` — minimum health needed (based on PAR2 availability)

If health drops below critical_health, park the download (stop wasting bandwidth
on an unrepairable download).

## Post-Processing: PAR2 Strategy

### Why PAR2 Matters

Usenet articles can go missing from servers (DMCA takedowns, retention expiry,
server issues). PAR2 files contain Reed-Solomon recovery data that can reconstruct
missing data blocks.

### Our Approach (v1)

Shell out to `par2`:

```go
// Verify
cmd := exec.Command("par2", "verify", par2File)
err := cmd.Run()
// exit 0 = all good
// exit 1 = repair possible
// exit 2 = repair not possible (insufficient blocks)

// Repair (if verify indicated damage)
cmd = exec.Command("par2", "repair", par2File)
err = cmd.Run()
```

### QuickPar Optimization

NZBGet's killer feature: it knows exactly which segments failed during download
and can tell par2 which blocks are damaged without doing a full verification scan.

For v1, we lose this optimization and do a standard verify+repair cycle.
For v2, we could pass the list of known-bad segments to a custom PAR2 implementation
to skip the verification scan.

## Post-Processing: RAR Extraction

Many Usenet releases are split into RAR volumes:
```
release.part01.rar
release.part02.rar
...
release.part48.rar
```

Using `nwaples/rardecode` v2:
```go
// Open first volume — it automatically finds subsequent volumes
r, err := rardecode.OpenReader(firstRarFile)
for {
    header, err := r.Next()
    // ... extract each file
}
```

Or use `golift/xtractr` for a higher-level API with queuing and progress callbacks.

## Import: The Opinionated Renamer

No templates. No tokens. One function per media type.

### Movie Import

```go
func moviePath(root, title string, year int, quality string, ext string) string {
    dir := fmt.Sprintf("%s (%d)", title, year)
    file := fmt.Sprintf("%s (%d) [%s]%s", title, year, quality, ext)
    return filepath.Join(root, dir, file)
}
// → "Movies/Dune Part Two (2024)/Dune Part Two (2024) [WEBDL-1080p].mkv"
```

### TV Episode Import

```go
func episodePath(root, series string, year, season, episode int, epTitle, quality, ext string) string {
    seriesDir := fmt.Sprintf("%s (%d)", series, year)
    seasonDir := fmt.Sprintf("Season %02d", season)
    file := fmt.Sprintf("%s - S%02dE%02d - %s [%s]%s", series, season, episode, epTitle, quality, ext)
    return filepath.Join(root, seriesDir, seasonDir, file)
}
// → "TV/Severance (2022)/Season 02/Severance - S02E01 - Hello, Ms. Cobel [WEBDL-1080p].mkv"
```

### Subtitle Import

Subtitles follow the same name as the media file with language suffix:
```
Severance - S02E01 - Hello, Ms. Cobel [WEBDL-1080p].en.srt
Severance - S02E01 - Hello, Ms. Cobel [WEBDL-1080p].is.srt
```

### File Move Strategy

1. If source and destination are on the same filesystem → hardlink (instant, no copy)
2. If different filesystem → copy + verify (checksum) + delete source
3. Never do a bare `rename()` across filesystems (it would fail)
4. Create directories as needed with `os.MkdirAll`

## Concurrency Model

```
Main goroutine
  │
  ├── Scheduler goroutine (triggers RSS sync, cleanup)
  │
  ├── Download manager goroutine
  │     ├── N worker goroutines per NNTP connection
  │     └── Segment channel for work distribution
  │
  └── Post-processing goroutine(s)
        └── One job at a time (sequential pipeline)
        └── Can run PAR2 while next download is in progress
```

Downloads and post-processing can run in parallel (one job downloading while
another is in post-processing), but post-processing itself is sequential per job.

## Error Handling

| Error | Action |
|-------|--------|
| Segment not found on any server | Mark failed, continue downloading rest |
| Too many failed segments (health < critical) | Park download, log warning |
| PAR2 repair failed | Mark download as failed, keep files for manual inspection |
| RAR extraction failed | Mark download as failed, keep files |
| Disk full | Pause all downloads, log error, wait for space |
| NNTP auth failure | Disable provider, log error, continue with others |
| Indexer rate limited | Back off exponentially, retry |
| Network timeout | Retry with exponential backoff, max 3 retries |
