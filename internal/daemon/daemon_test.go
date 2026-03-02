package daemon

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"net/rpc"
	"testing"

	"github.com/jokull/udl/internal/config"
	"github.com/jokull/udl/internal/database"
)

// startTestServer starts a daemon RPC server on a temporary Unix socket and
// returns an *rpc.Client connected to it. The server and socket are cleaned
// up when the test finishes.
func startTestServer(t *testing.T) *rpc.Client {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "udl-test.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen(unix, %s): %v", sockPath, err)
	}

	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	go serve(ln, cfg, db, nil, log)
	t.Cleanup(func() { ln.Close() })

	client, err := rpc.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("rpc.Dial(unix, %s): %v", sockPath, err)
	}
	t.Cleanup(func() { client.Close() })

	return client
}

func TestListMoviesEmpty(t *testing.T) {
	client := startTestServer(t)

	var reply MovieListReply
	if err := client.Call("Service.ListMovies", &Empty{}, &reply); err != nil {
		t.Fatalf("Service.ListMovies: %v", err)
	}
	if len(reply.Movies) != 0 {
		t.Errorf("ListMovies: got %d movies, want 0", len(reply.Movies))
	}
}

func TestListSeriesEmpty(t *testing.T) {
	client := startTestServer(t)

	var reply SeriesListReply
	if err := client.Call("Service.ListSeries", &Empty{}, &reply); err != nil {
		t.Fatalf("Service.ListSeries: %v", err)
	}
	if len(reply.Series) != 0 {
		t.Errorf("ListSeries: got %d series, want 0", len(reply.Series))
	}
}

func TestStatusRunning(t *testing.T) {
	client := startTestServer(t)

	var reply StatusReply
	if err := client.Call("Service.Status", &Empty{}, &reply); err != nil {
		t.Fatalf("Service.Status: %v", err)
	}
	if !reply.Running {
		t.Error("Status.Running = false, want true")
	}
}

func TestAddMovieNoTMDB(t *testing.T) {
	client := startTestServer(t)

	var reply AddMovieReply
	err := client.Call("Service.AddMovie", &AddMovieArgs{Query: "Fight Club"}, &reply)
	if err == nil {
		t.Fatal("Service.AddMovie: expected error when TMDB client is nil, got nil")
	}
}

func TestQueueEmpty(t *testing.T) {
	client := startTestServer(t)

	var reply QueueReply
	if err := client.Call("Service.Queue", &Empty{}, &reply); err != nil {
		t.Fatalf("Service.Queue: %v", err)
	}
	if len(reply.Downloads) != 0 {
		t.Errorf("Queue: got %d downloads, want 0", len(reply.Downloads))
	}
}

func TestHistoryEmpty(t *testing.T) {
	client := startTestServer(t)

	var reply HistoryReply
	if err := client.Call("Service.History", &Empty{}, &reply); err != nil {
		t.Fatalf("Service.History: %v", err)
	}
	if len(reply.Events) != 0 {
		t.Errorf("History: got %d events, want 0", len(reply.Events))
	}
}
