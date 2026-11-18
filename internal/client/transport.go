// Package client implements the sshub client: reaching a server over a
// transport command, listing its devices and attaching them locally.
package client

import (
	"context"
	"os/exec"
	"strings"
)

// Transport is how the client reaches "sshub-server" on a remote: a
// remote-shell program (like rsync -e) and the host to pass it.
type Transport struct {
	RSH  []string // program and arguments, e.g. ["ssh", "-p", "2222"]
	Host string
}

// RSH picks the remote-shell command: flag (-e) if set, else env
// ($SSHUB_RSH), else ssh with keepalives. It is split on whitespace, with no
// shell quoting or expansion.
func RSH(flag, env string) []string {
	for _, s := range []string{flag, env} {
		if f := strings.Fields(s); len(f) > 0 {
			return f
		}
	}
	return defaultRSH()
}

// defaultRSH is ssh, made to exit once the server has not answered for about
// 15 s. Without that, a server that reboots or drops off the network leaves
// ssh, and with it the vhci port, waiting until TCP gives up, and vhci
// enumerates no device while it waits for the answer to a cancelled request.
func defaultRSH() []string {
	return []string{"ssh", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3"}
}

// Command returns the command that runs "sshub-server" on the remote,
// talking over its stdin and stdout.
func (t Transport) Command(ctx context.Context) *exec.Cmd {
	rsh := t.RSH
	if len(rsh) == 0 {
		rsh = defaultRSH()
	}
	args := append(rsh[1:len(rsh):len(rsh)], t.Host, "sshub-server")
	cmd := exec.CommandContext(ctx, rsh[0], args...)
	dieWithParent(cmd)
	return cmd
}
