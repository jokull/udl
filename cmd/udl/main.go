package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/daemon"
	"github.com/jokull/udl/internal/database"
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
	Use:   "releases [tmdb-id]",
	Short: "Search indexers for a movie in the database",
	Long:  "Searches Usenet indexers for releases matching a movie already in the database.\nUse the movie's TMDB ID (from 'udl movie search' or 'udl movie list').",
	Args:  cobra.ExactArgs(1),
	RunE:  runMovieReleases,
}

var movieGrabCmd = &cobra.Command{
	Use:   "grab [tmdb-id] [#]",
	Short: "Grab a specific indexer release for a movie",
	Long:  "Searches indexers for a movie in the database and grabs the release at the given index.\nRun 'udl movie releases' first to see numbered results, then grab by number.",
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
	RunE:  runQueueRetry,
}

var movieRemoveCmd = &cobra.Command{
	Use:   "remove [tmdb-id]",
	Short: "Remove a movie from monitoring (not from disk)",
	Long:  "Remove a movie by its TMDB ID (from 'udl movie list').",
	Args:  cobra.ExactArgs(1),
	RunE:  runMovieRemove,
}

var tvRemoveCmd = &cobra.Command{
	Use:   "remove [tmdb-id]",
	Short: "Remove a series from monitoring (not from disk)",
	Long:  "Remove a series by its TMDB ID (from 'udl tv list').",
	Args:  cobra.ExactArgs(1),
	RunE:  runTVRemove,
}

var tvRefreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Refresh episode metadata from TMDB for all monitored series",
	RunE:  runTVRefresh,
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

