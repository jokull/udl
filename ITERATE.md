# ITERATE.md — Learning Loop for UDL Development

## The Loop

UDL replaces three mature tools (NZBGet, Sonarr, Radarr) that have years of battle-tested patterns. The development loop leverages their source code as a reference library:

```
1. Fork reference projects → ~/Forks/{nzbget,sonarr,radarr}
2. Pick a theme (e.g. "connection resilience", "crash recovery", "queue management")
3. Explore how each project handles that theme
4. Draft a list of improvements for UDL based on findings
5. Implement the most impactful items
6. Test with real downloads
7. Document what worked, update this file, repeat
```

### Setup

```bash
# Clone reference projects (one-time)
cd ~/Forks
git clone https://github.com/nzbgetcom/nzbget.git
git clone https://github.com/Sonarr/Sonarr.git
git clone https://github.com/Radarr/Radarr.git
```

### How to explore a theme

Use Claude Code to search across all three projects simultaneously:

```
# Example: how do they handle connection failures?
grep -r "backoff\|retry\|reconnect" ~/Forks/nzbget/daemon/connect/
grep -r "BackOff\|Retry" ~/Forks/Sonarr/src/NzbDrone.Core/Download/
grep -r "BackOff\|Retry" ~/Forks/Radarr/src/NzbDrone.Core/Download/
```

Key directories to study:
- **NZBGet:** `daemon/connect/`, `daemon/queue/`, `daemon/postprocess/`, `daemon/nntp/`
- **Sonarr:** `src/NzbDrone.Core/Download/`, `src/NzbDrone.Core/MediaFiles/`, `src/NzbDrone.Core/Queue/`
- **Radarr:** Same structure as Sonarr (forked codebase)

## Theme Backlog

Themes to explore, roughly ordered by impact:

| # | Theme | Reference focus | UDL area |
|---|-------|----------------|----------|
| 1 | Crash recovery & resume | NZBGet `daemon/queue/DiskState.cpp` | Pipeline resume |
| 2 | Connection health | NZBGet `daemon/connect/Connection.cpp` | `nntp/pool.go` |
| 3 | Download viability scoring | Sonarr `DecisionEngine/` | `daemon/searcher.go` |
| 4 | Queue prioritization | NZBGet `daemon/queue/QueueCoordinator.cpp` | `daemon/downloader.go` |
| 5 | Media file detection | Sonarr `MediaFiles/MediaFileService.cs` | `postprocess/` |
| 6 | Upgrade logic | Sonarr `DecisionEngine/Specifications/` | `quality/` |
| 7 | Naming & renaming | Sonarr `Organizer/FileNameBuilder.cs` | `organize/` |
| 8 | Health checks | Sonarr `HealthCheck/` | daemon status |
| 9 | Custom format scoring | Radarr `CustomFormats/` | future |

## Completed Themes

### Theme 2: Connection Health (2026-03-02)

**Source:** This defensive improvements plan.

**NZBGet patterns studied:**
- Connection pool with exponential backoff on provider failures
- Read timeouts on NNTP article fetch (prevents stuck workers)
- Segment-level retry before falling back to fill providers
- Per-connection health tracking (consecutive failures → backoff)

**Implemented in UDL:**
- Provider backoff: 5s → 15s → 30s → 60s → 120s → 300s (pool.go)
- 60s read deadline on Body() (conn.go)
- 3-attempt retry in fetchFromPool(), skip retry for NNTP 430 (engine.go)
- Backoff reset on successful Put() (pool.go)

---

## Crash-Resume Analysis

Every step in UDL's pipeline can be interrupted by crash, kill -9, or power loss.
This documents what happens at each step and what recovery looks like.

### Pipeline steps and crash behavior

#### 1. Status → "downloading"
**Crash here:** DB has status="downloading", no files on disk yet.
**Recovery:** On startup, `Downloader.Start()` resets all "downloading" → "queued".
`resetStuckDownloads()` also catches these if started_at > 2h ago.
**Status: HANDLED**

#### 2. NZB fetch (HTTP GET)
**Crash here:** NZB bytes partially received, nothing written to disk.
**Recovery:** Same as step 1 — status is "downloading", gets reset to "queued" on restart.
**Status: HANDLED**

#### 3. NZB parse
**Crash here:** Purely in-memory, nothing to clean up.
**Recovery:** Same as step 1.
**Status: HANDLED**

