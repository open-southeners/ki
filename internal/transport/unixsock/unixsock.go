// Package unixsock carries JSON-RPC over a Unix domain socket using
// newline-delimited framing: one message per line, at most MaxLine bytes.
package unixsock

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

// MaxLine is the maximum accepted message size in bytes (excluding the newline).
const MaxLine = 4 << 20

// ErrLineTooLong is returned by Read when a line exceeds MaxLine.
var ErrLineTooLong = errors.New("unixsock: message exceeds maximum line length")

// Listener accepts Unix socket connections as rpc.Conn values.
type Listener struct {
	ln   *net.UnixListener
	path string
}

// Listen removes a stale socket at path (refusing if the path exists but is not
// a socket), binds it, and applies mode to the socket file.
func Listen(path string, mode os.FileMode) (*Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("unixsock: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("unixsock: remove stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("unixsock: stat %s: %w", path, err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("unixsock: chmod %s: %w", path, err)
	}
	return &Listener{ln: ln, path: path}, nil
}

// Accept waits for the next connection or for ctx cancellation.
func (l *Listener) Accept(ctx context.Context) (rpc.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = l.ln.SetDeadline(pastTime()) })
	c, err := l.ln.Accept()
	if !stop() {
		// Cancellation fired; clear the deadline so the listener stays usable.
		_ = l.ln.SetDeadline(zeroTime())
		if err == nil {
			_ = c.Close()
		}
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return newConn(c), nil
}

// Addr returns the socket path.
func (l *Listener) Addr() string { return l.path }

// Close stops listening and removes the socket file.
func (l *Listener) Close() error {
	return l.ln.Close() // UnixListener unlinks the path on Close.
}

// Dial connects to a Unix socket served by Listener.
func Dial(ctx context.Context, path string) (rpc.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	return newConn(c), nil
}

type conn struct {
	c  net.Conn
	br *bufio.Reader

	rmu     sync.Mutex // guards pending and reads
	pending []byte     // partial line retained across interrupted reads

	wmu sync.Mutex
}

func newConn(c net.Conn) *conn {
	return &conn{c: c, br: bufio.NewReaderSize(c, 64<<10)}
}

// guard arms a deadline that fires when ctx is cancelled; the returned func
// disarms it and must be called once the I/O finishes.
func guard(ctx context.Context, set func(time.Time)) func() {
	set(zeroTime())
	stop := context.AfterFunc(ctx, func() { set(pastTime()) })
	return func() {
		stop() // if it already fired, the next operation resets the deadline
	}
}

func (c *conn) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	disarm := guard(ctx, func(t time.Time) { _ = c.c.SetReadDeadline(t) })
	defer disarm()
	for {
		chunk, err := c.br.ReadSlice('\n')
		if len(c.pending)+len(chunk) > MaxLine+1 {
			c.pending = nil
			return nil, ErrLineTooLong
		}
		c.pending = append(c.pending, chunk...)
		switch {
		case err == nil:
			line := bytes.TrimSuffix(c.pending, []byte("\n"))
			c.pending = nil
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				c.pending = nil
			}
			return nil, err
		}
	}
}

func (c *conn) Write(ctx context.Context, msg []byte) error {
	if bytes.ContainsAny(msg, "\n") {
		return errors.New("unixsock: message contains a raw newline")
	}
	if len(msg) > MaxLine {
		return ErrLineTooLong
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	disarm := guard(ctx, func(t time.Time) { _ = c.c.SetWriteDeadline(t) })
	defer disarm()
	buf := make([]byte, 0, len(msg)+1)
	buf = append(append(buf, msg...), '\n')
	if _, err := c.c.Write(buf); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func (c *conn) Close() error { return c.c.Close() }

func zeroTime() time.Time { return time.Time{} }
func pastTime() time.Time { return time.Unix(1, 0) }
