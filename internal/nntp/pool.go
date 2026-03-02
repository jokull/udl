package nntp

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ProviderConfig holds connection details for a Usenet provider.
type ProviderConfig struct {
	Name        string
	Host        string
	Port        int
	TLS         bool
	Username    string
	Password    string
	Connections int
	Level       int // 0 = primary, 1 = fill
}

// Pool manages a pool of NNTP connections to a single provider.
type Pool struct {
	config           ProviderConfig
	conns            chan *Conn
	mu               sync.Mutex
	active           int
	consecutiveFails int
	backoffUntil     time.Time
	log              *slog.Logger
}

// backoffDuration returns the backoff duration for the given number of consecutive failures.
func backoffDuration(fails int) time.Duration {
	durations := []time.Duration{5, 15, 30, 60, 120, 300}
	idx := fails - 1
	if idx < 0 {
		return 0
	}
	if idx >= len(durations) {
		idx = len(durations) - 1
	}
	return durations[idx] * time.Second
}

// NewPool creates a connection pool for a provider.
// Connections are created lazily up to config.Connections.
func NewPool(config ProviderConfig, log *slog.Logger) *Pool {
	return &Pool{
		config: config,
		conns:  make(chan *Conn, config.Connections),
		log:    log,
	}
}

// Get returns an available connection, creating one if needed and under the limit.
// Blocks if all connections are in use. Returns an error if the pool is in backoff.
func (p *Pool) Get() (*Conn, error) {
	// Check backoff before attempting anything.
	p.mu.Lock()
	if time.Now().Before(p.backoffUntil) {
		remaining := time.Until(p.backoffUntil)
		p.mu.Unlock()
		return nil, fmt.Errorf("pool %s: in backoff for %v", p.config.Name, remaining.Truncate(time.Second))
	}
	p.mu.Unlock()

	for {
		// Try to grab an idle connection without blocking.
		select {
		case c := <-p.conns:
			if c == nil {
				// nil sentinel from Return() — a slot freed up, retry.
				continue
			}
			return c, nil
		default:
		}

		// Try to create a new connection if under the limit.
		p.mu.Lock()
		if p.active < p.config.Connections {
			p.active++
			p.mu.Unlock()

			c, err := p.dial()
			if err != nil {
				p.mu.Lock()
				p.active--
				p.consecutiveFails++
				p.backoffUntil = time.Now().Add(backoffDuration(p.consecutiveFails))
				p.log.Warn("provider connection failed, entering backoff",
					"provider", p.config.Name,
					"consecutive_fails", p.consecutiveFails,
					"backoff", backoffDuration(p.consecutiveFails),
				)
				p.mu.Unlock()
				return nil, err
			}
			return c, nil
		}
		p.mu.Unlock()

		// All connections in use — block until one is returned.
		c := <-p.conns
		if c == nil {
			// nil sentinel from Return() — a slot freed up, retry.
			continue
		}
		return c, nil
	}
}

// Put returns a connection to the pool. Resets backoff on success since the provider is healthy.
func (p *Pool) Put(conn *Conn) {
	if conn == nil {
		return
	}
	p.mu.Lock()
	if p.consecutiveFails > 0 {
		p.consecutiveFails = 0
		p.backoffUntil = time.Time{}
	}
	p.mu.Unlock()
	select {
	case p.conns <- conn:
	default:
		// Pool is full (shouldn't happen), close the extra connection.
		conn.Close()
	}
}

// Return discards a broken connection and decrements the active count
// so a new connection can be dialed on the next Get.
// Sends a nil sentinel to wake up any goroutine blocked in Get().
func (p *Pool) Return(conn *Conn) {
	if conn != nil {
		conn.Close()
	}
	p.mu.Lock()
	p.active--
	p.mu.Unlock()

	// Wake up a blocked Get() caller by sending a nil sentinel.
	select {
	case p.conns <- nil:
	default:
	}
}

// PoolStatus holds a read-only snapshot of a pool's health state.
type PoolStatus struct {
	Name             string
	Active           int
	MaxConnections   int
	ConsecutiveFails int
	InBackoff        bool
	BackoffRemaining time.Duration
}

// Status returns a read-only snapshot of the pool's current health state.
func (p *Pool) Status() PoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps := PoolStatus{
		Name:             p.config.Name,
		Active:           p.active,
		MaxConnections:   p.config.Connections,
		ConsecutiveFails: p.consecutiveFails,
	}
	if time.Now().Before(p.backoffUntil) {
		ps.InBackoff = true
		ps.BackoffRemaining = time.Until(p.backoffUntil)
	}
	return ps
}

// Close closes all idle connections in the pool.
func (p *Pool) Close() {
	close(p.conns)
	for c := range p.conns {
		c.Close()
	}
}

// dial creates and authenticates a new connection.
func (p *Pool) dial() (*Conn, error) {
	p.log.Info("connecting to provider",
		"provider", p.config.Name,
		"host", p.config.Host,
		"active", p.active,
		"max", p.config.Connections,
	)

	c, err := Dial(p.config.Host, p.config.Port, p.config.TLS)
	if err != nil {
		return nil, fmt.Errorf("pool %s: %w", p.config.Name, err)
	}

	if p.config.Username != "" {
		if err := c.Auth(p.config.Username, p.config.Password); err != nil {
			c.Close()
			return nil, fmt.Errorf("pool %s: auth: %w", p.config.Name, err)
		}
	}

	p.log.Info("connected to provider", "provider", p.config.Name)
	return c, nil
}
