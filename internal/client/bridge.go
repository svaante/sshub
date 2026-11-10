package client

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

// Bridge is the local TCP endpoint that usbip attach dials: its connection
// is handed to "sshub-server" on the remote.
//
// It accepts a single connection and then stops listening, to keep the
// window in which another local user could connect short.
type Bridge struct {
	ln *net.TCPListener
}

// ListenBridge listens on an ephemeral port on 127.0.0.1.
func ListenBridge() (*Bridge, error) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("bridge: %w", err)
	}
	return &Bridge{ln: ln}, nil
}

// Port returns the port the bridge listens on.
func (b *Bridge) Port() int {
	return b.ln.Addr().(*net.TCPAddr).Port
}

// Serve accepts one connection and runs the transport for "sshub-server"
// with the connection as its stdin and stdout, until the transport exits or
// ctx is done.
func (b *Bridge) Serve(ctx context.Context, t Transport) error {
	stop := context.AfterFunc(ctx, func() { b.ln.Close() })
	conn, err := b.ln.AcceptTCP()
	stop()
	b.ln.Close()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("bridge: %w", err)
	}
	// Give the transport the socket itself: no copying through us.
	f, err := conn.File()
	conn.Close()
	if err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	defer f.Close()

	cmd := t.Command(ctx)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, f, os.Stderr
	// On cancellation ask the transport to exit before killing it.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("transport: %w", err)
	}
	return ctx.Err()
}

// Close stops listening if Serve has not accepted a connection yet.
func (b *Bridge) Close() error {
	return b.ln.Close()
}
