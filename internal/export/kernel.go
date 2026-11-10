// Package export provides the Linux exporter: the in-kernel usbip-host,
// driven through sshub-helper under sudo.
package export

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DefaultHelper is where packages install sshub-helper; the sudoers rule
// names this path.
const DefaultHelper = "/usr/libexec/sshub/sshub-helper"

// Kernel exports devices through usbip-host by running
// "sudo -n <DefaultHelper> export|release <busid>".
type Kernel struct{}

// Export binds busid to usbip-host and hands it sock, passed as the
// helper's stdin.
func (k Kernel) Export(ctx context.Context, busid string, sock *os.File) error {
	return k.run(ctx, sock, "export", busid)
}

// Release returns busid to its regular driver.
func (k Kernel) Release(ctx context.Context, busid string) error {
	return k.run(ctx, nil, "release", busid)
}

func (k Kernel) run(ctx context.Context, stdin *os.File, action, busid string) error {
	// -n: there is no terminal to prompt on; a missing sudoers rule must
	// fail instead of hanging.
	cmd := exec.CommandContext(ctx, "sudo", "-n", DefaultHelper, action, busid)
	if stdin != nil {
		cmd.Stdin = stdin // an *os.File is passed as fd 0 as is, not copied
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s %s: %w: %s", action, busid, err, msg)
		}
		return fmt.Errorf("%s %s: %w", action, busid, err)
	}
	return nil
}
