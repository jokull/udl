// Package nntp provides an NNTP client for downloading articles from Usenet.
package nntp

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Conn represents a single authenticated NNTP connection.
type Conn struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
}

const dialTimeout = 30 * time.Second

// Dial connects to an NNTP server. If useTLS is true, uses TLS.
func Dial(host string, port int, useTLS bool) (*Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var rawConn net.Conn
	var err error

	if useTLS {
		dialer := &net.Dialer{Timeout: dialTimeout}
		rawConn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: host,
		})
	} else {
		rawConn, err = net.DialTimeout("tcp", addr, dialTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("nntp: dial %s: %w", addr, err)
	}

	c := &Conn{
		conn:   rawConn,
		reader: bufio.NewReaderSize(rawConn, 64*1024),
		writer: bufio.NewWriter(rawConn),
	}

	// Read greeting line. 200 = posting allowed, 201 = no posting.
	code, _, err := c.readResponse()
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("nntp: greeting: %w", err)
	}
	if code != 200 && code != 201 {
		rawConn.Close()
		return nil, fmt.Errorf("nntp: unexpected greeting code %d", code)
	}

	return c, nil
}

// Auth authenticates with the server using AUTHINFO USER/PASS.
func (c *Conn) Auth(username, password string) error {
	// Send username.
	if err := c.sendCommand("AUTHINFO USER " + username); err != nil {
		return fmt.Errorf("nntp: auth user: %w", err)
	}
	code, _, err := c.readResponse()
	if err != nil {
		return fmt.Errorf("nntp: auth user response: %w", err)
	}
	if code != 381 {
		return fmt.Errorf("nntp: AUTHINFO USER: unexpected code %d (expected 381)", code)
	}

	// Send password.
	if err := c.sendCommand("AUTHINFO PASS " + password); err != nil {
		return fmt.Errorf("nntp: auth pass: %w", err)
	}
	code, _, err = c.readResponse()
	if err != nil {
		return fmt.Errorf("nntp: auth pass response: %w", err)
	}
	if code != 281 {
		return fmt.Errorf("nntp: AUTHINFO PASS: unexpected code %d (expected 281)", code)
	}

	return nil
}

const bodyTimeout = 60 * time.Second

// maxBodySize is the maximum size of a single NNTP article body.
// yEnc segments are typically 500KB-750KB; 2MB provides ample headroom.
const maxBodySize = 2 * 1024 * 1024

// Body fetches the body of an article by message ID.
// Returns the raw body as an io.Reader. The caller must read fully before
// issuing the next command on this connection.
// Sets a 60s read deadline to prevent stuck workers.
func (c *Conn) Body(messageID string) (io.Reader, error) {
	c.conn.SetDeadline(time.Now().Add(bodyTimeout))
	defer c.conn.SetDeadline(time.Time{})

	// Ensure message ID is wrapped in angle brackets.
	if !strings.HasPrefix(messageID, "<") {
		messageID = "<" + messageID + ">"
	}

	if err := c.sendCommand("BODY " + messageID); err != nil {
		return nil, fmt.Errorf("nntp: body command: %w", err)
	}

	code, _, err := c.readResponse()
	if err != nil {
		return nil, fmt.Errorf("nntp: body response: %w", err)
	}
	if code != 222 {
		return nil, fmt.Errorf("nntp: BODY: unexpected code %d (expected 222) for %s", code, messageID)
	}

	// Read multi-line body, handling dot-unstuffing.
	// Lines starting with ".." have the leading dot removed.
	// A line containing only "." terminates the body.
	var buf bytes.Buffer
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("nntp: reading body: %w", err)
		}

		// Strip trailing \r\n.
		trimmed := strings.TrimRight(line, "\r\n")

		// Termination line: a single dot.
		if trimmed == "." {
			break
		}

		// Dot-unstuffing: lines starting with ".." become ".".
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}

		buf.WriteString(trimmed)
		buf.WriteByte('\n')

		if buf.Len() > maxBodySize {
			return nil, fmt.Errorf("nntp: body exceeds %d bytes limit", maxBodySize)
		}
	}

	return &buf, nil
}

// Close closes the connection after sending QUIT.
func (c *Conn) Close() error {
	_ = c.sendCommand("QUIT")
	// Read response but don't fail if server closes early.
	_, _, _ = c.readResponse()
	return c.conn.Close()
}

// sendCommand writes a command line to the server.
func (c *Conn) sendCommand(cmd string) error {
	if _, err := c.writer.WriteString(cmd + "\r\n"); err != nil {
		return err
	}
	return c.writer.Flush()
}

// readResponse reads a single response line and parses the numeric code.
func (c *Conn) readResponse() (int, string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimRight(line, "\r\n")

	if len(line) < 3 {
		return 0, "", fmt.Errorf("nntp: response too short: %q", line)
	}

	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return 0, "", fmt.Errorf("nntp: invalid response code in %q: %w", line, err)
	}

	msg := ""
	if len(line) > 4 {
		msg = line[4:]
	}

	return code, msg, nil
}
