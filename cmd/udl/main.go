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
	Use:   "add [query]",
	Short: "Add a movie to wanted list",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMovieAdd,
}

var movieListCmd = &cobra.Command{
	Use:   "list",
	Short: "List wanted and downloaded movies",
	RunE:  runMovieList,
}

var movieSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search indexers for a movie",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMovieSearch,
}

var tvCmd = &cobra.Command{
	Use:   "tv",
	Short: "Manage TV series",
}

var tvAddCmd = &cobra.Command{
	Use:   "add [query]",
	Short: "Add a TV series to monitor",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runTVAdd,
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
	Use:   "retry [id]",
	Short: "Retry failed downloads (all or by ID)",
	RunE:  runQueueRetry,
}

var movieRemoveCmd = &cobra.Command{
	Use:   "remove [id|title]",
	Short: "Remove a movie from monitoring (not from disk)",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMovieRemove,
}

var tvRemoveCmd = &cobra.Command{
	Use:   "remove [id|title]",
	Short: "Remove a series from monitoring (not from disk)",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runTVRemove,
}

var tvRefreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Refresh episode metadata from TMDB for all monitored series",
	RunE:  runTVRefresh,
}

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Show download history",
	RunE:  runHistory,
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
	movieCmd.AddCommand(movieAddCmd, movieListCmd, movieSearchCmd, movieRemoveCmd)
	tvCmd.AddCommand(tvAddCmd, tvListCmd, tvRemoveCmd, tvRefreshCmd)
	queueCmd.AddCommand(queuePauseCmd, queueResumeCmd, queueClearCmd, queueRetryCmd)
	configCmd.AddCommand(configCheckCmd, configPathCmd)
	rootCmd.AddCommand(daemonCmd, statusCmd, movieCmd, tvCmd, queueCmd, historyCmd, configCmd)
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
	os.MkdirAll(dataDir, 0755)

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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig)
		cancel()
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

	fmt.Println("daemon: running")
	fmt.Printf("queue: %d items (%d downloading)\n", reply.QueueSize, reply.Downloading)
	fmt.Printf("indexers: %d\n", reply.IndexerCount)
	fmt.Printf("movies: %d  series: %d\n", reply.MovieCount, reply.SeriesCount)
	fmt.Printf("library: %s (movies), %s (tv)\n", reply.LibraryMovies, reply.LibraryTV)
	return nil
}

func runMovieAdd(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	query := strings.Join(args, " ")
	rpcArgs := &daemon.AddMovieArgs{Query: query}
	var reply daemon.AddMovieReply
	if err := client.Call("Service.AddMovie", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("added: %s (%d)\n", reply.Title, reply.Year)
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
	fmt.Fprintln(w, "TITLE\tYEAR\tSTATUS\tQUALITY")
	for _, m := range reply.Movies {
		q := ""
		if m.Quality.Valid {
			q = m.Quality.String
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", m.Title, m.Year, m.Status, q)
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
	rpcArgs := &daemon.SearchMovieArgs{Query: query}
	var reply daemon.SearchMovieReply
	if err := client.Call("Service.SearchMovie", rpcArgs, &reply); err != nil {
		return err
	}

	if len(reply.Results) == 0 {
		fmt.Println("no results found")
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

func runTVAdd(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	query := strings.Join(args, " ")
	rpcArgs := &daemon.AddSeriesArgs{Query: query}
	var reply daemon.AddSeriesReply
	if err := client.Call("Service.AddSeries", rpcArgs, &reply); err != nil {
		return err
	}

	fmt.Printf("added: %s (%d) — %d episodes\n", reply.Title, reply.Year, reply.EpisodeCount)
	if reply.Grabbed > 0 {
		fmt.Printf("  -> %d episodes enqueued for download\n", reply.Grabbed)
	}
	return nil
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
	fmt.Fprintln(w, "TITLE\tYEAR\tSTATUS")
	for _, s := range reply.Series {
		fmt.Fprintf(w, "%s\t%d\t%s\n", s.Title, s.Year, s.Status)
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

	if len(reply.Downloads) == 0 {
		fmt.Println("queue empty")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TITLE\tSTATUS\tPROGRESS")
	for _, d := range reply.Downloads {
		progress := d.Progress
		if progress > 100 {
			progress = 100
		}
		fmt.Fprintf(w, "%s\t%s\t%.0f%%\n", d.Title, d.Status, progress)
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
	fmt.Fprintln(w, "TYPE\tTITLE\tEVENT\tQUALITY\tTIME")
	for _, h := range reply.Events {
		q := ""
		if h.Quality.Valid {
			q = h.Quality.String
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", h.MediaType, h.Title, h.Event, q, h.CreatedAt.Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

func runMovieRemove(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	input := strings.Join(args, " ")
	rpcArgs := &daemon.RemoveMovieArgs{}
	if id, err := strconv.ParseInt(input, 10, 64); err == nil {
		rpcArgs.ID = id
	} else {
		rpcArgs.Title = input
	}

	var reply daemon.RemoveMovieReply
	if err := client.Call("Service.RemoveMovie", rpcArgs, &reply); err != nil {
		return err
	}
	fmt.Printf("removed: %s\n", reply.Title)
	return nil
}

func runTVRemove(cmd *cobra.Command, args []string) error {
	client, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w", err)
	}
	defer client.Close()

	input := strings.Join(args, " ")
	rpcArgs := &daemon.RemoveSeriesArgs{}
	if id, err := strconv.ParseInt(input, 10, 64); err == nil {
		rpcArgs.ID = id
	} else {
		rpcArgs.Title = input
	}

	var reply daemon.RemoveSeriesReply
	if err := client.Call("Service.RemoveSeries", rpcArgs, &reply); err != nil {
		return err
	}
	fmt.Printf("removed: %s (and all episodes)\n", reply.Title)
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
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid download ID: %s", args[0])
		}
		rpcArgs.ID = id
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

