package nntp

import (
	"fmt"
	"log/slog"
	"sync"
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
	config ProviderConfig
	conns  chan *Conn
	mu     sync.Mutex
	active int
	log    *slog.Logger
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
// Blocks if all connections are in use.
func (p *Pool) Get() (*Conn, error) {
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

// Put returns a connection to the pool.
func (p *Pool) Put(conn *Conn) {
	if conn == nil {
		return
	}
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
