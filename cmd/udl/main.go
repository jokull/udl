package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/daemon"
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/logging"
	"github.com/jokull/udl/internal/migrate"
	"github.com/jokull/udl/internal/plex"
	"github.com/jokull/udl/internal/shadow"
	"github.com/jokull/udl/internal/shadowfs"
	"github.com/jokull/udl/internal/tmdb"
)

// Set by goreleaser ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "udl",
	Short: "Usenet Download Layer",
	Long:  "A single binary replacing Sonarr + Radarr + NZBGet for Usenet media automation.",
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Start the UDL daemon",
	RunE:  runDaemon,
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE:  runStatus,
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check the whole install and say what to do about anything wrong",
	Long: "Runs every health check plus the operational ones that a status summary\n" +
		"hides — parked items, reclaimable partials, cooling friends, oversized\n" +
		"logs — and pairs each problem with the command that addresses it.",
	RunE: runDoctor,
}

func runDoctor(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.DoctorReply
	if err := client.Call("Service.Doctor", &daemon.Empty{}, &reply); err != nil {
		return err
	}
	if len(reply.Findings) == 0 {
		fmt.Println("nothing to check")
		return nil
	}

	for _, f := range reply.Findings {
		mark := "✓"
		switch f.Status {
		case "error":
			mark = "✗"
		case "warning":
			mark = "!"
		}
		fmt.Printf("  %s %s — %s\n", mark, f.Name, f.Message)
		if f.Hint != "" && f.Status != "ok" {
			for _, line := range strings.Split(f.Hint, "\n") {
				fmt.Printf("      %s\n", strings.TrimSpace(line))
			}
		}
	}
	fmt.Println()
	switch {
	case reply.Errors > 0:
		fmt.Printf("%d error(s), %d warning(s), %d ok\n", reply.Errors, reply.Warnings, reply.Healthy)
	case reply.Warnings > 0:
		fmt.Printf("%d warning(s), %d ok\n", reply.Warnings, reply.Healthy)
	default:
		fmt.Printf("all %d checks ok\n", reply.Healthy)
	}
	return nil
}

var movieCmd = &cobra.Command{
	Use:   "movie",
	Short: "Manage movies",
}

var movieAddCmd = &cobra.Command{
	Use:   "add [tmdb-id]",
	Short: "Add a movie by TMDB ID",
	Long:  "Adds a movie by its TMDB ID. Use 'udl movie search' first to find the ID.",
	Args:  cobra.ExactArgs(1),
	RunE:  runMovieAdd,
}

var movieOwnCmd = &cobra.Command{
	Use:   "own [tmdb-id]",
	Short: "Download a movie locally past its shadow status",
	Long: `Explicitly download a movie that a shadow already provides. The default
for a monitored and shadow-covered movie is to NOT download it (status
"shadow" — available via the mount). This command opts into owning it
locally: status becomes "wanted" and the indexers are searched immediately.`,
	Args: cobra.ExactArgs(1),
	RunE: runMovieOwn,
}

func runMovieOwn(cmd *cobra.Command, args []string) error {
	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("TMDB ID must be a number (use 'udl movie search' to find it)")
	}
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.OwnMovieReply
	if err := client.Call("Service.OwnMovie", &daemon.OwnMovieArgs{TMDBID: tmdbID}, &reply); err != nil {
		return err
	}
	switch reply.Status {
	case "downloaded":
		fmt.Printf("already owned locally: %s (%d) [tmdb=%d]\n", reply.Title, reply.Year, reply.TmdbID)
	case "wanted":
		fmt.Printf("owning: %s (%d) [tmdb=%d]\n", reply.Title, reply.Year, reply.TmdbID)
		if reply.Grabbed {
			fmt.Println("  -> release found and enqueued for download")
		} else {
			fmt.Println("  -> no matching release found on indexers; will retry in the search cycle")
		}
	default:
		fmt.Printf("owning: %s (%d) [tmdb=%d] (status %s)\n", reply.Title, reply.Year, reply.TmdbID, reply.Status)
	}
	return nil
}

var movieListCmd = &cobra.Command{
	Use:   "list",
	Short: "List wanted and downloaded movies",
	RunE:  runMovieList,
}

var movieSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search TMDB for movies",
	Long:  "Searches TMDB and shows results with TMDB IDs. Use the ID with 'udl movie add'.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMovieSearch,
}

var movieReleasesCmd = &cobra.Command{
	Use:   "releases [tmdb-id-or-title]",
	Short: "Search indexers for a movie in the database",
	Long:  "Searches Usenet indexers for releases matching a movie already in the database.\nAccepts TMDB ID or title.",
	Args:  cobra.ExactArgs(1),
	RunE:  runMovieReleases,
}

var movieGrabCmd = &cobra.Command{
	Use:   "grab [tmdb-id-or-title] [#]",
	Short: "Grab a specific indexer release for a movie",
	Long:  "Searches indexers for a movie in the database and grabs the release at the given index.\nRun 'udl movie releases' first to see numbered results, then grab by number.\nAccepts TMDB ID or title.",
	Args:  cobra.ExactArgs(2),
	RunE:  runMovieGrab,
}

var tvCmd = &cobra.Command{
	Use:   "tv",
	Short: "Manage TV series",
}

var tvAddCmd = &cobra.Command{
	Use:   "add [tmdb-id]",
	Short: "Add a TV series by TMDB ID",
	Long:  "Adds a TV series by its TMDB ID. Use 'udl tv search' first to find the ID.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTVAdd,
}

var tvSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search TMDB for TV series",
	Long:  "Searches TMDB and shows results with TMDB IDs. Use the ID with 'udl tv add'.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runTVSearch,
}

var tvListCmd = &cobra.Command{
	Use:   "list",
	Short: "List monitored series",
	RunE:  runTVList,
}

var queueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Show download queue",
	RunE:  runQueue,
}

var queuePauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Pause all downloads",
	RunE:  runQueuePause,
}

var queueResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume all downloads",
	RunE:  runQueueResume,
}

var queueClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear all queued/downloading entries",
	RunE:  runQueueClear,
}

var queueRetryCmd = &cobra.Command{
	Use:   "retry [movie:TMDB_ID|episode:TMDB_ID:S01E02]",
	Short: "Retry failed downloads (all or by category:id)",
	Args:  cobra.RangeArgs(0, 1),
	RunE:  runQueueRetry,
}

var plexProbeCmd = &cobra.Command{
	Use:   "probe",
	Short: "Measure each Plex friend's current speed and reliability",
	Long: "Reads a small range from a file each friend is known to have, through the\n" +
		"same transport downloads use. Reputation is otherwise learned only from\n" +
		"traffic, so a friend that is never chosen is never measured.",
	RunE: runPlexProbe,
}

func runPlexProbe(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	server, _ := cmd.Flags().GetString("server")
	mb, _ := cmd.Flags().GetInt("mb")

	rpcArgs := &daemon.PlexProbeArgs{Server: server}
	if mb > 0 {
		rpcArgs.Bytes = int64(mb) << 20
	}
	var reply daemon.PlexProbeReply
	if err := client.Call("Service.PlexProbe", rpcArgs, &reply); err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tRESULT\tSPEED\tSAMPLE\tRELIABILITY")
	for _, r := range reply.Results {
		speed := "—"
		if r.OK && r.MBps > 0 {
			speed = fmt.Sprintf("%.1f MB/s", r.MBps)
		}
		result := "ok"
		if !r.OK {
			result = "FAILED: " + r.Failed
		}
		reliability := "no history"
		if r.Attempts > 0 {
			reliability = fmt.Sprintf("%d%% of %d", r.Success, r.Attempts)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Server, result, speed, r.Sample, reliability)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("readings are not recorded as attempts; ranking learns from real transfers")
	return nil
}

var queueEvictCmd = &cobra.Command{
	Use:   "evict [movie:TMDB_ID|episode:TMDB_ID:S01E02]",
	Short: "Cancel a single download and reset to wanted",
	Args:  cobra.ExactArgs(1),
	RunE:  runQueueEvict,
}

var movieRemoveCmd = &cobra.Command{
	Use:   "remove [tmdb-id-or-title]",
	Short: "Remove a movie and delete its files from disk",
	Long:  "Remove a movie by TMDB ID or title. Use --keep-files to keep files on disk.",
	Args:  cobra.ExactArgs(1),
	RunE:  runMovieRemove,
}

var tvRemoveCmd = &cobra.Command{
	Use:   "remove [tmdb-id-or-title]",
	Short: "Remove a series from monitoring (not from disk)",
	Long:  "Remove a series by TMDB ID or title.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTVRemove,
}

var tvRefreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Refresh episode metadata from TMDB for all monitored series",
	RunE:  runTVRefresh,
}

var tvMonitorCmd = &cobra.Command{
	Use:   "monitor [tmdb-id-or-title]",
	Short: "Show or change season monitoring for a series",
	Long: `Show or change which seasons are monitored for a series.
Accepts TMDB ID or title.

Examples:
  udl tv monitor 1396                  # show monitoring status
  udl tv monitor "Breaking Bad" --all  # monitor everything
  udl tv monitor 1396 --season 3       # monitor S03
  udl tv monitor 1396 --season 3 --off # unmonitor S03
  udl tv monitor 1396 --latest         # only latest season
  udl tv monitor 1396 --none           # unmonitor everything`,
	Args: cobra.ExactArgs(1),
	RunE: runTVMonitor,
}

var plexCmd = &cobra.Command{
	Use:   "plex",
	Short: "Plex friends integration",
}

var plexServersCmd = &cobra.Command{
	Use:   "servers",
	Short: "List shared Plex servers from friends",
	RunE:  runPlexServers,
}

var plexCheckCmd = &cobra.Command{
	Use:   "check [tmdb-id]",
	Short: "Check if a movie or TV series is available on friends' Plex servers",
	Long:  "Check by TMDB ID. Resolves automatically to a movie or series from the local DB. For TV, use --season/--episode to filter the output to a specific slice.",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlexCheck,
}

var plexCleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "List unwatched media older than N days as delete candidates",
	Long:  "Read-only delete-candidate report for AI handoff. Queries Plex watch history on your owned server and lists items never watched, added more than --days ago, with per-row watch count, last-watched, size, and safety hints (shadow coverage, rarity, holiday, ...). Nothing is deleted.",
	RunE:  runPlexCleanup,
}
var plexLibrariesCmd = &cobra.Command{
	Use:   "libraries",
	Short: "Map all Plex libraries: type, download access, audio languages",
	Long: `Lists every library section on your own and friends' Plex servers with
its media type and whether this account can download from it (offline sync).

By default this makes one request to plex.tv plus one per reachable server.
Additive flags are each rate-limited to --rate requests/second:
  --counts  fetch per-section item counts (1 request per section)
  --audio   scan items for audio track languages (1 request per item scanned,
            capped by --items, so use it sparingly)`,
	RunE: runPlexLibraries,
}

var shadowCmd = &cobra.Command{
	Use:   "shadow",
	Short: "Virtual shadow libraries from friends' Plex servers",
}

var shadowCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a new shadow library",
	Long:  "Creates a shadow definition. Use --type movie|show and --mount <path>. Add friend libraries with 'udl shadow add'.",
	Args:  cobra.ExactArgs(1),
	RunE:  runShadowCreate,
}

var shadowAddCmd = &cobra.Command{
	Use:   "add [shadow] [server] [section]",
	Short: "Add a friend's library section to a shadow",
	Long:  "Adds one friend library section as a source. List what's addable with 'udl shadow sources <name>'.",
	Args:  cobra.ExactArgs(3),
	RunE:  runShadowAdd,
}

var shadowListCmd = &cobra.Command{
	Use:   "list",
	Short: "List shadow libraries",
	RunE:  runShadowList,
}

var shadowCoveredCmd = &cobra.Command{
	Use:   "covered [tmdb-id]",
	Short: "Show which shadows already provide a movie",
	Long: `Reports whether the built shadow manifests carry the movie (matched by
tmdb:// then imdb:// GUID). A covered movie is available via the mount
without a download; 'udl movie own' still downloads it locally if you
want to own it.`,
	Args: cobra.ExactArgs(1),
	RunE: runShadowCovered,
}

func runShadowCovered(cmd *cobra.Command, args []string) error {
	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("TMDB ID must be a number")
	}
	cov, err := shadow.Covered(tmdbID, "")
	if err != nil {
		return fmt.Errorf("load shadow manifests: %w", err)
	}
	if len(cov) == 0 {
		fmt.Printf("tmdb %d: not covered by any shadow\n", tmdbID)
		return nil
	}
	for _, c := range cov {
		fmt.Printf("%s: %s (%s)\n", c.Shadow, c.Title, c.GUID)
	}
	return nil
}

var shadowSourcesCmd = &cobra.Command{
	Use:   "sources [shadow]",
	Short: "Show friend servers and their sections available to add",
	Args:  cobra.ExactArgs(1),
	RunE:  runShadowSources,
}

var shadowManifestCmd = &cobra.Command{
	Use:   "manifest [shadow]",
	Short: "Build the judged, flattened manifest for a shadow",
	Long:  "Scans every added source, dedupes candidates across sources, ranks by resolution preference, and writes the manifest to ~/.config/udl/shadow/<name>.json. Use --json to print it.",
	Args:  cobra.ExactArgs(1),
	RunE:  runShadowManifest,
}

var shadowMountCmd = &cobra.Command{
	Use:          "mount [shadow]",
	Short:        "Mount a shadow as a local NFS filesystem",
	Long:         "Serves the union of your local files (moved to <mount>.upper) and the shadow manifest over NFSv3, then mounts it at the shadow's mount path so Plex sees both layers. Directory names merge; your files win name collisions. The mount itself needs sudo; Ctrl-C unmounts.",
	Args:         cobra.ExactArgs(1),
	RunE:         runShadowMount,
	SilenceUsage: true,
}

var shadowUnmountCmd = &cobra.Command{
	Use:          "unmount [shadow]",
	Short:        "Unmount a mounted shadow",
	Args:         cobra.ExactArgs(1),
	RunE:         runShadowUnmount,
	SilenceUsage: true,
}

var shadowEnableCmd = &cobra.Command{
	Use:          "enable [shadow]",
	Short:        "Serve and mount a shadow at boot via a LaunchDaemon",
	Long:         "Installs a root LaunchDaemon that serves the shadow and mounts it at the shadow's mount path when the machine boots, restarting it if it crashes. The mount survives reboots. Uses sudo for the install; remove with 'udl shadow disable <name>'.",
	Args:         cobra.ExactArgs(1),
	RunE:         runShadowEnable,
	SilenceUsage: true,
}

var shadowDisableCmd = &cobra.Command{
	Use:          "disable [shadow]",
	Short:        "Remove a shadow's LaunchDaemon and unmount it",
	Args:         cobra.ExactArgs(1),
	RunE:         runShadowDisable,
	SilenceUsage: true,
}

var blocklistCmd = &cobra.Command{
	Use:   "blocklist",
	Short: "Show blocklisted releases",
	RunE:  runBlocklist,
}

var blocklistClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear all blocklist entries",
	RunE:  runBlocklistClear,
}

var blocklistRemoveCmd = &cobra.Command{
	Use:   "remove [id]",
	Short: "Remove blocklist entries by id, media, or reason",
	Long: "Removes one entry by id, or every matching entry by filter.\n\n" +
		"  udl blocklist remove 518\n" +
		"  udl blocklist remove --media movie:1419406\n" +
		"  udl blocklist remove --reason 'HTTP 403'",
	Args: cobra.MaximumNArgs(1),
	RunE: runBlocklistRemove,
}

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Show download history",
	RunE:  runHistory,
}

var libraryCmd = &cobra.Command{
	Use:   "library",
	Short: "Library management",
}

var libraryImportCmd = &cobra.Command{
	Use:   "import [dir]",
	Short: "Scan a directory, identify media via TMDB, and import to library",
	Long:  "Dry-run by default. Use --execute to actually move files and update the database.",
	Args:  cobra.ExactArgs(1),
	RunE:  runLibraryImport,
}

var libraryCleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Scan library for orphan files, misnamed files, and missing files",
	Long:  "Dry-run by default. Use --rename --execute to fix misnamed files. Use --delete --execute to remove orphans.",
	RunE:  runLibraryCleanup,
}

