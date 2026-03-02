# UDL — Usenet Download Layer

A single Go binary that replaces Sonarr + Radarr + NZBGet for Usenet-based media
automation. CLI-first, daemon mode, opinionated defaults.

## Philosophy

**`go fmt` for media.** One correct way to name and organize files. No naming templates,
no configurable folder structures, no "custom formats". The tool knows the right answer.

- Single binary, single config file, single database
- CLI is the only interface (TUI planned for later)
- Usenet only — no torrents, no abstraction layers for "download clients"
- Built-in NNTP downloader — no external NZBGet/SABnzbd dependency
- Opinionated file organization — hardcoded, Plex/Jellyfin-compatible naming
- Minimal config surface — provider credentials, indexer keys, library paths, done

## Filesystem Convention (non-negotiable)

Folders use spaces (human-readable), filenames use dots (scene-style).

```
{library_root}/
├── tv/
│   └── {Series Name} ({Year})/
│       └── Season {00}/
│           └── {Series.Name}.S{00}E{00}.{Episode.Title}.{Quality}.{ext}
└── movies/
    └── {Movie Name} ({Year})/
        └── {Movie.Name}.{Year}.{Quality}.{ext}
```

Quality tags: `SDTV`, `HDTV-720p`, `WEBDL-1080p`, `Bluray-1080p`, `WEBDL-2160p`, etc.

No user-configurable naming. No tokens. This is the way.

## What UDL Does

1. You tell it what TV shows and movies you want (via CLI)
2. It periodically checks your Newznab indexers for new releases (RSS sync)
3. It picks the best release based on simple quality rules
4. It downloads from Usenet directly (NNTP with multi-connection)
5. It verifies (PAR2), repairs if needed, extracts (RAR), cleans up
6. It renames and moves files to your library in the One True Format
7. Plex/Jellyfin picks up the new files automatically

## Name

UDL = **U**senet **D**ownload **L**ayer. Short, unix-friendly, easy to type.

## Documents

- [ARCHITECTURE.md](ARCHITECTURE.md) — System design, components, data flow
- [RESEARCH.md](RESEARCH.md) — Go libraries, ecosystem analysis, dependency decisions
- [CURRENT-SETUP.md](CURRENT-SETUP.md) — Documentation of the existing local setup we're replacing
- [PIPELINE.md](PIPELINE.md) — Detailed download and post-processing pipeline design
