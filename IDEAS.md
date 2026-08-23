# IDEAS.md — Feature Backlog

Features beloved in Radarr/Sonarr/NZBGet that are still missing from UDL, roughly ordered by impact.

## High Impact — Daily Workflow

### Notifications
Discord/webhook/Pushover on grab/complete/fail. For a headless daemon, this is how users know anything happened.

### Lists / Auto-Import
IMDB watchlist, Trakt lists, Plex watchlist, TMDB trending. People add movies by starring them on IMDB, not by typing CLI commands. This is the #1 way content gets added in practice for most Radarr/Sonarr users.

### Season Packs
Sonarr grabs full-season NZBs when available (often better quality, better availability than individual episodes). UDL only searches per-episode.

### Segment-Level Resume
Already identified in ITERATE.md as highest-value missing feature. On a 20GB download that fails at 90%, you re-download the entire thing. NZBGet saves per-segment state.

## Medium Impact — Quality of Life

### Custom Format Scoring / Release Group Preferences
Prefer x265 over x264, prefer specific groups (SPARKS, FraMeSToR), penalize YIFY/CAM. Current scoring is quality-tier + size only — no way to express "I want HDR" or "avoid YIFY."

### Minimum Availability (Movies)
Radarr's "released" vs "in cinemas" vs "announced" filter. Without this, UDL will try to grab cams/screeners for movies still in theaters.

### Calendar / Upcoming
Sonarr's calendar showing air dates for monitored series. UDL has `air_date` in the DB but no way to view "what's coming this week." Could be `udl tv calendar`.

### Cutoff Unmet View
"Show me everything that could be upgraded." UDL has upgrade logic in `ShouldGrab` but no command to surface what's below preferred quality. Could be `udl wanted --upgradeable`.

## Lower Impact but Beloved by Power Users

### Speed Throttling / Scheduling
Limit to 50Mbps during the day, full speed at night. NZBGet's scheduler is heavily used.

### Post-Processing Scripts / Hooks
Run a custom script after import (update Plex, send notification, trigger Filebot, etc.). UDL's pipeline is hardcoded.

### Connection / Download Stats
NZBGet shows server health, average speed, data transferred. UDL has no stats tracking.

### Multi-Language Awareness
Radarr/Sonarr track release language. UDL treats everything as English — no way to prefer or filter by language.


## Shipped — Shadow Libraries (theme overlays)

`udl shadow` builds a curated, deduplicated view of friend Plex libraries, later
mounted so the local PMS sees shadow files as local. Motivation: friend libraries
hold Icelandic-dubbed kids' content that was deleted locally; the shadow restores
it as a base layer with the user's own files on top.

- Status: CLI + judge shipped and verified against live data (Aug 2026). Mount phase
  shipped: NFS union server + block cache + `udl shadow mount`/`unmount`, verified
  end-to-end (movies listed over NFS, Aladdin 4096 B matched direct HTTP, Bluey S03
  merged 40 local + 40 shadow episodes).

### CLI

- `udl shadow create <name> --type movie|show --mount <path> [--prefer 4k,1080,720,...]`
- `udl shadow add <name> <server> "<section>"` — curate themes one friend section at a time
- `udl shadow list` / `udl shadow sources <name>` / `udl shadow manifest <name> [--json]`
- State: `~/.config/udl/shadow.toml` (defs), `~/.config/udl/shadow/<name>.json` (manifest).
  CLI-direct, no daemon RPC — the running launchd daemon is untouched, no restart.

### Judge semantics (verified against real data)

