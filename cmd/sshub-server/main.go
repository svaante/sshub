// Command sshub-server is the server side of sshub: it speaks USB/IP
// on stdin and stdout, sharing the devices listed in /etc/sshub/devices.
// It is run as an SSH forced command, unprivileged; the one root step goes
// through sshub-helper under sudo. Arguments, and the command the client
// asked for, are ignored.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/svaante/sshub/internal/export"
	"github.com/svaante/sshub/internal/serve"
	"github.com/svaante/sshub/internal/share"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sshub-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	shares, err := share.Load(share.DefaultPath)
	if err != nil {
		return err
	}
	// The client going away must end the session, not kill it: without
	// this, writing to a closed stdout raises SIGPIPE and the device is
	// never released.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	d := &serve.Data{Sys: "/sys", Exporter: export.Kernel{}, Shares: shares}
	return d.Serve(ctx, os.Stdin, os.Stdout)
}
