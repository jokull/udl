package shadowfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Report is what a running shadow agent says about itself. The agent rewrites
// it every reportEvery; the daemon reads it for 'udl status' and 'udl doctor'.
// A report that stops updating means the agent is hung or gone — the one
// failure the agent cannot report itself.
type Report struct {
	Name            string           `json:"name"`
	PID             int              `json:"pid"`
	StartedAt       time.Time        `json:"started_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Items           int              `json:"items"`
	ManifestBuiltAt time.Time        `json:"manifest_built_at"`
	LastReload      time.Time        `json:"last_reload,omitzero"`
	LastReloadError string           `json:"last_reload_error,omitempty"`
	Deferred        int              `json:"deferred"`
	BytesServed     int64            `json:"bytes_served"`
	CacheHits       int64            `json:"cache_hits"`
	Fetches         int64            `json:"fetches"`
	Revalidations   int64            `json:"revalidations"`
	Prefetched      int64            `json:"prefetched"`
	SlowReads       int64            `json:"slow_reads"`
	ReadErrors      int64            `json:"read_errors"`
	ErrorsByClass   map[string]int64 `json:"errors_by_class,omitempty"`
	Recent          []ReadFailure    `json:"recent,omitempty"`

	// The agent's own client-side check of its mount (daemon mode only).
	MountChecked bool   `json:"mount_checked"`
	MountOK      bool   `json:"mount_ok"`
	MountError   string `json:"mount_error,omitempty"`
	Remounts     int    `json:"remounts"`
}

// ReportEvery is how often a serving agent rewrites its report.
const ReportEvery = 30 * time.Second

// WriteReport replaces the report at path atomically.
func WriteReport(path string, r *Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".report-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		if werr != nil {
			return werr
		}
		return cerr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ReadReport loads the report at path.
func ReadReport(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