var libraryPruneIncompleteCmd = &cobra.Command{
	Use:   "prune-incomplete",
	Short: "Remove orphan incomplete download directories",
	Long:  "Scans the incomplete directory for dirs whose download has completed, failed, or no longer exists. Dry-run by default.",
	RunE:  runLibraryPruneIncomplete,
}

var libraryVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Check library consistency (missing files, orphans, misnamed)",
	Long:  "Read-only check that reports all issues without making changes.\nUse --fix to claim orphan files by matching them against wanted/failed media.",
	RunE:  runLibraryVerify,
}

var libraryPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Delete files for unmonitored episodes",
	Long:  "Dry-run by default. Use --execute to actually delete files.",
	RunE:  runLibraryPrune,
}

var tvReleasesCmd = &cobra.Command{
	Use:   "releases [tmdb-id-or-title]",
	Short: "Search indexers for episode releases",
	Long:  "Searches Usenet indexers for releases matching a specific episode.\nRequires --season and --episode flags.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTVReleases,
}

var tvGrabCmd = &cobra.Command{
	Use:   "grab [tmdb-id-or-title] [#]",
	Short: "Grab a specific indexer release for an episode",
	Long:  "Searches indexers for an episode and grabs the release at the given index.\nRun 'udl tv releases' first to see numbered results, then grab by number.\nRequires --season and --episode flags.",
	Args:  cobra.ExactArgs(2),
	RunE:  runTVGrab,
}

var tvEpisodesCmd = &cobra.Command{
	Use:   "episodes [tmdb-id-or-title]",
	Short: "Show episodes for a series",
	Long:  "Shows all episodes or a specific season for a series in the database.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTVEpisodes,
}

var movieDeleteCmd = &cobra.Command{
	Use:   "delete [tmdb-id-or-title]",
	Short: "Delete a movie's file and reset to wanted",
	Long:  "Delete the downloaded file for a movie and reset it to wanted for re-download.\nDry-run by default; use --execute to actually delete.\nUse --search to blocklist the old NZB and re-search.",
	Args:  cobra.ExactArgs(1),
	RunE:  runMovieDelete,
}

var wantedCmd = &cobra.Command{
	Use:   "wanted",
	Short: "Show all wanted movies and episodes",
	RunE:  runWanted,
}

var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Show upcoming episodes",
	RunE:  runSchedule,
}

var tvDeleteCmd = &cobra.Command{
	Use:   "delete [title-or-tmdb-id]",
	Short: "Delete files for a series, season, or episode",
	Long:  "Delete files and reset episodes to wanted. Dry-run by default. Use --execute to actually delete.\nWhen --episode is specified, keeps episode monitored. Use --search to re-search immediately.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTVDelete,
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Import media from Sonarr/Radarr",
}

var migrateRadarrCmd = &cobra.Command{
	Use:   "radarr",
	Short: "Import movies from Radarr",
	Long:  "Fetches all monitored movies from Radarr and adds them to UDL.\nDry-run by default; use --execute to write to the database.",
	RunE:  runMigrateRadarr,
}

var migrateSonarrCmd = &cobra.Command{
	Use:   "sonarr",
	Short: "Import series from Sonarr",
	Long:  "Fetches all monitored series from Sonarr, resolves TVDB→TMDB IDs,\nand adds series + episodes to UDL.\nDry-run by default; use --execute to write to the database.",
	RunE:  runMigrateSonarr,
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a starter config file",
	Long:  "Creates ~/.config/udl/config.toml with a documented template. Will not overwrite an existing file.",
	RunE:  runInit,
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage configuration",
}

var configCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate config file",
	RunE:  runConfigCheck,
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print config file path",
	RunE:  runConfigPath,
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show active configuration (quality profile, indexers, paths)",
	RunE:  runConfigShow,
}

var movieInfoCmd = &cobra.Command{
	Use:   "info [tmdb-id-or-title]",
	Short: "Show full details for a movie",
	Long:  "Show status, quality, file, IDs, download state, history, and blocklist for a movie.",
	Args:  cobra.ExactArgs(1),
	RunE:  runMovieInfo,
}

var movieReconcileCmd = &cobra.Command{
	Use:          "reconcile",
	Short:        "Reconcile tracked movies against the shadow movies library",
	Long:         "Matches tracked movies against the 'movies' shadow manifest (by tmdb/imdb GUID). Downloaded movies the shadow covers become 'shadow' (available via the mount, never re-downloaded); downloaded movies it does not cover revert to 'wanted' so UDL re-acquires them. Dry-run by default; use --execute.",
	RunE:         runMovieReconcile,
	SilenceUsage: true,
}

var tvInfoCmd = &cobra.Command{
	Use:   "info [tmdb-id-or-title]",
	Short: "Show full details for a series",
	Long:  "Show status, IDs, episode counts, active downloads, and history for a series.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTVInfo,
}

var searchTriggerCmd = &cobra.Command{
	Use:   "search-trigger",
	Short: "Trigger immediate indexer search for wanted items",
	Long: `Force an immediate indexer search instead of waiting for the scheduler.

Examples:
  udl search-trigger                              # search all wanted items
  udl search-trigger --tmdb 1057823               # search a specific movie
  udl search-trigger --tmdb 42282 --season 6 --episode 1  # search a specific episode
  udl search-trigger "Girls"                      # search by title`,
	Args: cobra.RangeArgs(0, 1),
	RunE: runSearchTrigger,
}

func init() {
	movieRemoveCmd.Flags().Bool("keep-files", false, "Only remove from database, keep files on disk")
	movieDeleteCmd.Flags().Bool("execute", false, "Actually delete files (default is dry-run)")
	movieDeleteCmd.Flags().Bool("search", false, "Re-search after delete (blocklists old NZB)")
	tvMonitorCmd.Flags().IntP("season", "s", -1, "Season number to monitor/unmonitor")
	tvMonitorCmd.Flags().Bool("off", false, "Unmonitor the specified season")
	tvMonitorCmd.Flags().Bool("latest", false, "Monitor only the latest season")
	tvMonitorCmd.Flags().Bool("all", false, "Monitor all seasons")
	tvMonitorCmd.Flags().Bool("none", false, "Unmonitor all seasons")
	tvDeleteCmd.Flags().IntP("season", "s", -1, "Season number to delete (-1 means all)")
	tvDeleteCmd.Flags().IntP("episode", "e", -1, "Episode number to delete (requires --season)")
	tvDeleteCmd.Flags().Bool("execute", false, "Actually delete files (default is dry-run)")
	tvDeleteCmd.Flags().Bool("search", false, "Re-search after delete (blocklists old NZB if UDL-downloaded)")
	tvReleasesCmd.Flags().IntP("season", "s", -1, "Season number (required)")
	tvReleasesCmd.Flags().IntP("episode", "e", -1, "Episode number (required)")
	tvReleasesCmd.MarkFlagRequired("season")
	tvReleasesCmd.MarkFlagRequired("episode")
	tvGrabCmd.Flags().IntP("season", "s", -1, "Season number (required)")
	tvGrabCmd.Flags().IntP("episode", "e", -1, "Episode number (required)")
	tvGrabCmd.MarkFlagRequired("season")
	tvGrabCmd.MarkFlagRequired("episode")
	tvEpisodesCmd.Flags().IntP("season", "s", -1, "Season number (-1 for all)")
	tvCmd.AddCommand(tvAddCmd, tvSearchCmd, tvListCmd, tvRemoveCmd, tvRefreshCmd, tvMonitorCmd, tvDeleteCmd, tvReleasesCmd, tvGrabCmd, tvEpisodesCmd, tvInfoCmd)
	scheduleCmd.Flags().Int("days", 30, "Number of days to look ahead")
	queueClearCmd.Flags().Bool("unmonitored", false, "Only clear unmonitored episodes")
	historyCmd.Flags().String("type", "", "Filter by media type (movie or episode)")
	historyCmd.Flags().String("event", "", "Filter by event (grabbed, completed, failed)")
	historyCmd.Flags().Int("limit", 0, "Maximum number of entries (default 50)")
	historyCmd.Flags().Int("tmdb", 0, "Filter to a specific movie or series by TMDB ID")
	historyCmd.Flags().IntP("season", "s", 0, "Episode season (used with --tmdb)")
	historyCmd.Flags().IntP("episode", "e", 0, "Episode number (used with --tmdb)")
	searchTriggerCmd.Flags().Int("tmdb", 0, "TMDB ID of the movie or series to search")
	searchTriggerCmd.Flags().IntP("season", "s", 0, "Episode season (used with --tmdb)")
	searchTriggerCmd.Flags().IntP("episode", "e", 0, "Episode number (used with --tmdb)")
	movieReconcileCmd.Flags().Bool("execute", false, "Actually apply status changes (default is dry-run)")
	movieCmd.AddCommand(movieAddCmd, movieListCmd, movieSearchCmd, movieReleasesCmd, movieGrabCmd, movieRemoveCmd, movieDeleteCmd, movieInfoCmd, movieReconcileCmd, movieOwnCmd)
	queueCmd.AddCommand(queuePauseCmd, queueResumeCmd, queueClearCmd, queueRetryCmd, queueEvictCmd)
	plexCheckCmd.Flags().IntP("season", "s", 0, "Filter TV results to a specific season")
	plexCheckCmd.Flags().IntP("episode", "e", 0, "Filter TV results to a specific episode")
	plexCleanupCmd.Flags().Int("days", 90, "Minimum days since added to consider for cleanup")
	plexCleanupCmd.Flags().Bool("verbose", false, "Also show items that would be kept")
	plexCmd.AddCommand(plexServersCmd, plexCheckCmd, plexCleanupCmd, plexProbeCmd)
	plexProbeCmd.Flags().String("server", "", "Probe only this friend")
	plexProbeCmd.Flags().Int("mb", 0, "Sample size in MiB per friend (default 2)")
	plexLibrariesCmd.Flags().Bool("counts", false, "Fetch per-section item counts (1 request per section)")
	plexLibrariesCmd.Flags().Bool("probe", false, "Verify real file fetch per section (ranged GET on first item)")
	plexLibrariesCmd.Flags().Bool("audio", false, "Scan audio track languages per section (rate-limited, 1 request per item)")
	plexLibrariesCmd.Flags().Bool("shows", false, "Include TV sections in the audio scan (walks episodes)")
	plexLibrariesCmd.Flags().Int("items", 25, "Max items/episodes scanned per section for --audio")
	plexLibrariesCmd.Flags().Int("per-show", 3, "Max episodes examined per show for --audio")
	plexLibrariesCmd.Flags().Float64("rate", 3, "Max requests per second to Plex servers (0 = unlimited)")
	plexLibrariesCmd.Flags().String("server", "", "Only show this server (name substring)")
	plexLibrariesCmd.Flags().Bool("json", false, "Output JSON")
	plexCmd.AddCommand(plexLibrariesCmd)
	shadowCreateCmd.Flags().String("type", "", "media type: movie or show")
	shadowCreateCmd.Flags().String("mount", "", "local path the Plex library points at")
	shadowCreateCmd.Flags().StringSlice("prefer", nil, "resolution preference, highest first (e.g. 4k,1080,720)")
	shadowManifestCmd.Flags().Bool("json", false, "Print the full manifest as JSON")
	shadowMountCmd.Flags().String("port", "", "NFS port (default: auto-assigned)")
	shadowMountCmd.Flags().Int("cache-size", 50, "block cache size in GiB")
	shadowMountCmd.Flags().Bool("no-tuning", false, "skip disabling Plex preview thumbnails")
	shadowMountCmd.Flags().Bool("daemon", false, "run as a root daemon: serve only, no mount (for launchd + automount)")
	shadowEnableCmd.Flags().String("port", "", "fixed NFS port for the serve daemon (default: first free from 2055)")
	shadowCmd.AddCommand(shadowCreateCmd, shadowAddCmd, shadowListCmd, shadowSourcesCmd, shadowManifestCmd, shadowMountCmd, shadowUnmountCmd, shadowEnableCmd, shadowDisableCmd, shadowCoveredCmd)
	blocklistCmd.AddCommand(blocklistClearCmd, blocklistRemoveCmd)
	blocklistCmd.Flags().Bool("all", false, "Include blocks whose cooldown has lapsed")
	blocklistRemoveCmd.Flags().String("media", "", "Remove entries for a media item, e.g. movie:1419406")
	blocklistRemoveCmd.Flags().String("reason", "", "Remove entries whose reason or release contains this text")
	daemonCmd.Flags().String("log-file", "", "Log file to keep bounded (default ~/Library/Logs/udl.log)")
	configCmd.AddCommand(configCheckCmd, configPathCmd, configShowCmd)

	migrateRadarrCmd.Flags().String("url", "", "Radarr base URL (e.g. http://localhost:7878)")
	migrateRadarrCmd.Flags().String("apikey", "", "Radarr API key")
	migrateRadarrCmd.Flags().Bool("execute", false, "Actually write to database (default is dry-run)")
	migrateRadarrCmd.MarkFlagRequired("url")
	migrateRadarrCmd.MarkFlagRequired("apikey")

	migrateSonarrCmd.Flags().String("url", "", "Sonarr base URL (e.g. http://localhost:8989)")
	migrateSonarrCmd.Flags().String("apikey", "", "Sonarr API key")
	migrateSonarrCmd.Flags().Bool("execute", false, "Actually write to database (default is dry-run)")
	migrateSonarrCmd.MarkFlagRequired("url")
	migrateSonarrCmd.MarkFlagRequired("apikey")

	migrateCmd.AddCommand(migrateRadarrCmd, migrateSonarrCmd)

	libraryImportCmd.Flags().Bool("execute", false, "Actually import files (default is dry-run)")
	libraryCleanupCmd.Flags().Bool("execute", false, "Actually apply changes (default is dry-run)")
	libraryCleanupCmd.Flags().Bool("rename", false, "Fix misnamed files (requires --execute to apply)")
	libraryCleanupCmd.Flags().Bool("delete", false, "Delete orphan files (requires --execute to apply)")
	libraryPruneIncompleteCmd.Flags().Bool("execute", false, "Actually remove directories (default is dry-run)")
	libraryPruneCmd.Flags().Bool("unmonitored", false, "Prune files for unmonitored episodes")
	libraryPruneCmd.Flags().Bool("execute", false, "Actually delete files (default is dry-run)")
	libraryVerifyCmd.Flags().Bool("fix", false, "Claim orphan files by matching against wanted/failed media")
	libraryCmd.AddCommand(libraryImportCmd, libraryCleanupCmd, libraryPruneIncompleteCmd, libraryVerifyCmd, libraryPruneCmd)

	nzbSearchCmd.Flags().String("cat", "", "Newznab category code (e.g. 3010 for music/MP3)")
	nzbGrabCmd.Flags().StringP("output", "o", ".", "Output directory for downloaded files")
	nzbCmd.AddCommand(nzbSearchCmd, nzbGrabCmd)

	rootCmd.AddCommand(daemonCmd, statusCmd, doctorCmd, movieCmd, tvCmd, queueCmd, plexCmd, shadowCmd, historyCmd, blocklistCmd, libraryCmd, migrateCmd, configCmd, wantedCmd, scheduleCmd, searchTriggerCmd, versionCmd, initCmd, nzbCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("udl %s (%s) built %s\n", version, commit, date)
	},
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// --- Daemon ---

// rotateShadowLogs bounds the shadow agents' log files, which launchd holds open
// on their behalf. It rotates immediately, then on the same interval as the
// daemon's own log.
func rotateShadowLogs(ctx context.Context, dir string, log *slog.Logger) {
	rotate := func() {
		for _, p := range logging.ShadowLogPaths(dir) {
			rotated, err := (&logging.Rotator{Path: p}).Rotate()
			switch {
			case err != nil:
				log.Warn("shadow log rotation failed", "path", p, "error", err)
			case rotated:
				log.Info("rotated shadow log", "path", p, "backup", p+".1")
			}
		}
	}
	rotate()

	ticker := time.NewTicker(logging.DefaultInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rotate()
		}
	}
}

// defaultLogPath is where launchd redirects the daemon's output, and therefore
// the file that must be kept from growing without bound.
func defaultLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Logs", "udl.log")
}

