// Package hub lets several `telegram-mcp serve` processes share one
// Telegram connection.
//
// MCP clients start one server process per consumer (Claude Desktop starts
// one for chat and another for Cowork and Code sessions), but two
// connections on one auth key make Telegram drop both. So the first process
// takes the profile lock, connects to Telegram and listens on a unix socket
// in the profile's data directory; every later process, finding the lock
// taken, connects to that socket and relays its stdio over it. Each relayed
// client gets its own MCP session on the primary.
package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxSocketPath is the sun_path limit on macOS (104 bytes with the NUL);
// Linux allows 108, so the smaller one is used everywhere.
const maxSocketPath = 103

// SocketPath is the socket for one account, binary version and mode.
//
// Read-only and full access listen separately so that a process started
// with --read-only can never reach the write tools through the primary.
// The version is part of the name so that a relay never serves another
// version's tools: after an upgrade, a new relay must not silently hand
// the client the old primary's tool set.
func SocketPath(dataDir, account, version string, readOnly bool) string {
	name := "serve-" + account + "-v" + safeVersion(version)
	if readOnly {
		name += ".ro"
	}
	return filepath.Join(dataDir, name+".sock")
}

func safeVersion(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, v)
}

// otherVersions lists versions with a socket for account other than path's.
func otherVersions(path string) []string {
	dir, base := filepath.Split(path)
	account, _, _ := strings.Cut(strings.TrimPrefix(base, "serve-"), "-v")
	matches, _ := filepath.Glob(filepath.Join(dir, "serve-"+account+"-v*.sock"))
	var out []string
	for _, m := range matches {
		if m == path || strings.HasSuffix(m, ".ro.sock") {
			continue
		}
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "serve-"+account+"-v"), ".sock")
		out = append(out, v)
	}
	return out
}

// Listen replaces a stale socket at path and listens on it, owner-only.
// The caller must hold the profile lock, so no other primary can be using it.
func Listen(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is longer than %d bytes; use a shorter TELEGRAM_MCP_HOME", path, maxSocketPath)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve runs one MCP session on srv per accepted connection until ctx ends,
// then closes the listener, removes the socket and waits for the sessions.
func Serve(ctx context.Context, ln net.Listener, srv *mcp.Server, log *slog.Logger) {
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	defer func() {
		wg.Wait()
		if a, ok := ln.Addr().(*net.UnixAddr); ok {
			_ = os.Remove(a.Name)
		}
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				log.Error("accept", "err", err)
			}
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Closing the conn ends the session if ctx ends first.
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			ss, err := srv.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
			if err != nil {
				conn.Close()
				log.Error("relay session", "err", err)
				return
			}
			_ = ss.Wait()
		}()
	}
}

// Relay connects to the primary's socket and copies in to it and its
// replies to out, until either side closes. The primary may still be
// connecting to Telegram, so dialing is retried for up to wait.
func Relay(ctx context.Context, path string, in io.Reader, out io.Writer, wait time.Duration) error {
	conn, err := dial(ctx, path, wait)
	if err != nil {
		return err
	}
	defer conn.Close()

	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, in)
		// The client is gone: tell the primary, which ends the session.
		_ = conn.(*net.UnixConn).CloseWrite()
		done <- err
	}()
	go func() {
		_, err := io.Copy(out, conn)
		done <- err
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		err = nil
	}
	return err
}

func dial(ctx context.Context, path string, wait time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(wait)
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		// Not listening yet (or a stale file): retry. Anything else is real.
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("connect to running server: %w", err)
		}
		if time.Now().After(deadline) {
			if vs := otherVersions(path); len(vs) > 0 {
				return nil, fmt.Errorf("a different telegram-mcp version (%s) is serving this profile; "+
					"restart the MCP client (e.g. quit and reopen Claude Desktop) so all its processes run the same version",
					strings.Join(vs, ", "))
			}
			return nil, fmt.Errorf("another telegram-mcp process holds this profile but serves no socket at %s "+
				"(is it running for a different --account, or not as `serve`?)", path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