- Dedupe across sources by external ID (tmdb:// then imdb://) when tagged —
  friends title the same movie differently (Icelandic vs English) but share IDs;
  untagged items group by normalized title + year (+ S/E for episodes); a title
  group merges into a compatible ID group so a tagged and an untagged copy of the
  same movie still dedupe.
- Distinct years under one title stay separate (Lilo & Stitch 2002 vs 2025 remake;
  Dýrin í Hálsaskógi 2012 vs 2016 — different TMDB IDs, correctly both kept).
- Rank: `prefer` order (default 4k/1080/720/480/sd), tie-break source order then size.
- Episodes group by SERIES title, not episode title — generic dub episode names
  ("Þáttur 1", "Episode 24") collide across shows otherwise.
- Verified: `dubbed` 396 candidates → 302 kept / 94 dupes / ~1.0 TB;
  `dubbed-tv` 1027 candidates → 889 kept / 138 dupes / 336 GB.

### Layered mount plan (decision: synthetic-union NFS via go-nfs)

- Requirement: PMS scans `/Users/jokull/Plex/media/dubbed` (+ `dubbed-tv`); the dirs
  already hold real files; shadow items must appear below them, local wins.
- macOS has no native union fs. Chosen: a pure-Go NFS server (willscott/go-nfs)
  whose READDIR merges the local upper dir + manifest items (name collision → local
  wins) and whose READ serves local files directly or streams friend URLs through
  an on-disk block cache (2 MiB LRU blocks keyed by URL+offset, sequential prefetch).
  Client is the same machine (localhost). No kernel extension.
- `udl shadow mount <name>` does it all: moves existing local files to `<mount>.upper`
  (one-time, keeps the user's files; leftover empty upper dirs from failed runs are
  cleaned), starts the NFS server on 127.0.0.1 (auto port) — the server accepts
  connections BEFORE `mount_nfs` runs, since mount_nfs blocks on its MNT RPC until
  the server answers — runs the PMS tuning step, and
  `sudo mount -o port=N,mountport=N,vers=3,nolocks,resvport -t nfs localhost:/` at
  the mount path (vers=3: go-nfs is NFSv3 only; nolocks: no NLM support). Mount has
  a 120s timeout; on failure the local files are auto-restored (retried, since
  Spotlight can briefly hold the moved dirs). Staged progress output throughout,
  plus a 15s heartbeat with cache stats while serving. Ctrl-C unmounts;
  `udl shadow unmount <name>` for manual cleanup. Note the mount path resolves
  symlinks: `/Users/jokull/Plex` → `/Volumes/Plex`.
- Union semantics: directory names merge recursively; a file name in both layers
  resolves to the local copy. Bluey is the test: user's Icelandic-titled files
  (`Blæja (Bluey) - s03e36 - Mold.mp4`) coexist with the shadow's English-titled
  episodes (`Bluey (2018) - S03E02 - Bedroom.mp4`) in the same Season dir.
- PMS tuning: `GenerateBIFBehavior=never` (the current key on PMS 1.43; the old
  `GenerateBIFrames` is gone) — already `never` on this server; the mount command
  re-asserts it. Preview thumbnails pull whole files during scan.
- Surviving restarts: `udl shadow enable <name>` installs a per-user LaunchAgent
  (`~/Library/LaunchAgents/com.jokull.udl-shadow-<name>`, runs `udl shadow mount
  <name> --daemon`) with a fixed NFS port (first free from 2055, saved in
  shadow.toml). At login the agent serves the union and runs
  `sudo -n mount -o port=N,mountport=N,vers=3,nolocks,resvport -t nfs 127.0.0.1:/`
  at the mount path (one-time passwordless sudo rule for /sbin/mount,/sbin/umount
  in /etc/sudoers.d/udl-shadow); KeepAlive restarts it on crash, and the mount
  survives agent restarts (the client reconnects to the same port). Why not a
  system LaunchDaemon: macOS System Policy denies system-domain processes both
  `file-mount` and `file-read-data` on /Volumes/Plex (kernel log: "System Policy:
  mount_nfs deny(1) file-mount"; "udl deny(1) file-read-data") — user-session
  processes are approved. Why not automountd: its NFS client on macOS 26 cannot
  complete the mount handshake with a loopback go-nfs server ("NFS server not
  responding" despite a live server, correct port/mountport, and explicit
  127.0.0.1) — the agent's own `sudo -n mount` is the proven path.
  `udl shadow disable <name>` removes agent, plist, and any automount map.
  No code signing needed; the dev certificate only matters later for
  SMAppService/privileged helpers or notarized distribution.
- Gotcha fixed in production: the go-nfs file-handle cache defaults must exceed
  the union's path count (889 items + ~150 dirs) — with 1024 handles the LRU
  evicted handles mid-scan and clients got ESTALE, which made the PMS scanner
  silently skip new shows (stuck at 0%). Now 100k; regression-covered by a full
  recursive walk through NFS (TestLiveFullWalk: 1038 files, no stale handles).
- Fallback if go-nfs proves flaky on macOS: go-fuse + macFUSE.
- New dependency this phase: willscott/go-nfs (first new dep since the constraint;
  deliberate).

### Indexer Priority / Weighting
Prefer certain indexers over others. UDL treats all equally.
