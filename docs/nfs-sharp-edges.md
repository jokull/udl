# Shadow libraries over NFS — the sharp edges

Everything learned building `udl shadow` (manifest → judge → NFS union → Plex) on
macOS 26.5.2 (mini-1975, Apple Silicon). Each section: symptom, root cause, fix.
This file exists so the next person does not re-dig these holes.

## The working architecture (TL;DR)

- **Manifest**: `udl shadow manifest <name>` — CLI-direct, writes
  `~/.config/udl/shadow/<name>.json` (no daemon restart).
- **Serve**: per-user LaunchAgent (`~/Library/LaunchAgents/com.jokull.udl-shadow-<name>`)
  running `udl shadow mount <name> --daemon` — fixed NFS port (first free from
  2055, saved in shadow.toml), KeepAlive restart on crash.
- **Mount**: the agent attempts `sudo -n mount` at login (passwordless sudoers
  rule: `jokull ALL=(root) NOPASSWD: /sbin/mount, /sbin/umount` in
  `/etc/sudoers.d/udl-shadow`), bounded to 20s with 3 retries because System
  Policy intermittently stalls agent mounts (below). A mount made by any
  approved process survives agent restarts — if the agent's attempt stalls,
  mounting once from a terminal fixes it until the next reboot. The long-term
  fix is signing the binary with a stable developer certificate so the System
  Policy identity stops changing per rebuild.
- **Upper layer**: local files live in `~/.udl-shadow/<name>` on the **boot
  volume** — never under a volume that contains NFS mounts (see System Policy
  below). `<mount>.upper` is the historical default; override per shadow with
  `upper = "..."` in shadow.toml.
- **PMS tuning**: `GenerateBIFBehavior=never` (global) and per-section
  `enableIntroMarkerGeneration` / `enableCreditsMarkerGeneration` /
  `enableAdMarkerGeneration` / `enableBIFGeneration` / `enableVoiceActivityGeneration`
  = 0 on the shadow's own library sections.

## macOS 26 System Policy — the big one

Symptom: a launchd-spawned process (LaunchDaemon *or* per-user LaunchAgent)
reads a path under `/Volumes/Plex` and **hangs in `open(2)` forever**. Kernel log:

```
System Policy: udl(-1) deny(4) file-read-data /Volumes/Plex/...
System Policy: mount_nfs(8594) deny(1) file-mount /Volumes/Plex/dubbed-tv
```

Root causes, in order of discovery:

1. **System-domain LaunchDaemons are denied everything on `/Volumes`** —
   `file-mount` (mounts) and `file-read-data` (reading files) both. Terminal
   `sudo mount` works because user-session processes are approved. This is why
   the daemon could not mount *or* serve.