func init() {
	movieCmd.AddCommand(movieAddCmd, movieListCmd, movieSearchCmd, movieReleasesCmd, movieGrabCmd, movieRemoveCmd)
	tvCmd.AddCommand(tvAddCmd, tvSearchCmd, tvListCmd, tvRemoveCmd, tvRefreshCmd)
	queueCmd.AddCommand(queuePauseCmd, queueResumeCmd, queueClearCmd, queueRetryCmd)
	plexCleanupCmd.Flags().Int("days", 90, "Minimum days since added to consider for cleanup")
	plexCleanupCmd.Flags().Bool("execute", false, "Actually delete files (default is dry-run)")
	plexCleanupCmd.Flags().Bool("verbose", false, "Also show items that would be kept")
	plexCmd.AddCommand(plexServersCmd, plexCheckCmd, plexCleanupCmd)
	blocklistCmd.AddCommand(blocklistClearCmd, blocklistRemoveCmd)
	configCmd.AddCommand(configCheckCmd, configPathCmd)

	libraryImportCmd.Flags().Bool("execute", false, "Actually import files (default is dry-run)")
	libraryCleanupCmd.Flags().Bool("execute", false, "Actually apply changes (default is dry-run)")
	libraryCleanupCmd.Flags().Bool("rename", false, "Fix misnamed files (requires --execute to apply)")
	libraryCleanupCmd.Flags().Bool("delete", false, "Delete orphan files (requires --execute to apply)")
	libraryPruneIncompleteCmd.Flags().Bool("execute", false, "Actually remove directories (default is dry-run)")
	libraryCmd.AddCommand(libraryImportCmd, libraryCleanupCmd, libraryPruneIncompleteCmd, libraryVerifyCmd)

	rootCmd.AddCommand(daemonCmd, statusCmd, movieCmd, tvCmd, queueCmd, plexCmd, historyCmd, blocklistCmd, libraryCmd, configCmd)
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

	fmt.Printf("added: %s (%d) [tmdb=%d]\n", reply.Title, reply.Year, reply.TmdbID)
	if reply.Grabbed {
		fmt.Println("  -> release found and enqueued for download")
	} else {
		fmt.Println("  -> no matching release found on indexers")
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
	fmt.Fprintln(w, "TMDB ID\tTITLE\tYEAR\tSTATUS\tQUALITY")
	for _, m := range reply.Movies {
		q := ""
		if m.Quality.Valid {
			q = m.Quality.String
		}
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n", m.TmdbID, m.Title, m.Year, m.Status, q)
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
	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("TMDB ID must be a number (use 'udl movie search' to find it)")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.SearchMovieArgs{TmdbID: tmdbID}

	var reply daemon.SearchMovieReply
	if err := client.Call("Service.SearchMovie", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Results) == 0 {
		fmt.Println("no releases found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tTITLE\tQUALITY\tSIZE\tSCORE")
	for i, r := range reply.Results {
		size := fmt.Sprintf("%.1f GB", float64(r.Release.Size)/(1024*1024*1024))
		if r.Release.Size < 1024*1024*1024 {
			size = fmt.Sprintf("%.0f MB", float64(r.Release.Size)/(1024*1024))
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\n", i+1, r.Release.Title, r.Quality, size, r.Score)
		if i >= 19 {
			break // show top 20
		}
	}
	return w.Flush()
}

func runMovieGrab(cmd *cobra.Command, args []string) error {
	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("first argument must be a TMDB ID (use 'udl movie search' to find it)")
	}
	index, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("second argument must be a release number (use 'udl movie releases' to find it)")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.GrabMovieReleaseArgs{TmdbID: tmdbID, Index: index}

	var reply daemon.GrabMovieReleaseReply
	if err := client.Call("Service.GrabMovieRelease", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("grabbed: %s (%d) [tmdb=%d]\n", reply.Title, reply.Year, tmdbID)
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

	fmt.Printf("added: %s (%d) [tmdb=%d] — %d episodes\n", reply.Title, reply.Year, reply.TmdbID, reply.EpisodeCount)
	if reply.Grabbed > 0 {
		fmt.Printf("  -> %d episodes enqueued for download\n", reply.Grabbed)
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
	fmt.Fprintln(w, "TMDB ID\tTITLE\tYEAR\tSTATUS")
	for _, s := range reply.Series {
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\n", s.TmdbID, s.Title, s.Year, s.Status)
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
	fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tPROGRESS")
	for _, d := range reply.Items {
		progress := d.Progress
		if progress > 100 {
			progress = 100
		}
		id := mediaTag(d.Category, d.TmdbID, d.Season, d.EpisodeNum, d.MediaID)
		fmt.Fprintf(w, "%s\t%s\t%s\t%.0f%%\n", id, d.Title, d.Status, progress)
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

	var reply daemon.ClearQueueReply
	if err := client.Call("Service.ClearQueue", &daemon.Empty{}, &reply); err != nil {
		return err
	}
	fmt.Printf("cleared %d downloads\n", reply.Cleared)
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

	var reply daemon.HistoryReply
	if err := client.Call("Service.History", &daemon.Empty{}, &reply); err != nil {
		return err
	}

	if len(reply.Events) == 0 {
		fmt.Println("no history")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tEVENT\tQUALITY\tTIME")
	for _, h := range reply.Events {
		q := ""
		if h.Quality.Valid {
			q = h.Quality.String
		}
		id := mediaTag(h.MediaType, h.TmdbID, h.Season, h.EpisodeNum, h.MediaID)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id, h.Title, h.Event, q, h.CreatedAt.Format("2006-01-02 15:04"))
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
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", e.ID, media, e.ReleaseTitle, reason, e.CreatedAt.Format("2006-01-02 15:04"))
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
		return fmt.Errorf("invalid blocklist ID: %s", args[0])
	}

	rpcArgs := &daemon.BlocklistRemoveArgs{ID: id}
	if err := client.Call("Service.BlocklistRemove", rpcArgs, &daemon.Empty{}); err != nil {
		return err
	}
	fmt.Printf("removed blocklist entry %d\n", id)
	return nil
}

func runMovieRemove(cmd *cobra.Command, args []string) error {
	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("TMDB ID must be a number (use 'udl movie list' to find it)")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.RemoveMovieArgs{TmdbID: tmdbID}
	var reply daemon.RemoveMovieReply
	if err := client.Call("Service.RemoveMovie", rpcArgs, &reply); err != nil {
		return err
	}
	fmt.Printf("removed: %s (%d) [tmdb=%d]\n", reply.Title, reply.Year, reply.TmdbID)
	return nil
}

func runTVRemove(cmd *cobra.Command, args []string) error {
	tmdbID, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("TMDB ID must be a number (use 'udl tv list' to find it)")
	}

	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	rpcArgs := &daemon.RemoveSeriesArgs{TmdbID: tmdbID}
	var reply daemon.RemoveSeriesReply
	if err := client.Call("Service.RemoveSeries", rpcArgs, &reply); err != nil {
		return err
	}
	fmt.Printf("removed: %s (%d) [tmdb=%d] (and all episodes)\n", reply.Title, reply.Year, reply.TmdbID)
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

	// Show items in a table.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tTYPE\tTITLE\tQUALITY\tAGE\tSIZE")
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
		age := ""
		if item.AddedDays > 0 {
			age = fmt.Sprintf("%dd", item.AddedDays)
		}
		size := formatSize(item.SizeBytes)
		title := item.Title
		if item.Year > 0 {
			title = fmt.Sprintf("%s (%d)", item.Title, item.Year)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			action, item.MediaType, title, item.Quality, age, size)
	}
	w.Flush()
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
		w.Flush()
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
		w.Flush()
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
	w.Flush()
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
	w.Flush()
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

