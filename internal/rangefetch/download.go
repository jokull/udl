package rangefetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// DefaultBlockSize is the granularity at which a transfer is verified and
// persisted. It matches the shadow block cache so both share one code path.
const DefaultBlockSize = 2 << 20 // 2 MiB

// maxIdentityResets bounds how many times Run will restart from zero when a
// resource's identity keeps changing, so a flapping origin cannot loop forever.
const maxIdentityResets = 3

// Meta is the sidecar that makes a partial transfer resumable across
// attempts and daemon restarts. It is written atomically after every verified
// block, so it never claims more bytes than the partial file actually holds.
type Meta struct {
	URL          string `json:"url"`
	Size         int64  `json:"size"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	BlockSize    int64  `json:"block_size"`
	Completed    int64  `json:"completed"`
}

// Download materializes a remote resource into a single file, block by block,
// resuming from the last verified block instead of restarting at byte zero.
//
// The partial file and its sidecar survive retryable failures; only the caller
// (which knows whether a failure is terminal) calls Discard.
type Download struct {
	path      string
	metaPath  string
	f         *Fetcher
	blockSize int64

	mu        sync.Mutex
	file      *os.File
	res       Resource
	completed int64
}

// Open prepares (resuming where possible) a transfer of res into path.
//
// res.Size must be known — either from the caller's own metadata or from
// Probe — because a resumable transfer is planned in absolute blocks. When an
// existing sidecar describes a different resource, or the caller's size
// disagrees, the partial file is truncated to zero and the sidecar rewritten:
// stale bytes are never appended to.
func Open(f *Fetcher, path string, res Resource, blockSize int64) (*Download, error) {
	if f == nil {
		return nil, errors.New("rangefetch: nil fetcher")
	}
	if path == "" {
		return nil, errors.New("rangefetch: empty path")
	}
	if res.URL == "" {
		return nil, errors.New("rangefetch: empty URL")
	}
	if res.Size <= 0 {
		return nil, fmt.Errorf("%w: probe %s before opening a resumable transfer", ErrUnknownSize, sanitizeURL(res.URL))
	}
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}

	d := &Download{path: path, metaPath: path + ".meta.json", f: f, blockSize: blockSize, res: res}

	if meta, err := loadMeta(d.metaPath); err == nil && meta.BlockSize == blockSize {
		prior := Resource{URL: meta.URL, Size: meta.Size, ETag: meta.ETag, LastModified: meta.LastModified, ContentType: meta.ContentType}
		if res.SameIdentity(prior) {
			// The sidecar may know validators the caller has not learned yet;
			// adopt them so the next Get validates against the pinned version.
			if d.res.ETag == "" {
				d.res.ETag = meta.ETag
			}
			if d.res.LastModified == "" {
				d.res.LastModified = meta.LastModified
			}
			if d.res.ContentType == "" {
				d.res.ContentType = meta.ContentType
			}
			if meta.Completed > 0 && meta.Completed <= res.Size {
				d.completed = meta.Completed
			}
		}
	}

	if err := d.resetTo(d.completed); err != nil {
		return nil, err
	}
	if err := d.saveMeta(); err != nil {
		return nil, err
	}
	return d, nil
}

// URL returns the resource URL.
func (d *Download) URL() string { return d.res.URL }

// Path returns the partial file path.
func (d *Download) Path() string { return d.path }

// Resource returns the resource as currently known, including validators and
// total size learned from the origin.
func (d *Download) Resource() Resource {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.res
}

// Completed returns the number of verified, contiguous bytes from the start.
func (d *Download) Completed() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.completed
}

// Close releases the partial file handle without deleting anything, so a later
// Open can resume. Safe to call more than once.
func (d *Download) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return nil
	}
	err := d.file.Close()
	d.file = nil
	return err
}

// Discard removes the partial file and its sidecar. Callers use it for terminal
// failures and when the candidate release changes; retryable failures must
// leave both in place so the next attempt resumes.
func (d *Download) Discard() error {
	cerr := d.Close()
	rerr := errors.Join(removeIfExists(d.path), removeIfExists(d.metaPath))
	return errors.Join(cerr, rerr)
}

// Run fetches every missing block and returns once the file holds exactly
// res.Size bytes. It is safe to call again after a retryable error: verified
// blocks are not refetched. progress, when non-nil, is called after each
// verified block with the completed and total byte counts.
func (d *Download) Run(ctx context.Context, progress func(completed, total int64)) error {
	resets := 0
	for {
		if err := d.runBlocks(ctx, progress); err != nil {
			if errors.Is(err, ErrRangeIgnored) {
				// The origin does not honour ranges, so nothing already fetched
				// can be trusted to align with an absolute offset: one
				// sequential pass from zero is the only safe transfer.
				return d.runStream(ctx, progress)
			}
			if errors.Is(err, ErrResourceChanged) {
				resets++
				if resets > maxIdentityResets {
					return fmt.Errorf("%w: identity changed %d times", ErrResourceChanged, resets)
				}
				if err := d.restart(); err != nil {
					return err
				}
				continue
			}
			return err
		}
		return d.verifySize()
	}
}

// runBlocks fetches consecutive blocks, persisting verified progress after
// each one so an interruption costs at most the in-flight block. Bytes from a
// truncated response are kept: Fill resumes inside the range, and any partial
// block is written and counted as verified before the error is returned.
func (d *Download) runBlocks(ctx context.Context, progress func(completed, total int64)) error {
	buf := make([]byte, d.blockSize)
	for {
		off := d.Completed()
		res := d.Resource()
		if off >= res.Size {
			return nil
		}
		n := int64(len(buf))
		if rem := res.Size - off; rem < n {
			n = rem
		}

		got, next, err := d.f.Fill(ctx, res, off, buf[:n])
		if !next.SameIdentity(d.res) {
			// The origin is serving a different version; do not write bytes
			// that belong to the new one at the old offsets.
			return fmt.Errorf("%w: size %d -> %d", ErrResourceChanged, d.res.Size, next.Size)
		}
		d.adopt(next)
		if got > 0 {
			if werr := d.writeAt(buf[:got], off); werr != nil {
				return werr
			}
			if aerr := d.advance(off + int64(got)); aerr != nil {
				return aerr
			}
			if progress != nil {
				progress(off+int64(got), next.Size)
			}
		}
		if err != nil {
			return err
		}
	}
}

// runStream transfers the whole resource sequentially in a single request.
// Used when the origin ignores Range requests, where resuming by offset is
// impossible; progress is still persisted per block so a completed prefix is
// visible, but a restart necessarily begins at zero.
func (d *Download) runStream(ctx context.Context, progress func(completed, total int64)) error {
	if err := d.restart(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.res.URL, nil)
	if err != nil {
		return fmt.Errorf("rangefetch: build request: %w", err)
	}
	// Origins that ignore Range are also the ones observed holding the
	// connection open after the body, so this one-shot request must not leave
	// a reusable connection behind.
	req.Close = true
	resp, err := d.f.Client.Do(req)
	if err != nil {
		return &TransportError{Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	default:
		return &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, URL: d.res.URL}
	}

	if d.res.Size <= 0 {
		if resp.ContentLength <= 0 {
			return ErrUnknownSize
		}
		d.mu.Lock()
		d.res.Size = resp.ContentLength
		d.mu.Unlock()
	}

	buf := make([]byte, d.blockSize)
	for {
		res := d.Resource()
		off := d.Completed()
		if off >= res.Size {
			return nil
		}
		n := d.blockSize
		if rem := res.Size - off; rem < n {
			n = rem
		}

		read, rerr := io.ReadFull(resp.Body, buf[:n])
		if read > 0 {
			if err := d.writeAt(buf[:read], off); err != nil {
				return err
			}
			if err := d.advance(off + int64(read)); err != nil {
				return err
			}
			if progress != nil {
				progress(off+int64(read), res.Size)
			}
		}
		if rerr != nil {
			// Some Plex servers send the full body but leave the connection
			// open instead of closing it, so EOF never arrives; a filled
			// resource ends the transfer.
			if d.Completed() >= d.Resource().Size {
				return nil
			}
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return fmt.Errorf("%w: got %d of %d bytes", ErrShortRead, d.Completed(), d.Resource().Size)
			}
			return &TransportError{Err: rerr}
		}
	}
}

// adopt records validators learned from the origin without changing progress.
func (d *Download) adopt(next Resource) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.res.Size <= 0 {
		d.res.Size = next.Size
	}
	if d.res.ETag == "" {
		d.res.ETag = next.ETag
	}
	if d.res.LastModified == "" {
		d.res.LastModified = next.LastModified
	}
	if d.res.ContentType == "" {
		d.res.ContentType = next.ContentType
	}
}

// restart invalidates all verified progress for this resource.
func (d *Download) restart() error {
	return d.resetTo(0)
}

// resetTo truncates the partial file to keep bytes and records that as the
// verified prefix.
func (d *Download) resetTo(keep int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	if d.file == nil {
		f, err := os.OpenFile(d.path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return err
		}
		d.file = f
	}
	// Never resume past what the file actually holds: truncating up would
	// invent zero bytes that the origin never supplied.
	if st, err := d.file.Stat(); err == nil && keep > st.Size() {
		keep = st.Size()
	}
	if err := d.file.Truncate(keep); err != nil {
		return err
	}
	d.completed = keep
	return nil
}

// advance records a new verified prefix and persists the sidecar.
func (d *Download) advance(completed int64) error {
	d.mu.Lock()
	d.completed = completed
	d.mu.Unlock()
	return d.saveMeta()
}

func (d *Download) writeAt(data []byte, off int64) error {
	d.mu.Lock()
	file := d.file
	d.mu.Unlock()
	if file == nil {
		return errors.New("rangefetch: download is closed")
	}
	if _, err := file.WriteAt(data, off); err != nil {
		return fmt.Errorf("rangefetch: write at %d: %w", off, err)
	}
	return nil
}

// verifySize refuses to declare a transfer complete unless the file holds
// exactly the expected number of bytes.
func (d *Download) verifySize() error {
	res := d.Resource()
	info, err := os.Stat(d.path)
	if err != nil {
		return err
	}
	if info.Size() != res.Size {
		return fmt.Errorf("%w: have %d bytes, expected %d", ErrSizeMismatch, info.Size(), res.Size)
	}
	return nil
}

func (d *Download) saveMeta() error {
	res := d.Resource()
	meta := Meta{
		URL:          res.URL,
		Size:         res.Size,
		ETag:         res.ETag,
		LastModified: res.LastModified,
		ContentType:  res.ContentType,
		BlockSize:    d.blockSize,
		Completed:    d.Completed(),
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(d.metaPath), ".meta-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), d.metaPath)
}

func loadMeta(path string) (Meta, error) {
	var meta Meta
	data, err := os.ReadFile(path)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
