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
// ($SSHUB_RSH), else ssh. It is split on whitespace, with no shell
// quoting or expansion.
func RSH(flag, env string) []string {
	for _, s := range []string{flag, env} {
		if f := strings.Fields(s); len(f) > 0 {
			return f
		}
	}
	return []string{"ssh"}
}

// Command returns the command that runs "sshub-server" on the remote,
// talking over its stdin and stdout.
func (t Transport) Command(ctx context.Context) *exec.Cmd {
	rsh := t.RSH
	if len(rsh) == 0 {
		rsh = []string{"ssh"}
	}
	args := append(rsh[1:len(rsh):len(rsh)], t.Host, "sshub-server")
	return exec.CommandContext(ctx, rsh[0], args...)
}
