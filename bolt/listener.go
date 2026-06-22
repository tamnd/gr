package bolt

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"
)

// ErrListenerClosed is returned by Serve after Close or Shutdown stops the accept
// loop (doc 18 §8.7).
var ErrListenerClosed = errors.New("bolt: listener closed")

// TLSMode is the Bolt listener's transport security posture (doc 18 §11.1). Bolt
// has no in-band STARTTLS: a connection is TLS from the first byte or plaintext
// from the first byte, decided here before any Bolt byte flows.
type TLSMode int

const (
	// TLSDisabled serves plaintext only; a TLS ClientHello is not Bolt and the
	// handshake rejects it.
	TLSDisabled TLSMode = iota
	// TLSOptional sniffs the first byte: a TLS ClientHello (0x16) starts a TLS
	// handshake, the Bolt magic (0x60) starts a plaintext connection.
	TLSOptional
	// TLSRequired serves TLS only; a plaintext preamble is rejected.
	TLSRequired
)

// Listener accepts TCP connections and runs the Bolt protocol on each through a
// Server (doc 18 §8). One goroutine handles each connection (doc 18 §8.2); a
// configurable cap bounds how many run at once (doc 18 §8.8), and Shutdown drains
// live connections gracefully (doc 18 §8.7).
type Listener struct {
	// Server runs the protocol on each accepted connection (required).
	Server *Server
	// Addr is the listen address, default ":7687" (doc 18 §1.4).
	Addr string
	// TLSMode selects the transport security posture (doc 18 §11.1).
	TLSMode TLSMode
	// TLSConfig is the server TLS configuration, required for TLSRequired and for
	// the TLS branch of TLSOptional.
	TLSConfig *tls.Config
	// MaxConnections caps concurrent connections; 0 means unlimited (doc 18 §8.8).
	MaxConnections int
	// IdleTimeout bounds how long a connection may be silent between messages; a
	// read that exceeds it terminates the connection (doc 18 §8.7). 0 disables it.
	IdleTimeout time.Duration

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	sem    chan struct{}
	closed bool
	wg     sync.WaitGroup
}

// ListenAndServe opens the listen socket and serves until Close or Shutdown.
func (l *Listener) ListenAndServe() error {
	addr := l.Addr
	if addr == "" {
		addr = ":7687"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return l.Serve(ln)
}

// Serve accepts connections on ln and handles each on its own goroutine until
// Close or Shutdown, then returns ErrListenerClosed (doc 18 §8.7).
func (l *Listener) Serve(ln net.Listener) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		ln.Close()
		return ErrListenerClosed
	}
	l.ln = ln
	l.conns = map[net.Conn]struct{}{}
	if l.MaxConnections > 0 {
		l.sem = make(chan struct{}, l.MaxConnections)
	}
	l.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed {
				return ErrListenerClosed
			}
			// A transient accept error (e.g. too many open files): pause briefly
			// and keep serving rather than tearing the listener down.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		// Admission: past the cap, refuse immediately so the process is not
		// overwhelmed (doc 18 §8.8). The driver retries or fails over to itself.
		if l.sem != nil {
			select {
			case l.sem <- struct{}{}:
			default:
				conn.Close()
				continue
			}
		}
		l.track(conn)
		l.wg.Add(1)
		go l.handle(conn)
	}
}

// handle runs one connection: secure it per the TLS posture, wrap it for idle
// timeouts, and serve the Bolt protocol, cleaning up on every termination path.
func (l *Listener) handle(conn net.Conn) {
	defer l.wg.Done()
	defer func() {
		l.untrack(conn)
		if l.sem != nil {
			<-l.sem
		}
	}()

	secured, err := l.secure(conn)
	if err != nil {
		return
	}
	_ = l.Server.Serve(idleConn{Conn: secured, idle: l.IdleTimeout})
}

// secure applies the TLS posture to a freshly accepted connection (doc 18 §11.1).
func (l *Listener) secure(conn net.Conn) (net.Conn, error) {
	switch l.TLSMode {
	case TLSRequired:
		if l.TLSConfig == nil {
			conn.Close()
			return nil, errors.New("bolt: TLS required but no TLS config set")
		}
		return tls.Server(conn, l.TLSConfig), nil
	case TLSOptional:
		// One-byte peek routes the connection: TLS records begin 0x16, the Bolt
		// preamble begins 0x60, which never collide (doc 18 §11.1).
		if l.IdleTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(l.IdleTimeout))
		}
		var first [1]byte
		n, err := conn.Read(first[:])
		if err != nil || n == 0 {
			conn.Close()
			return nil, errors.New("bolt: empty connection")
		}
		_ = conn.SetReadDeadline(time.Time{})
		pc := &prefixConn{Conn: conn, prefix: []byte{first[0]}}
		switch first[0] {
		case 0x16:
			if l.TLSConfig == nil {
				conn.Close()
				return nil, errors.New("bolt: TLS ClientHello but no TLS config set")
			}
			return tls.Server(pc, l.TLSConfig), nil
		case 0x60:
			return pc, nil
		default:
			conn.Close()
			return nil, errors.New("bolt: unrecognized client preamble")
		}
	default: // TLSDisabled
		return conn, nil
	}
}

// Close stops the accept loop without waiting for live connections (doc 18 §8.7).
func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.ln != nil {
		return l.ln.Close()
	}
	return nil
}

// Shutdown stops accepting and waits for live connections to finish, force
// closing any that remain when ctx expires (doc 18 §8.7).
func (l *Listener) Shutdown(ctx context.Context) error {
	l.Close()
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		for c := range l.conns {
			c.Close()
		}
		l.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func (l *Listener) track(conn net.Conn) {
	l.mu.Lock()
	l.conns[conn] = struct{}{}
	l.mu.Unlock()
}

func (l *Listener) untrack(conn net.Conn) {
	l.mu.Lock()
	delete(l.conns, conn)
	l.mu.Unlock()
	conn.Close()
}

// idleConn resets the read deadline before each read so a connection silent
// longer than the idle timeout is torn down (doc 18 §8.7).
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Read(p)
}

// prefixConn replays bytes already read off the connection (the sniffed preamble)
// before reading from the underlying connection, so the peek is not lost.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}