func runDaemon(cmd *cobra.Command, args []string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	dataDir, err := config.DataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "udl.db")
	db, err := database.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	log.Info("starting udl daemon",
		"db", dbPath,
		"tv", cfg.Library.TV,
		"movies", cfg.Library.Movies,
		"providers", len(cfg.Usenet.Providers),
		"indexers", len(cfg.Indexers),
		"quality", cfg.Quality.Profile,
	)

	// Context cancelled on SIGINT/SIGTERM for clean shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Keep the log file bounded. launchd holds it open (the plist redirects
	// stdout and stderr here), so it is truncated in place rather than renamed —
	// see internal/logging for why that distinction matters.
	logPath, _ := cmd.Flags().GetString("log-file")
	if logPath == "" {
		logPath = defaultLogPath()
	}
	rotator := &logging.Rotator{
		Path: logPath,
		Notify: func(backup string) {
			log.Info("rotated log file", "path", logPath, "backup", backup)
		},
	}
	go rotator.Run(ctx)

	// The shadow agents' logs are rotated from here for the same reason, and
	// because they cannot be restarted cheaply: their NFS file handles live only
	// in memory, so a restart turns every handle the client holds into ESTALE
	// and drops whatever is streaming. Bounding the file from outside avoids
	// touching them at all.
	go rotateShadowLogs(ctx, filepath.Dir(logPath), log)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down gracefully", "signal", sig)
		cancel()
		// Second signal forces immediate exit.
		sig = <-sigCh
		log.Error("received second signal, forcing exit", "signal", sig)
		os.Exit(1)
	}()

	return daemon.ServeWithContext(ctx, cfg, db, log)
}

// --- CLI commands that talk to the daemon via RPC ---