2. **Per-user LaunchAgents are approved — except under a volume that contains
   an NFS mount.** On this macOS build, System Policy intermittently stalls
   `file-read-data` for launchd-spawned processes on paths whose volume carries
   an NFS mount. `/Volumes/Plex` contains the dubbed-tv NFS mount, so every
   `.upper` dir on that volume was a lottery: an agent spawned early worked,
   every freshly-spawned agent hung (deny(4) stalls the syscall — a hang, not
   an error). The TCC grants `kTCCServiceSystemPolicyNetworkVolumes` and
   `...RemovableVolumes` for `/Users/jokull/bin/udl` (auth_value 2) do **not**
   fix it. The stall also hits the agent's own `mount_nfs` (`deny(4)
   file-mount`) intermittently — mounts made from a terminal always work, and
   the mount survives the mounting process exiting, so a single terminal mount
   recovers a stalled agent. Kernel-log evidence:
   `System Policy: udl(-1) deny(4) file-read-data ...` / `mount_nfs(...) deny(4)
   file-mount ...`.
3. **Fix: upper layers live on the boot volume** (`~/.udl-shadow/<name>`).
   The agent reads those fine. The mountpoints stay on `/Volumes/Plex` (PMS
   paths); only the union's *local* files move. (Constraint: the boot volume
   must have room — see the mv warning below.)
4. **Stable code signature lifts the restriction.** The stall is the System
   Policy identity changing per rebuild (adhoc signatures). Signing
   `~/bin/udl` with a stable identity — a self-signed Code Signing
   certificate created in Keychain Access (`udl-signer`, Code Signing type;
   `codesign --force --sign udl-signer ~/bin/udl`; re-sign after every
   rebuild) — makes the identity stable, and the upper layer can then live
   under the NFS-containing volume again. The `movies` shadow uses
   `/Volumes/Plex/media/movies.upper` (the daemon's import target) with the
   signed binary. (The Apple Development cert failed with
   `errSecInternalComponent` in every context — keychain ACL/chain state —
   hence the self-signed route; the chain warning on self-signed roots is
   expected and harmless.)

Related: re-signing `udl` (ad-hoc signatures change per build) can invalidate
TCC identity. If you ever see a privacy prompt for "udl", grant it. Signing
with a stable developer certificate would make the identity permanent.

## mount_nfs client details

- **`port=` is not enough — you must pass `mountport=` too.** Without
  `mountport`, mount_nfs asks the portmapper (port 111) for the MOUNT service;
  there is no portmap on localhost → "NFS server localhost not responding".
- **Use `127.0.0.1`, not `localhost`.** mount_nfs resolves `localhost` to `::1`
  first; the server binds IPv4 loopback only → connection refused → "not
  responding". (The go-nfs README's `localhost` example is a trap on macOS.)
- **`vers=3`** — go-nfs speaks NFSv3 only; macOS may negotiate v4.
- **`nolocks`** — go-nfs has no NLM; without it the client may stall on lock
  negotiation.
- **`resvport`** — harmless as root, standard for NFS.
- Full working option string:
  `mount -o port=N,mountport=N,vers=3,nolocks,resvport -t nfs 127.0.0.1:/ <path>`

## Server lifecycle

- **The server must accept connections BEFORE mount_nfs runs.** mount_nfs blocks
  on its MNT RPC until the server answers; mounting after the server starts is
  a self-deadlock that looks like a hang.
- **Orphaned mounts are infectious.** Ctrl-C the serving process without a
  successful unmount → the mount table entry stays while the server is gone →
  every stat/readdir/readlink on that path hangs on RPC timeouts, wedging any
  process that touches it (including the next daemon, which hangs in
  `filepath.EvalSymlinks`). Never `EvalSymlinks` a path that might be a dead
  mount; resolve with readlink-only and check the mount table (`/sbin/mount`,
  which never blocks) first.
- **autofs triggers hang the same way.** Stat'ing a path that is an autofs
  trigger makes automountd attempt the mount against whatever server it finds —
  if the server isn't up yet (the very process doing the stat), the stat blocks.
  Serve-only daemons must never touch the mountpoint path.

## go-nfs file handles

- The `CachingHandler` LRU must comfortably exceed the total number of paths in
  the export (items + dirs). With 889 items + ~150 dirs the default 1024 is too
  small: the LRU evicts handles mid-walk and clients get **ESTALE**, which makes
  a PMS scan silently skip entries and stall at 0%. We serve with
  `NewCachingHandlerWithVerifierLimit(handler, 100000, 100000)`.
- `FromHandle` iterates the whole LRU on every RPC (O(limit) per request), so
  an oversized limit costs a few ms per RPC. 100k is fine for this workload.

## automountd (do not use)

`auto_master`/`auto_udl` direct maps look like the clean answer (System
Policy-approved, boot-safe) but on this macOS build automountd's NFS client
cannot complete the mount handshake with a loopback go-nfs server at all:
"NFS server 127.0.0.1 not responding" with a live server, correct
port/mountport, and explicit 127.0.0.1. Also: after a failed attempt the
trigger caches the failure and won't retry until `automount -u` + `-vc`, and
an armed trigger blocks the agent's own `mount` at the same path. The agent's
own `sudo -n mount` is the reliable path.

## launchd specifics

- **LaunchAgent bootstrap races the bootout**: rebooting an agent
  (`bootout` then immediately `bootstrap` the same label) fails with
  `Bootstrap failed: 5: Input/output error` while the old instance finishes
  shutting down. Retry the bootstrap a few times with a ~2s sleep.
- A launchd job whose process died in a kernel stall can linger as
  "running" in `launchctl print` with no visible process, holding its port —
  the socket is invisible to `lsof` and `ps` from the user session until
  launchd reaps it. `netstat -an` still shows the LISTEN.
- Serve-only daemons should create their upper dir at startup (an empty
  mountpoint/upper from a failed run is otherwise never created).

## Plex side

- **`allowSync`/`allowDownloads` lie about download capability.** Vader and
  kari report 0 yet stream fine; Plex reports 1 yet refuses ranged GETs.
  `udl plex libraries --probe` does a real ranged GET on the first item of each
  section — trust FETCH, not DOWNLOAD.
- **Preview thumbnails and marker analysis pull whole files.** Disable
  `GenerateBIFBehavior=never` (the current key; the old `GenerateBIFrames` is
  gone) and the per-section marker/BIF keys. Note `enableIntroMarkerGeneration`
  is show-only — PUTting it on a movie section returns 400; the other four
  keys are fine on both.
- **Scan symptoms**: a scan "stuck at 0%" with no reads flowing usually means
  the server is broken (see ESTALE and System Policy above), not that Plex is
  slow. Check the serving process's heartbeat, the cache block count, and the
  kernel log for `System Policy` denials.

## Boot volume — cache caps

Cap alone was not enough: eviction ran only every 64 fetches, so a fully
warm cache sat over its cap forever (13 GB under an 8 GB cap) and the disk
still filled. Fixed in `BlockCache`: trim on startup plus a 5-minute periodic
eviction, so over-cap caches converge without new fetches.

## Data safety — the mv trap

**A cross-volume `mv` unlinks each source file as it copies.** Moving a large
tree (29 GB) to a nearly-full volume (13 GB free) copied until ENOSPC — but
the already-copied files were deleted from the source as it went, and cleaning
up the partial copy destroyed the only remaining copies. Result: 138 local
files lost; no APFS snapshots or Time Machine existed. Rules:

- `cp` + verify counts + only then remove the source. Never `mv` big trees to
  constrained volumes.
- Check `df -h /` before copying to the boot volume.
- Recovery: check `tmutil listlocalsnapshots /` and
  `tmutil listbackups` FIRST — before deleting anything.

## Diagnostic playbook

- Who owns a port: `netstat -an | grep LISTEN` (lsof misses root-owned sockets
  from the user session).
- Is the server answering RPCs: the nfs-client probe
  (DialServiceAtPort → Mount → ReadDirPlus) — a working server returns entries
  in <1s.
- Where a hung process is stuck: `kill -QUIT <pid>` dumps Go goroutine stacks
  to the agent log; look for `syscall.wait4` (child hang), `open(2)` via
  `os.ReadDir` (System Policy stall), or `kevent` idle (nothing running).
- System Policy denials: `log show --last 10m --predicate 'eventMessage
  CONTAINS[c] "deny"'`.
