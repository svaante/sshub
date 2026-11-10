package client

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/svaante/sshub/internal/usbip"
)

// List returns the devices the remote shares that are free to attach. It
// sends a USB/IP devlist request, as stock "usbip list -r" does; the server
// leaves out devices in use. The transport's stderr (e.g. ssh errors) goes
// to ours.
func List(ctx context.Context, t Transport) ([]usbip.Device, error) {
	cmd := t.Command(ctx)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start transport: %w", err)
	}
	err = usbip.WriteDevlistRequest(stdin)
	var devs []usbip.Device
	if err == nil {
		devs, err = usbip.ReadDevlistReply(stdout)
	}
	stdin.Close()
	if werr := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("list devices: %w", errors.Join(err, werr))
	}
	return devs, nil
}
