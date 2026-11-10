// Command sshub attaches every USB device a remote machine shares, over
// SSH and USB/IP, and keeps them attached until interrupted.
//
//	sshub [-e rsh] <host>
//
// The remote runs sshub-server, normally as an SSH forced command.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/svaante/sshub/internal/client"
)

const usage = `usage: sshub [-e rsh] <host>

Attaches everything <host> shares, including devices plugged in later;
Ctrl-C detaches all. -e (or $SSHUB_RSH) sets the transport program,
default "ssh".`

func main() {
	flag.Usage = func() { fmt.Fprintln(flag.CommandLine.Output(), usage) }
	rsh := flag.String("e", "", "transport `program` and arguments (default $SSHUB_RSH, then ssh)")
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	t := client.Transport{RSH: client.RSH(*rsh, os.Getenv("SSHUB_RSH")), Host: flag.Arg(0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := client.Attach(ctx, t, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "sshub: %v\n", err)
		os.Exit(1)
	}
}
