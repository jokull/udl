package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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
	"github.com/jokull/udl/internal/migrate"
	"github.com/jokull/udl/internal/tmdb"
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
	Short: "Check if a movie is available on friends' Plex servers",
	Long:  "Check by TMDB ID (from 'udl movie search' or 'udl movie list').",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlexCheck,
}

var plexCleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Delete unwatched media older than N days from library",
	Long:  "Queries Plex watch history on your owned server. Items never watched and added more than --days ago are candidates for deletion. Dry-run by default; use --execute to actually delete files.",
	RunE:  runPlexCleanup,
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
	Short: "Remove a specific blocklist entry",
	Args:  cobra.ExactArgs(1),
	RunE:  runBlocklistRemove,
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
	Long:  "Read-only check that reports all issues without making changes.",
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
	movieCmd.AddCommand(movieAddCmd, movieListCmd, movieSearchCmd, movieReleasesCmd, movieGrabCmd, movieRemoveCmd, movieDeleteCmd, movieInfoCmd)
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
	queueCmd.AddCommand(queuePauseCmd, queueResumeCmd, queueClearCmd, queueRetryCmd, queueEvictCmd)
	plexCleanupCmd.Flags().Int("days", 90, "Minimum days since added to consider for cleanup")
	plexCleanupCmd.Flags().Bool("execute", false, "Actually delete files (default is dry-run)")
	plexCleanupCmd.Flags().Bool("verbose", false, "Also show items that would be kept")
	plexCmd.AddCommand(plexServersCmd, plexCheckCmd, plexCleanupCmd)
	blocklistCmd.AddCommand(blocklistClearCmd, blocklistRemoveCmd)
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
	libraryCmd.AddCommand(libraryImportCmd, libraryCleanupCmd, libraryPruneIncompleteCmd, libraryVerifyCmd, libraryPruneCmd)

	rootCmd.AddCommand(daemonCmd, statusCmd, movieCmd, tvCmd, queueCmd, plexCmd, historyCmd, blocklistCmd, libraryCmd, migrateCmd, configCmd, wantedCmd, scheduleCmd, searchTriggerCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// --- Daemon ---

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

	if reply.FailedCount > 0 || reply.BlockedCount > 0 {
		fmt.Printf("failed (24h): %d   blocklisted: %d\n", reply.FailedCount, reply.BlockedCount)
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
		if reply.Grabbed {
			fmt.Println("  -> release found and enqueued for download")
		} else {
			fmt.Println("  -> no matching release found on indexers")
		}
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
	fmt.Fprintln(w, "TMDB ID\tTITLE\tYEAR\tSTATUS\tQUALITY\tADDED\tFILE")
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
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\t%s\n", m.TmdbID, m.Title, m.Year, m.Status, q, added, file)
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
	fmt.Fprintln(w, "TMDB ID\tTITLE\tYEAR\tSTATUS\tEPS\tWANTED\tHAVE")
	for _, s := range reply.Series {
		counts := reply.Counts[s.ID]
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%d\t%d\t%d\n", s.TmdbID, s.Title, s.Year, s.Status, counts[0], counts[1], counts[2])
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

	if len(reply.Entries) == 0 {
		fmt.Println("blocklist empty")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tMEDIA\tRELEASE\tREASON\tTIME")
	for _, e := range reply.Entries {
		reason := e.Reason
		if len(reason) > 60 {
			reason = reason[:60] + "..."
		}
		media := mediaTag(e.MediaType, e.TmdbID, e.Season, e.EpisodeNum, e.MediaID)
		createdAt := "—"
		if e.CreatedAt.Valid {
			createdAt = e.CreatedAt.String
			if len(createdAt) > 16 {
				createdAt = createdAt[:16]
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", e.ID, media, e.ReleaseTitle, reason, createdAt)
	}
	return w.Flush()
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

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid blocklist ID %q: %w", args[0], err)
	}

	rpcArgs := &daemon.BlocklistRemoveArgs{ID: id}
	if err := client.Call("Service.BlocklistRemove", rpcArgs, &daemon.Empty{}); err != nil {
		return err
	}
	fmt.Printf("removed blocklist entry %d\n", id)
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
		return fmt.Errorf("TMDB ID must be a number (use 'udl movie search' to find it)")
	}

	rpcArgs := &daemon.PlexCheckArgs{TmdbID: tmdbID}
	var reply daemon.PlexCheckReply
	if err := client.Call("Service.PlexCheck", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Matches) == 0 {
		fmt.Printf("not found on any friend's server (tmdb=%d)\n", tmdbID)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tTITLE\tYEAR\tRESOLUTION\tQUALITY")
	for _, m := range reply.Matches {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", m.ServerName, m.Title, m.Year, m.Resolution, m.Quality)
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
	execute, _ := cmd.Flags().GetBool("execute")
	verbose, _ := cmd.Flags().GetBool("verbose")

	rpcArgs := &daemon.PlexCleanupArgs{Days: days, Execute: execute}
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
	fmt.Fprintln(w, "ACTION\tTYPE\tTITLE\tQUALITY\tAGE\tSIZE\tLAST WATCHED\tWATCHED BY")
	for _, item := range reply.Items {
		if item.Action == "keep" && !verbose {
			continue
		}
		action := item.Action
		if item.Deleted {
			action = "deleted"
		}
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
		if item.MediaType == "season" {
			title = fmt.Sprintf("%s S%02d (%dep)", title, item.Season, item.EpisodeCount)
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

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			action, item.MediaType, title, item.Quality, age, size, lastWatched, watchedBy)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println()

	// Summary.
	if execute {
		fmt.Printf("deleted %d items (%s), kept %d\n",
			reply.DeletedCount, formatSize(reply.DeletedSize), reply.TotalKeep)
	} else {
		fmt.Printf("would delete %d items (%s), keep %d — use --execute to apply\n",
			reply.TotalDelete, formatSize(reply.TotalSize), reply.TotalKeep)
	}
	return nil
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
	return nil
}

func runLibraryVerify(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	var reply daemon.LibraryVerifyReply
	if err := client.Call("Service.LibraryVerify", &daemon.LibraryVerifyArgs{}, &reply); err != nil {
		return err
	}

	if len(reply.Findings) == 0 {
		fmt.Println("library OK: no issues found")
		return nil
	}

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