#### 4. Create download directory
**Crash here:** Empty directory at `incomplete/<id>` may exist.
**Recovery:** Download gets re-queued. New run creates same dir (MkdirAll is idempotent).
On eventual failure, `fail()` cleans up the dir.
**Status: HANDLED** (but orphan dirs from startup reset are NOT cleaned)

#### 5. NNTP segment download
**Crash here:** Partial files in `incomplete/<id>`, some segments written.
**Recovery:** Download gets re-queued. **Full re-download** — no segment-level resume.
The incomplete directory from the crashed attempt exists. The new download creates files
in the same directory (MkdirAll), potentially overwriting partial data.
**Status: PARTIAL** — works but wastes bandwidth re-downloading completed segments.

**Future improvement:** NZBGet tracks per-segment completion in a state file.
On resume, it skips already-completed segments. This requires:
- A segment state file (`incomplete/<id>/segments.json`) tracking completed message IDs
- On resume, filtering the work list to exclude completed segments
- This is the single biggest missing crash-resume feature

#### 6. Status → "post_processing"
**Crash here:** All segments downloaded, status updated. Incomplete dir has full data.
**Recovery:** Download gets re-queued (status was "downloading" or "post_processing"
depending on timing). Re-runs entire pipeline from NZB fetch.
**Status: PARTIAL** — correct but wasteful. Could skip directly to post-processing
if incomplete dir contains all expected files.

**Future improvement:** Check if `incomplete/<id>` already has files matching the NZB.
If segment count matches, skip straight to post-processing.

#### 7. Post-processing (PAR2, RAR, cleanup)
**Crash here:** Files partially extracted/repaired in incomplete dir.
**Recovery:** Full re-download on re-queue. PAR2/RAR extraction is idempotent — running
it again on already-extracted files works (RAR skips existing, PAR2 re-verifies).
**Status: PARTIAL** — works but wasteful.

#### 8. File import (organize.Import)
**Crash here:** This is the critical step. Two sub-cases:
- **Hardlink succeeded, src not yet removed:** Library has the file. On retry,
  hardlink fails (file exists), falls to copy path. Harmless double.
- **Copy in progress (cross-device):** `.udl-tmp` file exists in library.
  On daemon restart, `CleanStaleTmpFiles()` removes stale `.udl-tmp` files.
  Download gets re-queued and re-imported.
- **Copy done, rename done, src not yet removed:** Library has the file, src still
  in incomplete dir. On retry, import tries to link/copy again. If dst exists, it
  will overwrite (os.Create truncates). Wasteful but not corrupt.
**Status: HANDLED** — atomic import prevents corruption, stale tmp cleanup on startup.

#### 9. DB completion (download + history + media status)
**Crash here:** This is now a single transaction (WithTx). Either all three writes
commit or none do.
- **Before commit:** Transaction rolled back. Download stays in prior status.
  On restart, gets re-queued. Library file exists but DB doesn't know → re-import
  overwrites the same file (harmless).
- **After commit, before cleanup:** DB is consistent. Incomplete dir remains.
  On next startup, no "downloading" status to reset. The orphan incomplete dir
  leaks disk space.
**Status: MOSTLY HANDLED** — DB consistency guaranteed by transaction.
Orphan incomplete dirs after successful completion are rare (crash between
tx commit and os.RemoveAll) but could accumulate.

**Future improvement:** Periodic cleanup job that removes `incomplete/<id>` dirs
where the corresponding download ID has status "completed" or "failed".

#### 10. Cleanup incomplete dir
**Crash here:** Dir removal is atomic per the OS. Either removed or not.
**Recovery:** If not removed, it's an orphan. See step 9 note.
**Status: ACCEPTABLE** — minor disk leak, rare.

### Plex pipeline crash behavior

Same analysis applies. The Plex pipeline is simpler (HTTP download → import) so fewer
crash points. The `.part` → final rename within the download dir is atomic.
Cross-device import uses the same `.udl-tmp` pattern.

### Summary: What's missing for full crash-resume

1. **Segment-level resume** (high value): Save segment completion state, skip completed
   segments on retry. NZBGet's `DiskState.cpp` is the reference.
2. **Post-processing resume** (medium): Detect that incomplete dir already has complete
   data and skip to post-processing instead of re-downloading.
3. **Orphan dir cleanup** (low): Periodic sweep of incomplete dirs for completed/failed
   downloads.

These are ordered by impact. Segment-level resume would save the most bandwidth and time,
especially for large downloads that fail near completion.