func runStatus(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.StatusReply
	if err := client.Call("Service.Status", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	// Detect terminal for color output.
	isTTY := isTerminal()

	fmt.Println("daemon: running")
	fmt.Printf("queue: %d items (%d downloading)\n", reply.QueueSize, reply.Downloading)
	fmt.Printf("indexers: %d   movies: %d   series: %d\n", reply.IndexerCount, reply.MovieCount, reply.SeriesCount)
	fmt.Printf("library: %s, %s\n", reply.LibraryMovies, reply.LibraryTV)

	if reply.FailedCount > 0 || reply.BlockedActiveCount > 0 || reply.ParkedCount > 0 {
		fmt.Printf("failed (24h): %d   blocklisted: %d active   parked: %d\n",
			reply.FailedCount, reply.BlockedActiveCount, reply.ParkedCount)
		if reply.ParkedCount > 0 {
			fmt.Printf("parked items burned the grab cap; see 'udl queue' or retry with 'udl queue retry'\n")
		}
	}

	if len(reply.Checks) > 0 {
		fmt.Println()
		fmt.Println("health:")
		for _, c := range reply.Checks {
			sym, color := statusSymbol(c.Status, isTTY)
			if isTTY && color != "" {
				fmt.Printf("  %s%s %s%s — %s\n", color, sym, c.Name, ansiReset, c.Message)
			} else {
				fmt.Printf("  %s %s — %s\n", sym, c.Name, c.Message)
			}
		}
	}
	return nil
}

const (
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiReset  = "\033[0m"
)

func statusSymbol(status string, color bool) (sym string, ansi string) {
	switch status {
	case "ok":
		return "\u2713", ansiGreen
	case "warning":
		return "!", ansiYellow
	case "error":
		return "\u2717", ansiRed
	default:
		return "?", ""
	}
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func runMovieAdd(cmd *cobra.Command, args []string) error {
	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("TMDB ID must be a number (use 'udl movie search' to find it)")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.AddMovieArgs{TMDBID: tmdbID}
	var reply daemon.AddMovieReply
	if err := client.Call("Service.AddMovie", rpcArgs, &reply); err != nil {
		return err
	}

	if reply.AlreadyExists {
		fmt.Printf("already exists: %s (%d) [tmdb=%d] — %s\n", reply.Title, reply.Year, reply.TmdbID, reply.Status)
		if reply.Grabbed {
			fmt.Println("  -> re-searched and enqueued for download")
		}
	} else {
		fmt.Printf("added: %s (%d) [tmdb=%d]\n", reply.Title, reply.Year, reply.TmdbID)
		if reply.ShadowStatus {
			fmt.Printf("  -> available via shadow (%s) — nothing to download; 'udl movie own %d' to download locally\n", reply.ShadowNames, reply.TmdbID)
		} else if reply.Grabbed {
			fmt.Println("  -> release found and enqueued for download")
		} else {
			fmt.Println("  -> no matching release found on indexers")
		}
	}
	if reply.ShadowNames != "" && !reply.ShadowStatus {
		fmt.Printf("  note: also available via shadow (%s) — a different version (e.g. dubbed)\n", reply.ShadowNames)
	}
	return nil
}

func runMovieList(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.MovieListReply
	if err := client.Call("Service.ListMovies", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	if len(reply.Movies) == 0 {
		fmt.Println("no movies")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TMDB ID\tTITLE\tYEAR\tLANG\tSTATUS\tQUALITY\tADDED\tFILE")
	for _, m := range reply.Movies {
		q := ""
		if m.Quality.Valid {
			q = m.Quality.String
		}
		added := ""
		if m.AddedAt.Valid {
			added = m.AddedAt.String
			if len(added) > 10 {
				added = added[:10]
			}
		}
		file := ""
		if m.FilePath.Valid && m.FilePath.String != "" {
			file = filepath.Base(m.FilePath.String)
		}
		lang := ""
		if m.OriginalLanguage.Valid {
			lang = m.OriginalLanguage.String
		}
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", m.TmdbID, m.Title, m.Year, lang, m.Status, q, added, file)
	}
	return w.Flush()
}

func runMovieSearch(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	query := strings.Join(args, " ")
	rpcArgs := &daemon.TMDBSearchMovieArgs{Query: query}
	var reply daemon.TMDBSearchMovieReply
	if err := client.Call("Service.TMDBSearchMovie", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Results) == 0 {
		fmt.Println("no results found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TMDB ID\tTITLE\tYEAR")
	for i, r := range reply.Results {
		fmt.Fprintf(w, "%d\t%s\t%d\n", r.TMDBID, r.Title, r.Year)
		if i >= 19 {
			break // show top 20
		}
	}
	return w.Flush()
}

func runMovieReleases(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.SearchMovieArgs{}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.SearchMovieReply
	if err := client.Call("Service.SearchMovie", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Results) == 0 {
		fmt.Println("no releases found")
		return nil
	}

	printReleases(reply.Results, reply.ExistingQuality)
	return nil
}

func runMovieGrab(cmd *cobra.Command, args []string) error {
	index, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("second argument must be a release number (use 'udl movie releases' to find it)")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.GrabMovieReleaseArgs{Index: index}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.GrabMovieReleaseReply
	if err := client.Call("Service.GrabMovieRelease", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("grabbed: %s (%d)\n", reply.Title, reply.Year)
	fmt.Printf("  release: %s\n", reply.ReleaseName)
	fmt.Printf("  quality: %s\n", reply.Quality)
	return nil
}

func runTVAdd(cmd *cobra.Command, args []string) error {
	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("TMDB ID must be a number (use 'udl tv search' to find it)")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.AddSeriesArgs{TMDBID: tmdbID}
	var reply daemon.AddSeriesReply
	if err := client.Call("Service.AddSeries", rpcArgs, &reply); err != nil {
		return err
	}

	if reply.AlreadyExists {
		fmt.Printf("already exists: %s (%d) [tmdb=%d] — %s\n", reply.Title, reply.Year, reply.TmdbID, reply.Status)
	} else {
		fmt.Printf("added: %s (%d) [tmdb=%d] — %d episodes\n", reply.Title, reply.Year, reply.TmdbID, reply.EpisodeCount)
		if reply.Grabbed > 0 {
			fmt.Printf("  -> %d episodes enqueued for download\n", reply.Grabbed)
		}
	}
	return nil
}

func runTVSearch(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	query := strings.Join(args, " ")
	rpcArgs := &daemon.TMDBSearchSeriesArgs{Query: query}
	var reply daemon.TMDBSearchSeriesReply
	if err := client.Call("Service.TMDBSearchSeries", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Results) == 0 {
		fmt.Println("no results found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TMDB ID\tTITLE\tYEAR")
	for i, r := range reply.Results {
		fmt.Fprintf(w, "%d\t%s\t%d\n", r.TMDBID, r.Title, r.Year)
		if i >= 19 {
			break // show top 20
		}
	}
	return w.Flush()
}

func runTVList(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.SeriesListReply
	if err := client.Call("Service.ListSeries", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	if len(reply.Series) == 0 {
		fmt.Println("no series")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TMDB ID\tTITLE\tYEAR\tLANG\tSTATUS\tEPS\tWANTED\tHAVE")
	for _, s := range reply.Series {
		counts := reply.Counts[s.ID]
		lang := ""
		if s.OriginalLanguage.Valid {
			lang = s.OriginalLanguage.String
		}
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%d\t%d\t%d\n", s.TmdbID, s.Title, s.Year, lang, s.Status, counts[0], counts[1], counts[2])
	}
	return w.Flush()
}

func runQueue(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.QueueReply
	if err := client.Call("Service.Queue", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	if len(reply.Items) == 0 {
		fmt.Println("queue empty")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tPROGRESS\tSIZE\tERROR")
	for _, d := range reply.Items {
		progress := d.Progress
		if progress > 100 {
			progress = 100
		}
		id := mediaTag(d.Category, d.TmdbID, d.Season, d.EpisodeNum, d.MediaID)
		progressStr := fmt.Sprintf("%.0f%%", progress)
		size := "-"
		if d.SizeBytes.Valid && d.SizeBytes.Int64 > 0 {
			size = formatSize(d.SizeBytes.Int64)
		}
		errMsg := ""
		if d.ErrorMsg.Valid && d.ErrorMsg.String != "" {
			errMsg = d.ErrorMsg.String
			// During post_processing, download_error holds phase labels, not errors.
			if d.Status == "post_processing" && isPhaseLabel(errMsg) {
				errMsg = "[phase] " + errMsg
			}
			if len(errMsg) > 60 {
				errMsg = errMsg[:60] + "..."
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", id, d.Title, d.Status, progressStr, size, errMsg)
	}
	return w.Flush()
}

func runQueuePause(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	if err := client.Call("Service.PauseAll", &daemon.Empty{}, &daemon.Empty{}); err != nil {
		return err
	}
	fmt.Println("downloads paused")
	return nil
}

func runQueueClear(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	unmonitored, _ := cmd.Flags().GetBool("unmonitored")
	rpcArgs := &daemon.ClearQueueArgs{Unmonitored: unmonitored}
	var reply daemon.ClearQueueReply
	if err := client.Call("Service.ClearQueue", rpcArgs, &reply); err != nil {
		return err
	}
	if unmonitored {
		fmt.Printf("cleared %d unmonitored downloads\n", reply.Cleared)
	} else {
		fmt.Printf("cleared %d downloads\n", reply.Cleared)
	}
	return nil
}

func runQueueResume(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	if err := client.Call("Service.ResumeAll", &daemon.Empty{}, &daemon.Empty{}); err != nil {
		return err
	}
	fmt.Println("downloads resumed")
	return nil
}

func runHistory(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	mediaType, _ := cmd.Flags().GetString("type")
	event, _ := cmd.Flags().GetString("event")
	limit, _ := cmd.Flags().GetInt("limit")
	tmdbID, _ := cmd.Flags().GetInt("tmdb")
	season, _ := cmd.Flags().GetInt("season")
	episode, _ := cmd.Flags().GetInt("episode")

	rpcArgs := &daemon.HistoryArgs{MediaType: mediaType, Event: event, Limit: limit, TmdbID: tmdbID, Season: season, Episode: episode}
	var reply daemon.HistoryReply
	if err := client.Call("Service.History", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Events) == 0 {
		fmt.Println("no history")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tEVENT\tQUALITY\tSOURCE\tTIME")
	for _, h := range reply.Events {
		q := ""
		if h.Quality.Valid {
			q = h.Quality.String
		}
		source := ""
		if h.Source.Valid {
			source = h.Source.String
		}
		id := mediaTag(h.MediaType, h.TmdbID, h.Season, h.EpisodeNum, h.MediaID)
		createdAt := "—"
		if h.CreatedAt.Valid {
			createdAt = h.CreatedAt.String
			if len(createdAt) > 16 {
				createdAt = createdAt[:16]
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", id, h.Title, h.Event, q, source, createdAt)
	}
	return w.Flush()
}

func runBlocklist(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.BlocklistReply
	if err := client.Call("Service.Blocklist", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	showAll, _ := cmd.Flags().GetBool("all")

	var shown []database.BlocklistEntry
	for _, e := range reply.Entries {
		if e.Active || showAll {
			shown = append(shown, e)
		}
	}
	if len(shown) == 0 {
		if len(reply.Entries) == 0 {
			fmt.Println("blocklist empty")
		} else {
			fmt.Printf("no active blocks (%d lapsed; use --all to see them)\n", len(reply.Entries))
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tMEDIA\tRELEASE\tCLASS\tUNTIL\tREASON\tTIME")
	lapsed := 0
	for _, e := range shown {
		reason := e.Reason
		if len(reason) > 48 {
			reason = reason[:48] + "..."
		}
		media := mediaTag(e.MediaType, e.TmdbID, e.Season, e.EpisodeNum, e.MediaID)
		createdAt := shortTime(e.CreatedAt)
		class := string(e.FailureClass)
		if class == "" {
			class = "—"
		}
		until := "permanent"
		if e.ExpiresAt.Valid {
			until = shortTime(e.ExpiresAt)
			if !e.Active {
				until = "lapsed " + until
				lapsed++
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, media, e.ReleaseTitle, class, until, reason, createdAt)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	active := len(shown) - lapsed
	fmt.Printf("%d active block(s)", active)
	if lapsed > 0 {
		fmt.Printf(", %d lapsed", lapsed)
	}
	fmt.Println()
	return nil
}

// shortTime renders a database timestamp for a table cell.
func shortTime(ts sql.NullString) string {
	if !ts.Valid || ts.String == "" {
		return "—"
	}
	out := ts.String
	if t, err := time.Parse(time.RFC3339, out); err == nil {
		return t.UTC().Format("2006-01-02 15:04")
	}
	if len(out) > 16 {
		return out[:16]
	}
	return out
}

func runBlocklistClear(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.BlocklistClearReply
	if err := client.Call("Service.BlocklistClear", &daemon.Empty{}, &reply); err != nil {
		return err
	}
	fmt.Printf("cleared %d blocklist entries\n", reply.Cleared)
	return nil
}

func runBlocklistRemove(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	media, _ := cmd.Flags().GetString("media")
	reason, _ := cmd.Flags().GetString("reason")

	if len(args) == 0 && media == "" && reason == "" {
		return fmt.Errorf("give an id, or one of --media/--reason")
	}

	rpcArgs := &daemon.BlocklistRemoveArgs{Media: media, Reason: reason}
	if len(args) > 0 {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid blocklist ID %q: %w", args[0], err)
		}
		rpcArgs.ID = id
	}
	if err := client.Call("Service.BlocklistRemove", rpcArgs, &daemon.Empty{}); err != nil {
		return err
	}
	switch {
	case rpcArgs.ID != 0:
		fmt.Printf("removed blocklist entry %d\n", rpcArgs.ID)
	case media != "" || reason != "":
		fmt.Printf("removed blocklist entries matching media=%q reason=%q\n", media, reason)
	}
	return nil
}

func runMovieRemove(cmd *cobra.Command, args []string) error {
	keepFiles, _ := cmd.Flags().GetBool("keep-files")

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.RemoveMovieArgs{KeepFiles: keepFiles}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.RemoveMovieReply
	if err := client.Call("Service.RemoveMovie", rpcArgs, &reply); err != nil {
		return err
	}
	fmt.Printf("removed: %s (%d) [tmdb=%d]\n", reply.Title, reply.Year, reply.TmdbID)
	return nil
}

func runTVRemove(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.RemoveSeriesArgs{}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.RemoveSeriesReply
	if err := client.Call("Service.RemoveSeries", rpcArgs, &reply); err != nil {
		return err
	}
	fmt.Printf("removed: %s (%d) [tmdb=%d] (and all episodes)\n", reply.Title, reply.Year, reply.TmdbID)
	return nil
}

func runTVDelete(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	season, _ := cmd.Flags().GetInt("season")
	episode, _ := cmd.Flags().GetInt("episode")
	execute, _ := cmd.Flags().GetBool("execute")
	search, _ := cmd.Flags().GetBool("search")

	if episode >= 0 && season < 0 {
		return fmt.Errorf("--episode requires --season")
	}

	rpcArgs := &daemon.TVDeleteArgs{
		Season:  season,
		Episode: episode,
		Execute: execute,
		Search:  search,
	}

	// Accept TMDB ID or title.
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.TVDeleteReply
	if err := client.Call("Service.TVDelete", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Items) == 0 {
		fmt.Println("no downloaded files found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tEPISODE\tSIZE\tPATH")
	for _, item := range reply.Items {
		action := "delete"
		if item.Deleted {
			action = "deleted"
		}
		ep := fmt.Sprintf("%s S%02dE%02d", item.SeriesTitle, item.Season, item.Episode)
		if item.EpTitle != "" {
			ep += " " + item.EpTitle
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", action, ep, formatSize(item.SizeBytes), item.FilePath)
	}
	w.Flush()

	if execute {
		fmt.Printf("\n%d files deleted, %s reclaimed\n", len(reply.Items), formatSize(reply.TotalBytes))
		if search {
			fmt.Println("re-search triggered")
		}
	} else {
		fmt.Printf("\n%d files, %s total (dry-run: use --execute to delete)\n", len(reply.Items), formatSize(reply.TotalBytes))
	}
	return nil
}

func runLibraryPrune(cmd *cobra.Command, args []string) error {
	unmonitored, _ := cmd.Flags().GetBool("unmonitored")
	if !unmonitored {
		return fmt.Errorf("--unmonitored flag is required")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	execute, _ := cmd.Flags().GetBool("execute")

	rpcArgs := &daemon.LibraryPruneArgs{Unmonitored: true, Execute: execute}
	var reply daemon.LibraryPruneReply
	if err := client.Call("Service.LibraryPrune", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Items) == 0 {
		fmt.Println("no unmonitored downloaded files found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tSERIES\tEPISODE\tSIZE\tPATH")
	for _, item := range reply.Items {
		action := "delete"
		if item.Deleted {
			action = "deleted"
		}
		ep := fmt.Sprintf("S%02dE%02d", item.Season, item.Episode)
		if item.EpTitle != "" {
			ep += " " + item.EpTitle
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", action, item.SeriesTitle, ep, formatSize(item.SizeBytes), item.FilePath)
	}
	w.Flush()

	if execute {
		fmt.Printf("\n%d files deleted, %s reclaimed\n", len(reply.Items), formatSize(reply.TotalBytes))
	} else {
		fmt.Printf("\n%d files, %s total (dry-run: use --execute to delete)\n", len(reply.Items), formatSize(reply.TotalBytes))
	}
	return nil
}

func runTVRefresh(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	fmt.Println("refreshing series from TMDB...")
	var reply daemon.RefreshSeriesReply
	if err := client.Call("Service.RefreshSeries", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	fmt.Printf("checked %d series, %d new episodes, %d marked ended\n",
		reply.Checked, reply.NewEpisodes, reply.Ended)
	return nil
}

func runTVMonitor(cmd *cobra.Command, args []string) error {
	season, _ := cmd.Flags().GetInt("season")
	off, _ := cmd.Flags().GetBool("off")
	latest, _ := cmd.Flags().GetBool("latest")
	all, _ := cmd.Flags().GetBool("all")
	none, _ := cmd.Flags().GetBool("none")

	mode := ""
	switch {
	case latest:
		mode = "latest"
	case all:
		mode = "all"
	case none:
		mode = "none"
	case season >= 0 && off:
		mode = "off"
	case season >= 0:
		mode = "on"
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.MonitorSeasonArgs{Season: season, Mode: mode}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}
	var reply daemon.MonitorSeasonReply
	if err := client.Call("Service.MonitorSeason", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("%s (%d)\n\n", reply.Title, reply.Year)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SEASON\tMONITORED\tWANTED\tCOMPLETED\tTOTAL")
	for _, s := range reply.Seasons {
		label := "--"
		if s.Monitored == s.Total {
			label = "[x]"
		} else if s.Monitored > 0 {
			label = fmt.Sprintf("[%d/%d]", s.Monitored, s.Total)
		}
		fmt.Fprintf(w, "S%02d\t%s\t%d\t%d\t%d\n", s.Season, label, s.Wanted, s.Completed, s.Total)
	}
	w.Flush()

	if reply.Affected > 0 {
		fmt.Printf("\n%d episodes updated\n", reply.Affected)
	}
	return nil
}

func runQueueRetry(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.RetryDownloadArgs{}
	if len(args) > 0 {
		// Parse "movie:<tmdb-id>" or "episode:<series-tmdb>:S01E02" format.
		parts := strings.SplitN(args[0], ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid format %q — use movie:TMDB_ID or episode:TMDB_ID:S01E02", args[0])
		}
		category := parts[0]
		if category != "movie" && category != "episode" {
			return fmt.Errorf("invalid category %q — use movie or episode", category)
		}
		rpcArgs.Category = category
		if category == "movie" {
			tmdbID, err := strconv.Atoi(parts[1])
			if err != nil {
				return fmt.Errorf("invalid TMDB ID %q: %w", parts[1], err)
			}
			rpcArgs.TmdbID = tmdbID
		} else {
			// Parse "TMDB_ID:S01E02" for episodes.
			epParts := strings.SplitN(parts[1], ":", 2)
			if len(epParts) != 2 {
				return fmt.Errorf("invalid episode format %q — use episode:TMDB_ID:S01E02", args[0])
			}
			tmdbID, err := strconv.Atoi(epParts[0])
			if err != nil {
				return fmt.Errorf("invalid series TMDB ID %q: %w", epParts[0], err)
			}
			var season, episode int
			if _, err := fmt.Sscanf(strings.ToUpper(epParts[1]), "S%dE%d", &season, &episode); err != nil {
				return fmt.Errorf("invalid episode identifier %q — expected S01E02 format", epParts[1])
			}
			rpcArgs.TmdbID = tmdbID
			rpcArgs.Season = season
			rpcArgs.Episode = episode
		}
	}

	var reply daemon.RetryDownloadReply
	if err := client.Call("Service.RetryDownload", rpcArgs, &reply); err != nil {
		return err
	}
	if reply.Count == 0 {
		fmt.Println("no failed downloads to retry")
	} else {
		fmt.Printf("retried %d download(s)\n", reply.Count)
	}
	return nil
}

func runQueueEvict(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.EvictQueueArgs{}
	parts := strings.SplitN(args[0], ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format %q — use movie:TMDB_ID or episode:TMDB_ID:S01E02", args[0])
	}
	category := parts[0]
	if category != "movie" && category != "episode" {
		return fmt.Errorf("invalid category %q — use movie or episode", category)
	}
	rpcArgs.Category = category
	if category == "movie" {
		tmdbID, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("invalid TMDB ID %q: %w", parts[1], err)
		}
		rpcArgs.TmdbID = tmdbID
	} else {
		epParts := strings.SplitN(parts[1], ":", 2)
		if len(epParts) != 2 {
			return fmt.Errorf("invalid episode format %q — use episode:TMDB_ID:S01E02", args[0])
		}
		tmdbID, err := strconv.Atoi(epParts[0])
		if err != nil {
			return fmt.Errorf("invalid series TMDB ID %q: %w", epParts[0], err)
		}
		var season, episode int
		if _, err := fmt.Sscanf(strings.ToUpper(epParts[1]), "S%dE%d", &season, &episode); err != nil {
			return fmt.Errorf("invalid episode identifier %q — expected S01E02 format", epParts[1])
		}
		rpcArgs.TmdbID = tmdbID
		rpcArgs.Season = season
		rpcArgs.Episode = episode
	}

	var reply daemon.EvictQueueReply
	if err := client.Call("Service.EvictQueue", rpcArgs, &reply); err != nil {
		return err
	}
	fmt.Printf("evicted %q from queue\n", reply.Title)
	return nil
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.ConfigShowReply
	if err := client.Call("Service.ConfigShow", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Quality Profile\t%s\n", reply.ProfileName)
	fmt.Fprintf(w, "Min Quality\t%s\n", reply.MinQuality)
	fmt.Fprintf(w, "Preferred\t%s\n", reply.Preferred)
	fmt.Fprintf(w, "Upgrade Until\t%s\n", reply.UpgradeUntil)
	if len(reply.MustNotContain) > 0 {
		fmt.Fprintf(w, "Must Not Contain\t%s\n", strings.Join(reply.MustNotContain, ", "))
	}
	if len(reply.PreferredWords) > 0 {
		fmt.Fprintf(w, "Preferred Words\t%s\n", strings.Join(reply.PreferredWords, ", "))
	}
	if reply.RetentionDays > 0 {
		fmt.Fprintf(w, "Retention\t%d days\n", reply.RetentionDays)
	}
	fmt.Fprintf(w, "Indexers\t%s\n", strings.Join(reply.Indexers, ", "))
	for _, p := range reply.Providers {
		fmt.Fprintf(w, "Provider\t%s\n", p)
	}
	fmt.Fprintf(w, "Library TV\t%s\n", reply.LibraryTV)
	fmt.Fprintf(w, "Library Movies\t%s\n", reply.LibraryMovies)
	fmt.Fprintf(w, "Incomplete\t%s\n", reply.IncompletePath)
	fmt.Fprintf(w, "Plex\t%v\n", reply.PlexEnabled)
	fmt.Fprintf(w, "Seerr\t%v\n", reply.SeerrEnabled)
	if reply.WebPort > 0 {
		fmt.Fprintf(w, "Web Port\t%d\n", reply.WebPort)
	}
	return w.Flush()
}

func runPlexServers(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.PlexServersReply
	if err := client.Call("Service.PlexServers", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	if !reply.Enabled {
		fmt.Println("plex integration not configured (set plex.token in config or PLEX_TOKEN env var)")
		return nil
	}

	if len(reply.Servers) == 0 {
		fmt.Println("no shared servers found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tURI")
	for _, s := range reply.Servers {
		fmt.Fprintf(w, "%s\t%s\n", s.Name, s.URI)
	}
	return w.Flush()
}

func runPlexCheck(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("TMDB ID must be a number (use 'udl movie search' or 'udl tv search' to find it)")
	}

	season, _ := cmd.Flags().GetInt("season")
	episode, _ := cmd.Flags().GetInt("episode")

	rpcArgs := &daemon.PlexCheckArgs{TmdbID: tmdbID, Season: season, Episode: episode}
	var reply daemon.PlexCheckReply
	if err := client.Call("Service.PlexCheck", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Matches) == 0 {
		fmt.Printf("%s %q (%d) not found on any friend's server (tmdb=%d)\n", reply.MediaType, reply.Title, reply.Year, tmdbID)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if reply.MediaType == "tv" {
		sort.Slice(reply.Matches, func(i, j int) bool {
			a, b := reply.Matches[i], reply.Matches[j]
			if a.ServerName != b.ServerName {
				return a.ServerName < b.ServerName
			}
			if a.Season != b.Season {
				return a.Season < b.Season
			}
			return a.Episode < b.Episode
		})
		fmt.Fprintln(w, "SERVER\tSERIES\tS/E\tRESOLUTION\tQUALITY")
		for _, m := range reply.Matches {
			fmt.Fprintf(w, "%s\t%s\tS%02dE%02d\t%s\t%s\n", m.ServerName, m.Title, m.Season, m.Episode, m.Resolution, m.Quality)
		}
	} else {
		fmt.Fprintln(w, "SERVER\tTITLE\tYEAR\tRESOLUTION\tQUALITY")
		for _, m := range reply.Matches {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", m.ServerName, m.Title, m.Year, m.Resolution, m.Quality)
		}
	}
	return w.Flush()
}

func runPlexCleanup(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	days, _ := cmd.Flags().GetInt("days")
	verbose, _ := cmd.Flags().GetBool("verbose")

	rpcArgs := &daemon.PlexCleanupArgs{Days: days}
	var reply daemon.PlexCleanupReply
	if err := client.Call("Service.PlexCleanup", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Items) == 0 {
		fmt.Println("no downloaded media found in Plex library")
		return nil
	}

	// Sort by (Title, Season) for stable ordering.
	sort.Slice(reply.Items, func(i, j int) bool {
		a, b := reply.Items[i], reply.Items[j]
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return a.Season < b.Season
	})

	// Show items in a table.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tTYPE\tTITLE\tQUALITY\tAGE\tSIZE\tWATCH COUNT\tLAST WATCHED\tWATCHED BY\tHINTS")
	for _, item := range reply.Items {
		if item.Action == "keep" && !verbose {
			continue
		}
		action := item.Action
		if item.Action == "keep" {
			action = fmt.Sprintf("keep (%s)", item.Reason)
		}
		age := "-"
		if item.AddedDays > 0 {
			age = fmt.Sprintf("%dd", item.AddedDays)
		}
		size := formatSize(item.SizeBytes)
		title := item.Title
		if item.Year > 0 {
			title = fmt.Sprintf("%s (%d)", item.Title, item.Year)
		}
		if item.MediaType == "episode" {
			title = fmt.Sprintf("%s S%02dE%02d", title, item.Season, item.Episode)
		}

		watchCount := "-"
		if item.WatchCount > 0 {
			watchCount = strconv.Itoa(item.WatchCount)
		}

		lastWatched := "-"
		if item.LastWatchedAt > 0 {
			daysAgo := int(time.Since(time.Unix(item.LastWatchedAt, 0)).Hours() / 24)
			if daysAgo == 0 {
				lastWatched = "today"
			} else {
				lastWatched = fmt.Sprintf("%dd ago", daysAgo)
			}
		}

		watchedBy := "-"
		if len(item.WatchedBy) > 0 {
			sort.Strings(item.WatchedBy)
			watchedBy = strings.Join(item.WatchedBy, ", ")
		}

		hints := "-"
		if len(item.Hints) > 0 {
			hints = strings.Join(item.Hints, ", ")
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			action, item.MediaType, title, item.Quality, age, size, watchCount, lastWatched, watchedBy, hints)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println()

	// Summary.
	fmt.Printf("%d delete candidates (%s reclaimable), %d kept — read-only report for AI handoff\n",
		reply.TotalDelete, formatSize(reply.TotalSize), reply.TotalKeep)
	return nil
}

type plexSectionJSON struct {
	Title     string         `json:"title"`
	Key       string         `json:"key"`
	Type      string         `json:"type"`
	Download  bool           `json:"download"`
	Fetch     bool           `json:"fetch,omitempty"`
	Items     *int           `json:"items,omitempty"`
	Scanned   int            `json:"scanned,omitempty"`
	Languages map[string]int `json:"languages,omitempty"`
}

type plexLibrariesJSON struct {
	Server    string            `json:"server"`
	Owned     bool              `json:"owned"`
	Reachable bool              `json:"reachable"`
	Error     string            `json:"error,omitempty"`
	Sections  []plexSectionJSON `json:"sections,omitempty"`
}

func runPlexLibraries(cmd *cobra.Command, args []string) error {
	token := plexToken()
	if token == "" {
		return fmt.Errorf("no Plex token configured (set plex.token or PLEX_TOKEN)")
	}
	audio, _ := cmd.Flags().GetBool("audio")
	shows, _ := cmd.Flags().GetBool("shows")
	counts, _ := cmd.Flags().GetBool("counts")
	probe, _ := cmd.Flags().GetBool("probe")
	items, _ := cmd.Flags().GetInt("items")
	perShow, _ := cmd.Flags().GetInt("per-show")
	rate, _ := cmd.Flags().GetFloat64("rate")
	serverFilter, _ := cmd.Flags().GetString("server")
	jsonOut, _ := cmd.Flags().GetBool("json")

	client := plex.New(token)
	client.SetRateLimit(rate)
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
	defer cancel()

	var servers []plex.Server
	owned, err := client.DiscoverOwnedServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: owned server: %v\n", err)
	} else {
		servers = append(servers, *owned)
	}
	friends, err := client.DiscoverServers()
	if err != nil {
		return fmt.Errorf("discover friends' servers: %w", err)
	}
	servers = append(servers, friends...)

	var entries []*plexLibrariesJSON
	for _, srv := range servers {
		if serverFilter != "" && !strings.Contains(strings.ToLower(srv.Name), strings.ToLower(serverFilter)) {
			continue
		}
		entry := &plexLibrariesJSON{Server: srv.Name, Owned: srv.Owned, Reachable: true, Sections: []plexSectionJSON{}}
		entries = append(entries, entry)

		secs, err := client.LibrarySectionsAll(srv)
		if err != nil {
			entry.Reachable = false
			entry.Error = err.Error()
			continue
		}
		for _, sec := range secs {
			row := plexSectionJSON{Title: sec.Title, Key: sec.Key, Type: sec.Type, Download: sec.Download}
			if counts {
				if n, err := client.SectionTotalSize(srv, sec.Key); err == nil {
					row.Items = &n
				}
			}
			if probe {
				ok, err := client.ProbeDownload(ctx, srv, sec.Key)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: probe %s/%s: %v\n", srv.Name, sec.Title, err)
				}
				row.Fetch = ok
			}
			if audio && (sec.Type == "movie" || (sec.Type == "show" && shows)) {
				st, err := client.SectionLanguageStats(ctx, srv, sec, plex.ScanOptions{
					MaxItems:     items,
					MaxPerShow:   perShow,
					IncludeShows: shows,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: audio scan %s/%s: %v\n", srv.Name, sec.Title, err)
				} else {
					row.Languages = st.ByLanguage
					row.Scanned = st.Scanned
				}
			}
			entry.Sections = append(entry.Sections, row)
		}
	}

	if jsonOut {
		data, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := "SERVER\tOWNED\tSECTION\tTYPE\tDOWNLOAD\tITEMS\tLANG"
	if probe {
		header = "SERVER\tOWNED\tSECTION\tTYPE\tDOWNLOAD\tFETCH\tITEMS\tLANG"
	}
	fmt.Fprintln(w, header)
	for _, e := range entries {
		if !e.Reachable {
			if probe {
				fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\t-\tunreachable: %s\n", e.Server, e.Error)
			} else {
				fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\tunreachable: %s\n", e.Server, e.Error)
			}
			continue
		}
		for _, s := range e.Sections {
			ownedCol := "no"
			if e.Owned {
				ownedCol = "yes"
			}
			dlCol := "no"
			if s.Download {
				dlCol = "yes"
			}
			itemsCol := "-"
			if s.Items != nil {
				itemsCol = strconv.Itoa(*s.Items)
			} else if s.Scanned > 0 {
				itemsCol = fmt.Sprintf("scan:%d", s.Scanned)
			}
			langCol := "-"
			if len(s.Languages) > 0 {
				keys := make([]string, 0, len(s.Languages))
				for k := range s.Languages {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, fmt.Sprintf("%s:%d", k, s.Languages[k]))
				}
				langCol = strings.Join(parts, " ")
			}
			if probe {
				fetchCol := "no"
				if s.Fetch {
					fetchCol = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					e.Server, ownedCol, s.Title, s.Type, dlCol, fetchCol, itemsCol, langCol)
			} else {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					e.Server, ownedCol, s.Title, s.Type, dlCol, itemsCol, langCol)
			}
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return nil
}

func runShadowCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	mediaType, _ := cmd.Flags().GetString("type")
	mount, _ := cmd.Flags().GetString("mount")
	if mediaType != "movie" && mediaType != "show" {
		return fmt.Errorf("--type must be 'movie' or 'show'")
	}
	if mount == "" {
		return fmt.Errorf("--mount is required (the local path your Plex library points at)")
	}
	prefer, _ := cmd.Flags().GetStringSlice("prefer")

	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	if shadow.Get(defs, name) != nil {
		return fmt.Errorf("shadow %q already exists", name)
	}
	defs = append(defs, shadow.Def{Name: name, Type: mediaType, Mount: mount, Prefer: prefer})
	if err := shadow.Save(defs); err != nil {
		return err
	}
	fmt.Printf("created shadow %q (%s) at %s — add libraries with 'udl shadow add %s <server> <section>'\n",
		name, mediaType, mount, name)
	return nil
}

func runShadowAdd(cmd *cobra.Command, args []string) error {
	name, server, section := args[0], args[1], args[2]

	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	def := shadow.Get(defs, name)
	if def == nil {
		return fmt.Errorf("no shadow named %q (see 'udl shadow list')", name)
	}
	for _, s := range def.Sources {
		if s.Server == server && strings.EqualFold(s.Section, section) {
			return fmt.Errorf("%s:%s already added to %q", server, section, name)
		}
	}
	def.Sources = append(def.Sources, shadow.Source{Server: server, Section: section})
	if err := shadow.Save(defs); err != nil {
		return err
	}
	fmt.Printf("added %s:%s to shadow %q (%d sources)\n", server, section, name, len(def.Sources))
	return nil
}

func runShadowList(cmd *cobra.Command, args []string) error {
	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		fmt.Println("no shadow libraries configured (use 'udl shadow create')")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tMOUNT\tSOURCES")
	for _, d := range defs {
		srcs := make([]string, 0, len(d.Sources))
		for _, s := range d.Sources {
			srcs = append(srcs, s.Server+":"+s.Section)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Name, d.Type, d.Mount, strings.Join(srcs, ", "))
	}
	return w.Flush()
}

func runShadowSources(cmd *cobra.Command, args []string) error {
	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	def := shadow.Get(defs, args[0])
	if def == nil {
		return fmt.Errorf("no shadow named %q (see 'udl shadow list')", args[0])
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	token := cfg.Plex.Token
	if token == "" {
		token = os.Getenv("PLEX_TOKEN")
	}
	if token == "" {
		return fmt.Errorf("plex token not configured (set plex.token in config or PLEX_TOKEN env var)")
	}

	avail, err := shadow.AvailableSections(context.Background(), plex.New(token), def)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tREACHABLE\tSECTIONS (this shadow type: "+def.Type+")")
	for _, a := range avail {
		status := "yes"
		if !a.Reachable {
			status = "no (" + a.Error + ")"
		}
		var parts []string
		for _, sec := range a.Sections {
			mark := ""
			for _, added := range a.Added {
				if strings.EqualFold(sec, added) {
					mark = " [added]"
					break
				}
			}
			parts = append(parts, sec+mark)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", a.Server, status, strings.Join(parts, ", "))
	}
	return w.Flush()
}

func runShadowManifest(cmd *cobra.Command, args []string) error {
	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	def := shadow.Get(defs, args[0])
	if def == nil {
		return fmt.Errorf("no shadow named %q (see 'udl shadow list')", args[0])
	}
	if len(def.Sources) == 0 {
		return fmt.Errorf("shadow %q has no sources — add some with 'udl shadow add %s <server> <section>'", def.Name, def.Name)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	token := cfg.Plex.Token
	if token == "" {
		token = os.Getenv("PLEX_TOKEN")
	}
	if token == "" {
		return fmt.Errorf("plex token not configured (set plex.token in config or PLEX_TOKEN env var)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	m, err := shadow.Build(ctx, plex.New(token), def)
	if err != nil {
		return err
	}

	path, err := shadow.ManifestPath(def.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("shadow %s (%s) mount=%s\n", m.Name, m.Type, m.Mount)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tSECTION\tSTATUS\tITEMS")
	for _, s := range m.Sources {
		status := "ok"
		if !s.OK {
			status = "ERR: " + s.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", s.Server, s.Section, status, s.Items)
	}
	w.Flush()
	fmt.Printf("candidates %d, kept %d, dupes dropped %d, failed %d, total %s\n",
		m.Stats.Candidates, m.Stats.Kept, m.Stats.Dupes, m.Stats.Failed, formatSize(m.Stats.TotalSize))
	fmt.Printf("manifest: %s\n", path)
	return nil
}

func runShadowMount(cmd *cobra.Command, args []string) error {
	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	def := shadow.Get(defs, args[0])
	if def == nil {
		return fmt.Errorf("no shadow named %q (see 'udl shadow list')", args[0])
	}
	m, err := shadow.LoadManifest(def.Name)
	if err != nil {
		return fmt.Errorf("manifest for %q not built — run 'udl shadow manifest %s' first: %w", def.Name, def.Name, err)
	}
	if len(m.Items) == 0 {
		return fmt.Errorf("manifest for %q is empty — rebuild it with 'udl shadow manifest %s'", def.Name, def.Name)
	}

	var totalSize int64
	for _, it := range m.Items {
		totalSize += it.Size
	}
	fmt.Printf("shadow %s (%s): %d items, %s\n", def.Name, m.Type, len(m.Items), formatSize(totalSize))

	daemon, _ := cmd.Flags().GetBool("daemon")
	// Resolve the real mountpoint (following symlinks like media/dubbed-tv).
	// A mount whose server died (Ctrl-C'd foreground process, crash) becomes
	// an orphan: every stat/readdir hangs on RPC timeouts. Only the mount
	// table is consulted — never the path itself, which would hang.
	real, mounted, err := resolveMount(def.Mount)
	if err != nil {
		return fmt.Errorf("check mount table: %w", err)
	}
	// The daemon never mounts (automount owns the mountpoint), so it serves
	// even when something is already mounted there.
	if mounted && !daemon {
		return fmt.Errorf("%s is already mounted — unmount it first ('udl shadow unmount %s' or 'sudo umount -f %s')", real, def.Name, real)
	}

	upper := real + ".upper"
	if def.Upper != "" {
		upper = def.Upper
	}

	// One-time move of existing local files into the upper layer so the mount
	// point can be taken over without losing the user's own files. The upper
	// dir is created only after the move decision so a fresh run never skips
	// it. In daemon mode none of this runs: the mountpoint is an autofs
	// trigger, and even stat'ing it makes automountd try to mount against a
	// server that is not up yet — a self-inflicted hang.
	moved := false
	if !daemon {
		if st, err := os.Stat(real); err == nil && st.IsDir() && !dirEmpty(real) {
			if _, uerr := os.Stat(upper); os.IsNotExist(uerr) || (uerr == nil && dirEmpty(upper)) {
				if uerr == nil {
					if err := os.Remove(upper); err != nil {
						return fmt.Errorf("remove stale %s: %w", upper, err)
					}
				}
				fmt.Printf("  moving local files: %s -> %s\n", real, upper)
				if err := os.Rename(real, upper); err != nil {
					return fmt.Errorf("move %s to %s: %w", real, upper, err)
				}
				moved = true
			} else {
				fmt.Printf("  note: %s still has files and %s exists — those files are hidden by the mount\n", real, upper)
			}
		}
		if err := os.MkdirAll(real, 0o755); err != nil {
			return err
		}
	}
	// The upper layer is a real directory in both modes: ensure it exists so
	// the union has a home for locally downloaded files. The mountpoint
	// itself is never touched in daemon mode (autofs/mount owns it).
	if err := os.MkdirAll(upper, 0o755); err != nil {
		return err
	}
	fmt.Printf("  upper layer: %s\n", upper)

	cacheDir, err := shadow.CacheDir(def.Name)
	if err != nil {
		return err
	}
	cacheGB, _ := cmd.Flags().GetInt("cache-size")
	cache := shadowfs.NewBlockCache(cacheDir, int64(cacheGB)<<30)
	fs := shadowfs.NewUnion(upper, m.Items, cache)
	fmt.Printf("  block cache: %s (%d GiB)\n", cacheDir, cacheGB)

	// Port resolution: --port flag wins, then the def's fixed port (set by
	// 'udl shadow enable' for automount), then auto-assigned.
	portFlag, _ := cmd.Flags().GetString("port")
	port := portFlag
	if port == "" && def.Port > 0 {
		port = strconv.Itoa(def.Port)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return fmt.Errorf("shadowfs: listen: %w", err)
	}
	tcpAddr := ln.Addr().(*net.TCPAddr)
	fmt.Printf("  NFS server: 127.0.0.1:%d\n", tcpAddr.Port)

	// The server must accept connections BEFORE mount_nfs runs — mount_nfs
	// blocks on its MNT RPC until the server answers, so starting it after
	// the mount would deadlock.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- shadowfs.Serve(ln, fs)
	}()

	if noTuning, _ := cmd.Flags().GetBool("no-tuning"); !noTuning {
		tuneShadowPlex(cmd.Context(), def)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 120*time.Second)
	defer cancel()
	if daemon {
		// The agent runs in the user's session, which macOS System Policy
		// approves for mounts — unlike launchd system daemons. Mount via
		// passwordless sudo (one-time /etc/sudoers.d rule) so the mount
		// comes up at login and survives restarts.
		//
		// macOS 26's System Policy intermittently STALLS mount_nfs for
		// launchd-spawned processes (deny(4) — a hang, not an error), so
		// each attempt is bounded and retried; a mount made by any approved
		// process survives agent restarts, and the fallback is mounting from
		// a terminal once.
		already, _ := isMounted(real)
		if already {
			fmt.Printf("  already mounted at %s\n", real)
		} else {
			opts := fmt.Sprintf("port=%d,mountport=%d,vers=3,nolocks,resvport", tcpAddr.Port, tcpAddr.Port)
			var out []byte
			var err error
			for range 3 {
				mctx, mcancel := context.WithTimeout(cmd.Context(), 20*time.Second)
				mnt := exec.CommandContext(mctx, "sudo", "-n", "mount", "-o", opts, "-t", "nfs", "127.0.0.1:/", real)
				out, err = mnt.CombinedOutput()
				mcancel()
				if err == nil {
					break
				}
				if mctx.Err() != nil {
					fmt.Printf("  mount attempt stalled (%s) — retrying\n", strings.TrimSpace(string(out)))
				}
			}
			if err != nil {
				fmt.Printf("  mount skipped: %s\n", strings.TrimSpace(string(out)))
				fmt.Println("  mount it once from a terminal to recover:")
				fmt.Printf("    sudo mount -o %s -t nfs 127.0.0.1:/ %s\n", opts, real)
			} else {
				fmt.Printf("  mounted at %s via sudo\n", real)
			}
		}
	} else {
		opts := fmt.Sprintf("port=%d,mountport=%d,vers=3,nolocks,resvport", tcpAddr.Port, tcpAddr.Port)
		fmt.Printf("  mounting: sudo mount -o %s -t nfs 127.0.0.1:/ %s\n", opts, real)
		fmt.Println("  (enter your password when prompted)")
		mnt := exec.CommandContext(ctx, "sudo", "mount", "-o", opts, "-t", "nfs", "127.0.0.1:/", real)
		mnt.Stdin, mnt.Stdout, mnt.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := mnt.Run(); err != nil {
			ln.Close()
			if moved {
				// macOS filesystem events (Spotlight) may briefly hold the
				// moved dirs, so retry; fall back to per-entry moves.
				fmt.Printf("restoring local files: %s -> %s\n", upper, real)
				var rerr error
				for range 10 {
					if rerr = os.Rename(upper, real); rerr == nil {
						break
					}
					time.Sleep(500 * time.Millisecond)
				}
				if rerr != nil {
					des, derr := os.ReadDir(upper)
					if derr == nil {
						restored := 0
						for _, e := range des {
							if err := os.Rename(filepath.Join(upper, e.Name()), filepath.Join(real, e.Name())); err == nil {
								restored++
							}
						}
						if restored == len(des) {
							_ = os.Remove(upper)
							rerr = nil
						} else {
							rerr = fmt.Errorf("moved %d/%d entries back", restored, len(des))
						}
					}
				}
				if rerr != nil {
					fmt.Printf("  warning: could not restore automatically (%v) — files remain at %s\n", rerr, upper)
				} else {
					fmt.Println("  local files restored")
				}
			}
			if ctx.Err() != nil {
				return fmt.Errorf("mount at %s timed out after 120s (sudo mount did not complete) — try again", real)
			}
			return fmt.Errorf("mount at %s failed (sudo): %w", real, err)
		}
		// Confirm the mount actually happened and the union is visible.
		des, rerr := os.ReadDir(real)
		if rerr != nil || len(des) == 0 {
			fmt.Printf("  warning: mount reported success but %s shows %v entries — check 'mount'\n", real, len(des))
		} else {
			fmt.Printf("  mounted: %d entries visible at %s\n", len(des), real)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Printf("\nunmounting %s...\n", real)
		if daemon {
			um := exec.Command("sudo", "-n", "umount", real)
			um.Stdin, um.Stdout, um.Stderr = os.Stdin, os.Stdout, os.Stderr
			_ = um.Run()
		} else {
			um := exec.Command("sudo", "umount", real)
			um.Stdin, um.Stdout, um.Stderr = os.Stdin, os.Stdout, os.Stderr
			_ = um.Run()
		}
		os.Exit(0)
	}()

	// Heartbeat so a long-serving mount never looks hung.
	go func() {
		for {
			time.Sleep(15 * time.Second)
			hits, misses, bytes := cache.Stats()
			fmt.Printf("  serving: %s streamed, %d cache hits, %d fetches\n", formatSize(bytes), hits, misses)
		}
	}()

	fmt.Println("serving — Ctrl-C to unmount")
	return <-serveErr
}

func runShadowUnmount(cmd *cobra.Command, args []string) error {
	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	def := shadow.Get(defs, args[0])
	if def == nil {
		return fmt.Errorf("no shadow named %q (see 'udl shadow list')", args[0])
	}
	real, _, err := resolveMount(def.Mount)
	if err != nil {
		return fmt.Errorf("check mount table: %w", err)
	}
	um := exec.Command("sudo", "umount", real)
	um.Stdin, um.Stdout, um.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := um.Run(); err != nil {
		return fmt.Errorf("unmount %s failed: %w", real, err)
	}
	fmt.Printf("unmounted %s\n", real)
	return nil
}

// shadowLabel returns the LaunchDaemon label for a shadow.
func shadowLabel(name string) string {
	return "com.jokull.udl-shadow-" + name
}

// shadowPlist renders the per-user LaunchAgent plist for a shadow. The agent
// runs in the user's session (approved by macOS System Policy for /Volumes
// access — system-domain daemons are denied file-read-data there) with the
// user's own HOME, so config/cache resolve naturally. KeepAlive restarts it
// unless it exits cleanly, so crashes self-heal.
func shadowPlist(label, exe, name, home string) string {
	logPath := filepath.Join(home, "Library", "Logs", label+".log")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>shadow</string>
		<string>mount</string>
		<string>%s</string>
		<string>--daemon</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, label, exe, name, logPath, logPath)
}

func runShadowEnable(cmd *cobra.Command, args []string) error {
	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	def := shadow.Get(defs, args[0])
	if def == nil {
		return fmt.Errorf("no shadow named %q (see 'udl shadow list')", args[0])
	}
	if _, err := shadow.LoadManifest(def.Name); err != nil {
		return fmt.Errorf("manifest for %q not built — run 'udl shadow manifest %s' first: %w", def.Name, def.Name, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	label := shadowLabel(def.Name)

	sudoRun := func(args ...string) error {
		ex := exec.Command("sudo", args...)
		ex.Stdin, ex.Stdout, ex.Stderr = os.Stdin, os.Stdout, os.Stderr
		return ex.Run()
	}
	// Clear any previous install first: an old system LaunchDaemon can linger
	// as a zombie holding the NFS port, and its socket is not visible or
	// killable from the user session until launchd reaps it.
	_ = sudoRun("launchctl", "bootout", "system/"+label)
	_ = sudoRun("rm", "-f", "/Library/LaunchDaemons/"+label+".plist")
	_ = runQuiet("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), label))

	// Fixed NFS port for the serve daemon: --port wins, then the def's, then
	// the first free port from 2055. Persisted so automount and the daemon
	// agree. The def's saved port is only reused if it is actually free now.
	portFlag, _ := cmd.Flags().GetString("port")
	port := 0
	if portFlag != "" {
		port, err = strconv.Atoi(portFlag)
		if err != nil {
			return fmt.Errorf("invalid --port %q: %w", portFlag, err)
		}
	} else if def.Port > 0 && portFree(def.Port) {
		port = def.Port
	}
	if port == 0 {
		port = pickFreePort(2055)
		if port == 0 {
			return fmt.Errorf("no free NFS port found in 2055-2154")
		}
	}
	if def.Port != port {
		def.Port = port
		if err := shadow.Save(defs); err != nil {
			return err
		}
		fmt.Printf("  fixed NFS port: %d (saved to shadow.toml)\n", port)
	}

	// The server is a per-user LaunchAgent: macOS System Policy denies
	// system-domain daemons file-read-data and file-mount on /Volumes/Plex,
	// but user-session processes are approved. The agent mounts itself via
	// passwordless sudo at login. (automountd is NOT used: its NFS client
	// cannot complete the mount handshake with a loopback server on this
	// macOS build.)
	if err := writeAutoMaps(nil, "", 0, sudoRun); err != nil {
		return err
	}
	if err := sudoRun("automount", "-vc"); err != nil {
		return fmt.Errorf("automount reload failed: %w", err)
	}
	fmt.Println("  automount map removed (agent mounts via sudo instead)")

	// Per-user LaunchAgent: serves the union in the user's session (approved
	// for /Volumes access), restarts on crash, runs at login.
	uid := os.Getuid()
	agentDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return err
	}
	plistPath := filepath.Join(agentDir, label+".plist")
	if err := os.WriteFile(plistPath, []byte(shadowPlist(label, exe, def.Name, home)), 0o644); err != nil {
		return err
	}
	// The bootout above leaves the previous instance shutting down for a
	// moment; bootstrapping the same label immediately can race it with
	// "Input/output error". Retry briefly.
	var bootErr error
	for range 5 {
		bootErr = runQuiet("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), plistPath)
		if bootErr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if bootErr != nil {
		return fmt.Errorf("bootstrap LaunchAgent failed: %w", bootErr)
	}
	for range 5 {
		if err := runQuiet("launchctl", "kickstart", fmt.Sprintf("gui/%d/%s", uid, label)); err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	fmt.Printf("enabled %s — serves at login on port %d, restarts on failure\n", def.Name, port)
	fmt.Printf("log: %s\n", filepath.Join(home, "Library", "Logs", label+".log"))
	fmt.Println("for the automatic mount at login, add a passwordless sudo rule once:")
	fmt.Println("  echo '$(whoami) ALL=(root) NOPASSWD: /sbin/mount, /sbin/umount' | sudo tee /etc/sudoers.d/udl-shadow")
	fmt.Printf("remove with: udl shadow disable %s\n", def.Name)
	return nil
}

func runShadowDisable(cmd *cobra.Command, args []string) error {
	defs, err := shadow.Load()
	if err != nil {
		return err
	}
	def := shadow.Get(defs, args[0])
	if def == nil {
		return fmt.Errorf("no shadow named %q (see 'udl shadow list')", args[0])
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	label := shadowLabel(def.Name)
	agentPlist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	sysPlist := filepath.Join("/Library/LaunchDaemons", label+".plist")

	sudoRun := func(args ...string) error {
		ex := exec.Command("sudo", args...)
		ex.Stdin, ex.Stdout, ex.Stderr = os.Stdin, os.Stdout, os.Stderr
		return ex.Run()
	}
	// bootout sends SIGTERM; the server exits 0 (graceful). Clean both the
	// per-user agent and any older system-daemon install.
	_ = runQuiet("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), label))
	_ = sudoRun("launchctl", "bootout", "system/"+label)
	_ = sudoRun("rm", "-f", sysPlist)
	// Remove the automount entry (rebuild the map from the remaining defs;
	// the disabled shadow stays in shadow.toml but its map entry must go).
	var remaining []shadow.Def
	for _, d := range defs {
		if d.Name == def.Name {
			continue
		}
		remaining = append(remaining, d)
	}
	if err := writeAutoMaps(remaining, "", 0, sudoRun); err != nil {
		return err
	}
	if err := sudoRun("automount", "-vc"); err != nil {
		return fmt.Errorf("automount reload failed: %w", err)
	}
	if err := os.Remove(agentPlist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s failed: %w", agentPlist, err)
	}
	fmt.Printf("disabled %s — serve agent and automount entry removed\n", def.Name)
	return nil
}

// runQuiet runs a command with output to the terminal but ignores failure
// (used for best-effort cleanup steps).
func runQuiet(name string, args ...string) error {
	ex := exec.Command(name, args...)
	ex.Stdin, ex.Stdout, ex.Stderr = os.Stdin, os.Stdout, os.Stderr
	return ex.Run()
}

// writeAutoMaps regenerates /etc/auto_udl from the defs (plus the given
// extra entry) and ensures /etc/auto_master references it. Called with the
// current defs; pass the shadow's own real path and port to add it, or empty
// to rebuild from defs only (removing it).
func writeAutoMaps(defs []shadow.Def, real string, port int, sudoRun func(...string) error) error {
	entries := []string{}
	seen := map[string]bool{}
	entry := func(r string, p int) string {
		// 127.0.0.1, not "localhost": mount_nfs resolves localhost to ::1
		// first, and the server binds IPv4 loopback only, so the mount never
		// connects. mountport must match port: mount_nfs resolves the MOUNT
		// protocol via portmap unless mountport is given, and there is no
		// portmap on loopback.
		return fmt.Sprintf("%s -fstype=nfs,nolocks,resvport,vers=3,port=%d,mountport=%d 127.0.0.1:/", r, p, p)
	}
	if real != "" {
		entries = append(entries, entry(real, port))
		seen[real] = true
	}
	for _, d := range defs {
		if d.Port == 0 {
			continue
		}
		r, _, err := resolveMount(d.Mount)
		if err != nil {
			continue
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		entries = append(entries, entry(r, d.Port))
	}
	sort.Strings(entries)

	tmp, err := os.CreateTemp("", "auto_udl-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	for _, e := range entries {
		if _, err := tmp.WriteString(e + "\n"); err != nil {
			tmp.Close()
			return err
		}
	}
	tmp.Close()
	if len(entries) == 0 {
		// No shadows left: drop the map and its auto_master reference.
		_ = sudoRun("rm", "-f", "/etc/auto_udl")
		return removeAutoMasterLine(sudoRun)
	}
	if err := sudoRun("install", "-m", "644", tmpName, "/etc/auto_udl"); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("install /etc/auto_udl failed: %w", err)
	}
	os.Remove(tmpName)
	return ensureAutoMasterLine(sudoRun)
}

// ensureAutoMasterLine appends the direct-map reference to /etc/auto_master
// if it is not already there.
func ensureAutoMasterLine(sudoRun func(...string) error) error {
	data, err := os.ReadFile("/etc/auto_master")
	if err != nil {
		return err
	}
	if strings.Contains(string(data), "/etc/auto_udl") {
		return nil
	}
	ex := exec.Command("sudo", "sh", "-c", `echo "/- /etc/auto_udl" >> /etc/auto_master`)
	ex.Stdin, ex.Stdout, ex.Stderr = os.Stdin, os.Stdout, os.Stderr
	return ex.Run()
}

// removeAutoMasterLine drops the auto_udl reference from /etc/auto_master.
func removeAutoMasterLine(sudoRun func(...string) error) error {
	data, err := os.ReadFile("/etc/auto_master")
	if err != nil {
		return err
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "/etc/auto_udl") {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n")
	if out == string(data) {
		return nil
	}
	tmp, err := os.CreateTemp("", "auto_master-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if err := sudoRun("install", "-m", "644", tmpName, "/etc/auto_master"); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("install /etc/auto_master failed: %w", err)
	}
	os.Remove(tmpName)
	return nil
}

// pickFreePort returns the first free TCP port starting at start.
func pickFreePort(start int) int {
	for p := start; p < start+100; p++ {
		if portFree(p) {
			return p
		}
	}
	return 0
}

// portFree reports whether a TCP port can be bound on loopback.
func portFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// tuneShadowPlex disables the Plex features that read whole media files on
// the owned server, scoped to the library sections this shadow is mounted at:
// preview thumbnails, intro/credit/ad markers, and voice-activity analysis.
// Those would otherwise stream entire shadow files from friends' servers.
func tuneShadowPlex(ctx context.Context, def *shadow.Def) {
	token := plexToken()
	if token == "" {
		fmt.Println("plex tuning skipped: no token configured")
		return
	}
	client := plex.New(token)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	srv, err := client.DiscoverOwnedServer()
	if err != nil {
		fmt.Printf("plex tuning skipped: %v\n", err)
		return
	}
	if err := client.SetPreferences(cctx, *srv, map[string]string{"GenerateBIFBehavior": "never"}); err != nil {
		fmt.Printf("plex tuning skipped: %v\n", err)
		return
	}
	fmt.Println("plex: preview thumbnails disabled (GenerateBIFBehavior=never)")

	real, _, err := resolveMount(def.Mount)
	if err != nil {
		fmt.Printf("plex tuning skipped: %v\n", err)
		return
	}
	sections, err := client.LibrarySections(*srv)
	if err != nil {
		fmt.Printf("plex tuning skipped: %v\n", err)
		return
	}
	markerPrefs := map[string]string{
		"enableIntroMarkerGeneration":   "0",
		"enableCreditsMarkerGeneration": "0",
		"enableAdMarkerGeneration":      "0",
		"enableBIFGeneration":           "0",
		"enableVoiceActivityGeneration": "0",
	}
	matched := 0
	for _, s := range sections {
		if s.Type != def.Type {
			continue
		}
		for _, loc := range s.Locations {
			locReal, _, lerr := resolveMount(loc)
			if lerr != nil || locReal != real {
				continue
			}
			if err := client.SetSectionPreferences(cctx, *srv, s.Key, markerPrefs); err != nil {
				fmt.Printf("plex tuning skipped: section %s: %v\n", s.Title, err)
				continue
			}
			fmt.Printf("plex: intro/credit/ad/BIF/voice-activity analysis disabled on %q\n", s.Title)
			matched++
		}
	}
	if matched == 0 {
		fmt.Println("plex tuning: no owned library section matched this shadow's mount path")
	}
}

// plexToken returns the configured Plex token, from config or PLEX_TOKEN.
func plexToken() string {
	cfg, err := config.Load()
	if err == nil && cfg.Plex.Token != "" {
		return cfg.Plex.Token
	}
	return os.Getenv("PLEX_TOKEN")
}

// dirEmpty reports whether a directory has no entries.
func dirEmpty(dir string) bool {
	des, err := os.ReadDir(dir)
	return err == nil && len(des) == 0
}

// isMounted reports whether path is currently a mountpoint, by scanning the
// mount table. Reading /sbin/mount never blocks, unlike touching the path
// itself when the mount's server is dead.
func isMounted(path string) (bool, error) {
	out, err := exec.Command("/sbin/mount").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, " on "+path) {
			return true, nil
		}
	}
	return false, nil
}

// resolveMount resolves the real path for a shadow mount point and reports
// whether anything is mounted there. It never stats or readlinks a path
// that could be a dead NFS mount: the directory prefix is resolved with
// readlink only, the mount table is checked before the final component is
// touched, and the final symlink (e.g. media/dubbed-tv -> ../dubbed-tv) is
// only read once its resolved parent is known not to be the mount.
func resolveMount(p string) (real string, mounted bool, err error) {
	clean := filepath.Clean(p)
	dir := resolveDir(filepath.Dir(clean))
	real = filepath.Join(dir, filepath.Base(clean))
	mounted, err = isMounted(real)
	if err != nil || mounted {
		return real, mounted, err
	}
	target, rerr := os.Readlink(real)
	if rerr != nil {
		return real, false, nil // not a symlink — nothing else to resolve
	}
	if filepath.IsAbs(target) {
		real = filepath.Clean(target)
	} else {
		real = filepath.Clean(filepath.Join(dir, target))
	}
	mounted, err = isMounted(real)
	return real, mounted, err
}

// resolveDir resolves symlinks in a directory path using only readlink on
// each component as it is reached. It never descends into a final mount:
// callers only pass parent directories of configured mount points.
func resolveDir(dir string) string {
	cur := filepath.Clean(dir)
	for range 10 {
		t, err := os.Readlink(cur)
		if err == nil {
			if filepath.IsAbs(t) {
				cur = filepath.Clean(t)
			} else {
				cur = filepath.Clean(filepath.Join(filepath.Dir(cur), t))
			}
			continue
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		t, err = os.Readlink(parent)
		if err == nil {
			if filepath.IsAbs(t) {
				cur = filepath.Clean(filepath.Join(t, filepath.Base(cur)))
			} else {
				cur = filepath.Clean(filepath.Join(filepath.Dir(parent), t, filepath.Base(cur)))
			}
			continue
		}
		return cur
	}
	return cur
}

// mediaTag formats a TMDB-based media identifier for CLI output.
// Movies: "movie:<tmdb_id>", Episodes: "episode:<series_tmdb_id>:S01E02".
// Falls back to "category:<db_id>" if TMDB ID is unavailable.
func mediaTag(category string, tmdbID int, season, episodeNum int, mediaID int64) string {
	if tmdbID != 0 {
		if category == "movie" {
			return fmt.Sprintf("movie:%d", tmdbID)
		}
		return fmt.Sprintf("episode:%d:S%02dE%02d", tmdbID, season, episodeNum)
	}
	return fmt.Sprintf("%s:%d", category, mediaID)
}

func formatSize(bytes int64) string {
	if bytes == 0 {
		return "-"
	}
	gb := float64(bytes) / (1024 * 1024 * 1024)
	if gb >= 1.0 {
		return fmt.Sprintf("%.1f GB", gb)
	}
	mb := float64(bytes) / (1024 * 1024)
	return fmt.Sprintf("%.0f MB", mb)
}

// printReleases prints a table of scored releases with enhanced columns.
func printReleases(results []daemon.ScoredRelease, existingQuality string) {
	if existingQuality != "" {
		fmt.Printf("Current quality: %s\n\n", existingQuality)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tTITLE\tQUALITY\tSOURCE\tSIZE\tAGE\tGROUP\tINDEXER\tSCORE\tSTATUS")
	for i, r := range results {
		size := fmt.Sprintf("%.1f GB", float64(r.Release.Size)/(1024*1024*1024))
		if r.Release.Size < 1024*1024*1024 {
			size = fmt.Sprintf("%.0f MB", float64(r.Release.Size)/(1024*1024))
		}
		age := "-"
		if r.Release.PubDate != "" {
			if t, err := time.Parse(time.RFC1123Z, r.Release.PubDate); err == nil {
				days := int(time.Since(t).Hours() / 24)
				age = fmt.Sprintf("%dd", days)
			} else if t, err := time.Parse(time.RFC1123, r.Release.PubDate); err == nil {
				days := int(time.Since(t).Hours() / 24)
				age = fmt.Sprintf("%dd", days)
			}
		}
		group := r.Parsed.Group
		if group == "" {
			group = "-"
		}
		source := r.Parsed.Source
		if source == "" {
			source = "-"
		}
		indexer := r.Indexer
		if indexer == "" {
			indexer = "-"
		}
		status := ""
		if r.Rejected {
			status = r.RejectionReason
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", i+1, r.Release.Title, r.Quality, source, size, age, group, indexer, r.Score, status)
		if i >= 19 {
			break
		}
	}
	w.Flush()
}

func runTVReleases(cmd *cobra.Command, args []string) error {
	season, _ := cmd.Flags().GetInt("season")
	episode, _ := cmd.Flags().GetInt("episode")

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.SearchEpisodeArgs{Season: season, Episode: episode}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.SearchEpisodeReply
	if err := client.Call("Service.SearchEpisode", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Results) == 0 {
		fmt.Println("no releases found")
		return nil
	}

	printReleases(reply.Results, reply.ExistingQuality)
	return nil
}

func runTVGrab(cmd *cobra.Command, args []string) error {
	season, _ := cmd.Flags().GetInt("season")
	episode, _ := cmd.Flags().GetInt("episode")
	index, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("second argument must be a release number (use 'udl tv releases' to find it)")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.GrabEpisodeReleaseArgs{Season: season, Episode: episode, Index: index}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.GrabEpisodeReleaseReply
	if err := client.Call("Service.GrabEpisodeRelease", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("grabbed: %s S%02dE%02d\n", reply.SeriesTitle, reply.Season, reply.Episode)
	fmt.Printf("  release: %s\n", reply.ReleaseName)
	fmt.Printf("  quality: %s\n", reply.Quality)
	return nil
}

func runTVEpisodes(cmd *cobra.Command, args []string) error {
	season, _ := cmd.Flags().GetInt("season")

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.SeriesEpisodesArgs{Season: season}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.SeriesEpisodesReply
	if err := client.Call("Service.SeriesEpisodes", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Episodes) == 0 {
		fmt.Println("no episodes")
		return nil
	}

	fmt.Printf("%s (%d)\n\n", reply.SeriesTitle, reply.Year)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "S/E\tTITLE\tAIR DATE\tMON\tSTATUS\tQUALITY\tFILE\tLAST SEARCHED")
	for _, ep := range reply.Episodes {
		se := fmt.Sprintf("S%02dE%02d", ep.Season, ep.Episode)
		title := ""
		if ep.Title.Valid {
			title = ep.Title.String
		}
		airDate := "-"
		if ep.AirDate.Valid && ep.AirDate.String != "" {
			airDate = ep.AirDate.String
		}
		mon := "--"
		if ep.Monitored {
			mon = "[x]"
		}
		q := ""
		if ep.Quality.Valid {
			q = ep.Quality.String
		}
		file := ""
		if ep.FilePath.Valid && ep.FilePath.String != "" {
			file = filepath.Base(ep.FilePath.String)
		} else if ep.NzbName.Valid && ep.NzbName.String != "" {
			file = ep.NzbName.String
			if len(file) > 50 {
				file = file[:50] + "..."
			}
		}
		lastSearched := "-"
		if ep.LastSearchedAt.Valid && ep.LastSearchedAt.String != "" {
			lastSearched = ep.LastSearchedAt.String
			if len(lastSearched) > 16 {
				lastSearched = lastSearched[:16]
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", se, title, airDate, mon, ep.Status, q, file, lastSearched)
	}
	return w.Flush()
}

func runMovieDelete(cmd *cobra.Command, args []string) error {
	execute, _ := cmd.Flags().GetBool("execute")
	search, _ := cmd.Flags().GetBool("search")

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.MovieDeleteArgs{Execute: execute, Search: search}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.MovieDeleteReply
	if err := client.Call("Service.MovieDelete", rpcArgs, &reply); err != nil {
		return err
	}

	if execute {
		fmt.Printf("deleted: %s (%d)\n", reply.Title, reply.Year)
		fmt.Printf("  file: %s (%s)\n", reply.FilePath, formatSize(reply.SizeBytes))
		if search {
			fmt.Println("  re-search triggered")
		}
	} else {
		fmt.Printf("would delete: %s (%d)\n", reply.Title, reply.Year)
		fmt.Printf("  file: %s (%s)\n", reply.FilePath, formatSize(reply.SizeBytes))
		fmt.Println("  (dry-run: use --execute to delete)")
	}
	return nil
}

func runWanted(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.WantedReply
	if err := client.Call("Service.Wanted", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	if len(reply.Items) == 0 {
		fmt.Println("nothing wanted")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tTMDB\tTITLE\tAIR DATE\tLAST SEARCHED\tSEARCHABLE")
	for _, item := range reply.Items {
		airDate := "-"
		if item.AirDate.Valid && item.AirDate.String != "" {
			airDate = item.AirDate.String
		}
		lastSearched := "-"
		if item.LastSearchedAt.Valid && item.LastSearchedAt.String != "" {
			lastSearched = item.LastSearchedAt.String
			if len(lastSearched) > 16 {
				lastSearched = lastSearched[:16]
			}
		}
		searchable := "yes"
		if !item.CanSearch {
			searchable = "NO (missing ID)"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n", item.Category, item.TmdbID, item.Title, airDate, lastSearched, searchable)
	}
	return w.Flush()
}

func runMovieReconcile(cmd *cobra.Command, args []string) error {
	dir, err := config.DataDir()
	if err != nil {
		return err
	}
	db, err := database.Open(filepath.Join(dir, "udl.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	movies, err := db.ListMovies()
	if err != nil {
		return err
	}
	m, err := shadow.LoadManifest("movies")
	if err != nil {
		return fmt.Errorf("movies shadow manifest not built — run 'udl shadow manifest movies' first: %w", err)
	}
	tmdbInShadow := map[string]bool{}
	imdbInShadow := map[string]bool{}
	for _, it := range m.Items {
		switch {
		case strings.HasPrefix(it.GUID, "tmdb://"):
			tmdbInShadow[strings.TrimPrefix(it.GUID, "tmdb://")] = true
		case strings.HasPrefix(it.GUID, "imdb://"):
			imdbInShadow[strings.TrimPrefix(it.GUID, "imdb://")] = true
		}
	}

	execute, _ := cmd.Flags().GetBool("execute")
	var toShadow, toWanted, alreadyWanted int
	var shown []string
	for _, mo := range movies {
		switch mo.Status {
		case "shadow":
			continue
		case "wanted":
			alreadyWanted++
			continue
		}
		if mo.Status != "downloaded" {
			continue
		}
		inShadow := tmdbInShadow[strconv.Itoa(mo.TmdbID)] ||
			(mo.ImdbID.Valid && imdbInShadow[mo.ImdbID.String])
		if inShadow {
			toShadow++
			if execute {
				if err := db.UpdateMovieStatus(mo.ID, "shadow", mo.Quality.String, ""); err != nil {
					return fmt.Errorf("%s: %w", mo.Title, err)
				}
			}
		} else {
			toWanted++
			if execute {
				if err := db.UpdateMovieStatus(mo.ID, "wanted", mo.Quality.String, ""); err != nil {
					return fmt.Errorf("%s: %w", mo.Title, err)
				}
			}
		}
		if len(shown) < 15 {
			state := "shadow"
			if !inShadow {
				state = "wanted"
			}
			shown = append(shown, fmt.Sprintf("  %s (%d) -> %s", mo.Title, mo.Year, state))
		}
	}

	fmt.Printf("movies: %d tracked (%d wanted, %d downloaded)\n", len(movies), alreadyWanted, toShadow+toWanted)
	fmt.Printf("shadow covers: %d downloaded -> shadow (available via mount)\n", toShadow)
	fmt.Printf("not covered:   %d downloaded -> wanted (re-download via usenet)\n", toWanted)
	if len(shown) > 0 {
		fmt.Println("sample:")
		for _, s := range shown {
			fmt.Println(s)
		}
	}
	if execute {
		fmt.Printf("applied: %d movies updated\n", toShadow+toWanted)
	} else {
		fmt.Println("dry-run — use --execute to apply")
	}
	return nil
}

func runMovieInfo(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.MovieInfoArgs{}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.MovieInfoReply
	if err := client.Call("Service.MovieInfo", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("tmdb:%d  %s (%d)\n", reply.TmdbID, reply.Title, reply.Year)
	fmt.Printf("status:      %s\n", reply.Status)
	if cov, covErr := shadow.Covered(reply.TmdbID, ""); covErr == nil && len(cov) > 0 {
		fmt.Printf("shadow:      available via %s\n", shadow.FormatCoverage(cov))
	}
	if reply.Quality != "" {
		fmt.Printf("quality:     %s\n", reply.Quality)
	}
	if reply.FilePath != "" {
		fmt.Printf("file:        %s\n", filepath.Base(reply.FilePath))
	}
	canSearch := "yes"
	if !reply.CanSearch {
		canSearch = "no (missing IMDB ID)"
	}
	imdb := reply.ImdbID
	if imdb == "" {
		imdb = "-"
	}
	fmt.Printf("imdb:        %s   can-search: %s\n", imdb, canSearch)
	if reply.AddedAt != "" {
		added := reply.AddedAt
		if len(added) > 10 {
			added = added[:10]
		}
		fmt.Printf("added:       %s\n", added)
	}

	// Download info if in queue.
	if reply.NzbName != "" {
		fmt.Printf("\ndownload:\n")
		fmt.Printf("  nzb:       %s\n", reply.NzbName)
		if reply.SizeBytes > 0 {
			fmt.Printf("  size:      %s\n", formatSize(reply.SizeBytes))
		}
		fmt.Printf("  progress:  %.0f%%\n", reply.Progress)
		if reply.Source != "" {
			fmt.Printf("  source:    %s\n", reply.Source)
		}
		if reply.Error != "" {
			errLabel := reply.Error
			if reply.Status == "post_processing" && isPhaseLabel(errLabel) {
				errLabel = "[phase] " + errLabel
			}
			fmt.Printf("  error:     %s\n", errLabel)
		}
	}

	// History.
	if len(reply.History) > 0 {
		fmt.Printf("\nhistory:\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "EVENT\tQUALITY\tSOURCE\tTIME")
		for _, h := range reply.History {
			q := ""
			if h.Quality.Valid {
				q = h.Quality.String
			}
			source := ""
			if h.Source.Valid {
				source = h.Source.String
			}
			createdAt := ""
			if h.CreatedAt.Valid {
				createdAt = h.CreatedAt.String
				if len(createdAt) > 16 {
					createdAt = createdAt[:16]
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", h.Event, q, source, createdAt)
		}
		w.Flush()
	}

	// Blocklist.
	if len(reply.Blocklist) > 0 {
		fmt.Printf("\nblocklist: %d entries\n", len(reply.Blocklist))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "RELEASE\tREASON\tTIME")
		for _, b := range reply.Blocklist {
			release := b.ReleaseTitle
			if len(release) > 50 {
				release = release[:50] + "..."
			}
			createdAt := ""
			if b.CreatedAt.Valid {
				createdAt = b.CreatedAt.String
				if len(createdAt) > 16 {
					createdAt = createdAt[:16]
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", release, b.Reason, createdAt)
		}
		w.Flush()
	}

	return nil
}

func runTVInfo(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.SeriesInfoArgs{}
	if tmdbID, err := strconv.Atoi(args[0]); err == nil {
		rpcArgs.TmdbID = tmdbID
	} else {
		rpcArgs.Title = args[0]
	}

	var reply daemon.SeriesInfoReply
	if err := client.Call("Service.SeriesInfo", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("tmdb:%d   %s (%d)\n", reply.TmdbID, reply.Title, reply.Year)
	canSearch := "yes"
	if !reply.CanSearch {
		canSearch = "no (missing TVDB ID)"
	}
	tvdb := "-"
	if reply.TvdbID != 0 {
		tvdb = strconv.Itoa(reply.TvdbID)
	}
	fmt.Printf("tvdb:%s   can-search: %s\n", tvdb, canSearch)
	fmt.Printf("status:      %s\n", reply.Status)
	if reply.AddedAt != "" {
		added := reply.AddedAt
		if len(added) > 10 {
			added = added[:10]
		}
		fmt.Printf("added:       %s\n", added)
	}

	fmt.Printf("\nepisodes: total=%d  wanted=%d  downloaded=%d", reply.EpisodeTotal, reply.EpisodeWanted, reply.EpisodeHave)
	if reply.EpisodeFailed > 0 {
		fmt.Printf("  failed=%d", reply.EpisodeFailed)
	}
	fmt.Println()

	// Season breakdown.
	if len(reply.Seasons) > 0 {
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SEASON\tMON\tTOTAL\tWANTED\tHAVE")
		for _, sm := range reply.Seasons {
			mon := "--"
			if sm.Monitored > 0 {
				mon = "[x]"
			}
			fmt.Fprintf(w, "S%02d\t%s\t%d\t%d\t%d\n", sm.Season, mon, sm.Total, sm.Wanted, sm.Completed)
		}
		w.Flush()
	}

	// Active downloads.
	if len(reply.ActiveDownloads) > 0 {
		status := "active"
		failCount := 0
		for _, d := range reply.ActiveDownloads {
			if d.Status == "failed" {
				failCount++
			}
		}
		if failCount == len(reply.ActiveDownloads) {
			status = fmt.Sprintf("%d failed", failCount)
		} else if failCount > 0 {
			status = fmt.Sprintf("%d active, %d failed", len(reply.ActiveDownloads)-failCount, failCount)
		}
		fmt.Printf("\nactive downloads: %s\n", status)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tPROGRESS\tERROR")
		for _, d := range reply.ActiveDownloads {
			id := mediaTag(d.Category, d.TmdbID, d.Season, d.EpisodeNum, d.MediaID)
			errMsg := ""
			if d.ErrorMsg.Valid && d.ErrorMsg.String != "" {
				errMsg = d.ErrorMsg.String
				if len(errMsg) > 50 {
					errMsg = errMsg[:50] + "..."
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%.0f%%\t%s\n", id, d.Title, d.Status, d.Progress, errMsg)
		}
		w.Flush()
	}

	// History.
	if len(reply.History) > 0 {
		fmt.Printf("\nhistory (last %d):\n", len(reply.History))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "EVENT\tSOURCE\tTIME")
		for _, h := range reply.History {
			source := ""
			if h.Source.Valid {
				source = h.Source.String
			}
			createdAt := ""
			if h.CreatedAt.Valid {
				createdAt = h.CreatedAt.String
				if len(createdAt) > 16 {
					createdAt = createdAt[:16]
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", h.Event, source, createdAt)
		}
		w.Flush()
	}

	return nil
}

func runSearchTrigger(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	tmdbID, _ := cmd.Flags().GetInt("tmdb")
	season, _ := cmd.Flags().GetInt("season")
	episode, _ := cmd.Flags().GetInt("episode")

	rpcArgs := &daemon.ForceSearchArgs{
		TmdbID:  tmdbID,
		Season:  season,
		Episode: episode,
	}
	if len(args) > 0 && tmdbID == 0 {
		if id, err := strconv.Atoi(args[0]); err == nil {
			rpcArgs.TmdbID = id
		} else {
			rpcArgs.Title = args[0]
		}
	}

	var reply daemon.ForceSearchReply
	if err := client.Call("Service.ForceSearch", rpcArgs, &reply); err != nil {
		return err
	}

	if reply.Count == 1 {
		fmt.Println("triggered search for 1 item")
	} else {
		fmt.Printf("triggered search for %d wanted items\n", reply.Count)
	}
	return nil
}

// isPhaseLabel returns true if the error string is a post-processing phase label.
func isPhaseLabel(s string) bool {
	switch s {
	case "par2 verify", "par2 repair", "rar extract", "importing", "cleanup":
		return true
	}
	return false
}

func runSchedule(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	days, _ := cmd.Flags().GetInt("days")
	rpcArgs := &daemon.ScheduleArgs{Days: days}
	var reply daemon.ScheduleReply
	if err := client.Call("Service.Schedule", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Episodes) == 0 {
		fmt.Println("no upcoming episodes")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERIES\tS/E\tTITLE\tAIR DATE\tMON\tSTATUS")
	for _, ep := range reply.Episodes {
		se := fmt.Sprintf("S%02dE%02d", ep.Season, ep.Episode)
		title := ""
		if ep.Title.Valid {
			title = ep.Title.String
		}
		airDate := "-"
		if ep.AirDate.Valid {
			airDate = ep.AirDate.String
		}
		mon := "--"
		if ep.Monitored {
			mon = "[x]"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", ep.SeriesTitle, se, title, airDate, mon, ep.Status)
	}
	return w.Flush()
}

func runLibraryImport(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	execute, _ := cmd.Flags().GetBool("execute")

	rpcArgs := &daemon.LibraryImportArgs{
		Dir:     args[0],
		Execute: execute,
	}
	var reply daemon.LibraryImportReply
	if err := client.Call("Service.LibraryImport", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("scanned %d media files\n\n", reply.Scanned)

	if len(reply.Actions) > 0 {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ACTION\tTYPE\tTITLE\tQUALITY\tPATH")
		for _, a := range reply.Actions {
			dest := a.DestPath
			if dest == "" {
				dest = a.Reason
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Action, a.MediaType, a.Title, a.Quality, dest)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Println()
	}

	if len(reply.Errors) > 0 {
		fmt.Printf("errors:\n")
		for _, e := range reply.Errors {
			fmt.Printf("  %s\n", e)
		}
		fmt.Println()
	}

	mode := "dry-run: use --execute to perform"
	if execute {
		mode = "executed"
	}
	importParts := []string{
		fmt.Sprintf("%d to import", reply.Imported),
	}
	if reply.Upgraded > 0 {
		importParts = append(importParts, fmt.Sprintf("%d upgrades", reply.Upgraded))
	}
	importParts = append(importParts,
		fmt.Sprintf("%d skipped", reply.Skipped),
		fmt.Sprintf("%d errors", len(reply.Errors)),
	)
	fmt.Printf("%s (%s)\n", strings.Join(importParts, ", "), mode)
	return nil
}

func runLibraryCleanup(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	execute, _ := cmd.Flags().GetBool("execute")
	rename, _ := cmd.Flags().GetBool("rename")
	del, _ := cmd.Flags().GetBool("delete")

	rpcArgs := &daemon.LibraryCleanupArgs{
		Rename:  rename,
		Delete:  del,
		Execute: execute,
	}
	var reply daemon.LibraryCleanupReply
	if err := client.Call("Service.LibraryCleanup", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Findings) > 0 {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "FINDING\tTYPE\tTITLE\tPATH")
		for _, f := range reply.Findings {
			path := f.FilePath
			if f.Finding == "misnamed" && f.ExpectedPath != "" {
				path = fmt.Sprintf("%s\n\t\t\t→ %s", f.FilePath, f.ExpectedPath)
				if f.Renamed {
					path += " (renamed)"
				}
			}
			if f.Deleted {
				path += " (deleted)"
			}
			if f.Finding == "missing" && execute {
				path += " (reset to wanted)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Finding, f.MediaType, f.Title, path)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Println()
	}

	ok := reply.Scanned - reply.Orphans - reply.Misnamed
	parts := []string{
		fmt.Sprintf("%d scanned", reply.Scanned),
		fmt.Sprintf("%d ok", ok),
		fmt.Sprintf("%d orphans", reply.Orphans),
		fmt.Sprintf("%d misnamed", reply.Misnamed),
		fmt.Sprintf("%d missing", reply.Missing),
	}
	if reply.EmptyDirsRemoved > 0 {
		parts = append(parts, fmt.Sprintf("%d empty dirs removed", reply.EmptyDirsRemoved))
	}
	fmt.Println(strings.Join(parts, ", "))
	return nil
}

func runLibraryPruneIncomplete(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	execute, _ := cmd.Flags().GetBool("execute")

	rpcArgs := &daemon.PruneIncompleteArgs{Execute: execute}
	var reply daemon.PruneIncompleteReply
	if err := client.Call("Service.LibraryPruneIncomplete", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Findings) == 0 {
		fmt.Println("no orphan incomplete directories found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REASON\tSIZE\tDIR")
	for _, f := range reply.Findings {
		size := fmt.Sprintf("%.1f MB", float64(f.Size)/(1024*1024))
		status := f.Dir
		if f.Pruned {
			status += " (removed)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.Reason, size, status)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println()

	totalMB := float64(reply.TotalSize) / (1024 * 1024)
	mode := "dry-run: use --execute to remove"
	if execute {
		mode = fmt.Sprintf("removed %d of %d dirs", reply.PrunedDirs, reply.TotalDirs)
	}
	fmt.Printf("%d orphan dirs (%.1f MB) — %s\n", reply.TotalDirs, totalMB, mode)

	// Directories whose name doesn't match a layout the daemon writes are
	// reported but never removed, so say so rather than leaving the user to
	// wonder why --execute skipped them.
	unknown := 0
	for _, f := range reply.Findings {
		if f.Reason == "unknown" {
			unknown++
		}
	}
	if unknown > 0 {
		fmt.Printf("%d dir(s) with an unrecognized name were left in place; remove them by hand if you are sure\n", unknown)
	}
	return nil
}

func runLibraryVerify(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	fix, _ := cmd.Flags().GetBool("fix")
	var reply daemon.LibraryVerifyReply
	if err := client.Call("Service.LibraryVerify", &daemon.LibraryVerifyArgs{Fix: fix}, &reply); err != nil {
		return err
	}

	if reply.Claimed > 0 {
		fmt.Printf("claimed %d orphan(s)\n", reply.Claimed)
	}

	if len(reply.Findings) == 0 && reply.Claimed == 0 {
		fmt.Println("library OK: no issues found")
		return nil
	}

	if len(reply.Findings) > 0 {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "FINDING\tTYPE\tTITLE\tPATH")
		for _, f := range reply.Findings {
			path := f.FilePath
			if f.Finding == "misnamed" && f.ExpectedPath != "" {
				path = fmt.Sprintf("%s → %s", f.FilePath, f.ExpectedPath)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Finding, f.MediaType, f.Title, path)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Println()
	}

	fmt.Printf("%d orphans, %d misnamed, %d missing\n",
		reply.Orphans, reply.Misnamed, reply.Missing)

	if reply.Orphans > 0 || reply.Misnamed > 0 || reply.Missing > 0 {
		os.Exit(1)
	}
	return nil
}

func runConfigCheck(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	fmt.Println("config OK")
	return nil
}

func runConfigPath(cmd *cobra.Command, args []string) error {
	p, err := config.Path()
	if err != nil {
		return err
	}
	fmt.Println(p)
	return nil
}

func runInit(cmd *cobra.Command, args []string) error {
	p, err := config.Path()
	if err != nil {
		return err
	}

	// Don't overwrite existing config.
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("config already exists at %s\nEdit it directly or run: udl config check", p)
	}

	// Create config directory.
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	if err := os.WriteFile(p, []byte(configTemplate), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Printf("Created %s\n\n", p)
	fmt.Println("Next steps:")
	fmt.Println("  1. Edit the config file and fill in your credentials")
	fmt.Println("  2. Run: udl config check")
	fmt.Println("  3. Run: udl daemon")
	return nil
}

const configTemplate = `# UDL configuration
# Docs: https://github.com/jokull/udl

# ── Library paths (where Plex/Jellyfin reads from) ──
[library]
tv = ""      # e.g. "/media/tv" or "/Users/you/Plex/media/tv"
movies = ""  # e.g. "/media/movies" or "/Users/you/Plex/media/movies"

# ── Working directories (temporary, can be on a different drive) ──
[paths]
incomplete = ""  # active downloads
complete = ""    # post-processed, before import to library

# ── Quality profile ──
# Presets: "720p", "1080p" (default), "4k", "remux"
# Or set min/preferred/upgrade_until individually.
[quality]
profile = "1080p"
# must_not_contain = ["CAM", "HDTS", "TELECINE"]
# preferred_words = ["FLUX", "NTb"]
# preferred_codecs = ["hevc", "h264"]   # bonus score for releases using these codecs
# blocked_codecs = ["av1"]              # hard-reject releases using these codecs

# ── TMDB (required) ──
# Get a free API key at https://www.themoviedb.org/settings/api
[tmdb]
apikey = ""

# ── Usenet providers ──
# At least one provider is required. Most providers offer plans at
# https://www.reddit.com/r/usenet/wiki/providers
[[usenet.providers]]
name = ""          # e.g. "newshosting"
host = ""          # e.g. "news.newshosting.com"
port = 563
tls = true
username = ""
password = ""
connections = 20   # check your plan's limit
# level = 0        # 0 = primary (default), 1+ = fill/backup

# ── Indexers (Newznab-compatible) ──
# At least one indexer is required. Popular options:
# DOGnzb, NZBgeek, Nzb.su, omgwtfnzbs
[[indexers]]
name = ""      # e.g. "DOGnzb"
url = ""       # e.g. "https://api.dognzb.cr"
apikey = ""

# ── Optional: Plex integration ──
# Check friends' servers before downloading. Get your token:
# https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/
# [plex]
# token = ""

# ── Optional: Web UI ──
# [web]
# port = 9876   # 0 = disabled (default)
# bind = "127.0.0.1"

# ── Optional: Usenet retention ──
# [usenet]
# retention_days = 4000  # reject articles older than this
`

// --- Migrate commands (no daemon required) ---

func openDBDirect() (*database.DB, error) {
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "udl.db")
	return database.Open(dbPath)
}

func runMigrateRadarr(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")
	apiKey, _ := cmd.Flags().GetString("apikey")
	execute, _ := cmd.Flags().GetBool("execute")

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	db, err := openDBDirect()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	mode := "dry-run"
	if execute {
		mode = "execute"
	}
	fmt.Printf("migrating from Radarr (%s) [%s]\n\n", url, mode)

	res, err := migrate.RunRadarr(db, url, apiKey, execute, log)
	if err != nil {
		return err
	}

	fmt.Printf("\nresults: %d added, %d skipped, %d with files, %d wanted\n",
		res.Added, res.Skipped, res.Files, res.Wanted)
	if len(res.Errors) > 0 {
		fmt.Printf("errors (%d):\n", len(res.Errors))
		for _, e := range res.Errors {
			fmt.Printf("  %s\n", e)
		}
	}
	if !execute && res.Added > 0 {
		fmt.Println("\nuse --execute to write to database")
	}
	return nil
}

func runMigrateSonarr(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")
	apiKey, _ := cmd.Flags().GetString("apikey")
	execute, _ := cmd.Flags().GetBool("execute")

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config (need TMDB API key): %w", err)
	}

	tmdbClient, err := tmdb.New(cfg.TMDB.APIKey)
	if err != nil {
		return fmt.Errorf("init TMDB client: %w", err)
	}

	db, err := openDBDirect()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	mode := "dry-run"
	if execute {
		mode = "execute"
	}
	fmt.Printf("migrating from Sonarr (%s) [%s]\n\n", url, mode)

	res, err := migrate.RunSonarr(db, tmdbClient, url, apiKey, execute, log)
	if err != nil {
		return err
	}

	fmt.Printf("\nresults: %d series added, %d skipped, %d episode files, %d episodes wanted\n",
		res.Added, res.Skipped, res.Files, res.Wanted)
	if len(res.Errors) > 0 {
		fmt.Printf("errors (%d):\n", len(res.Errors))
		for _, e := range res.Errors {
			fmt.Printf("  %s\n", e)
		}
	}
	if !execute && res.Added > 0 {
		fmt.Println("\nuse --execute to write to database")
	}
	return nil
}

// --- Raw NZB commands ---

var nzbCmd = &cobra.Command{
	Use:   "nzb",
	Short: "Raw Usenet NZB search and download",
	Long:  "Search indexers and download NZBs directly, bypassing the media library pipeline.",
}

var nzbSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search indexers for NZBs",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runNZBSearch,
}

var nzbGrabCmd = &cobra.Command{
	Use:   "grab [nzb-url]",
	Short: "Download an NZB to a local directory",
	Long:  "Fetches the NZB, downloads via NNTP, post-processes (PAR2/RAR), and puts files in the output directory.",
	Args:  cobra.ExactArgs(1),
	RunE:  runNZBGrab,
}

func runNZBSearch(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	cat, _ := cmd.Flags().GetString("cat")
	query := strings.Join(args, " ")

	rpcArgs := &daemon.NZBSearchArgs{
		Query:    query,
		Category: cat,
	}
	var reply daemon.NZBSearchReply
	if err := client.Call("Service.NZBSearch", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Results) == 0 {
		fmt.Println("no results found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "#\tTITLE\tSIZE\tINDEXER\n")
	for i, r := range reply.Results {
		size := formatSize(r.Size)
		title := r.Title
		if len(title) > 80 {
			title = title[:77] + "..."
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i+1, title, size, r.Indexer)
	}
	w.Flush()

	fmt.Printf("\nTo download, copy the NZB URL and run:\n")
	fmt.Printf("  udl nzb grab <nzb-url> -o /path/to/output/\n")

	// Print URLs for easy copy-paste.
	fmt.Println()
	for i, r := range reply.Results {
		fmt.Printf("  [%d] %s\n", i+1, r.Link)
	}

	return nil
}

func runNZBGrab(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	outputDir, _ := cmd.Flags().GetString("output")
	// Resolve relative paths.
	if !filepath.IsAbs(outputDir) {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		outputDir = filepath.Join(wd, outputDir)
	}

	nzbURL := args[0]

	fmt.Printf("downloading to %s ...\n", outputDir)

	rpcArgs := &daemon.NZBGrabArgs{
		NzbURL:    nzbURL,
		OutputDir: outputDir,
	}
	var reply daemon.NZBGrabReply
	if err := client.Call("Service.NZBGrab", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("done — %d file(s):\n", len(reply.Files))
	for _, f := range reply.Files {
		fmt.Printf("  %s\n", f)
	}
	return nil
}
