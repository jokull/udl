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

### Recycle Bin
Move replaced files to a trash folder instead of deleting. Safety net for upgrades gone wrong.

### Indexer Priority / Weighting
Prefer certain indexers over others. UDL treats all equally.
