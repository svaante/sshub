package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/svaante/sshub/internal/export"
	"github.com/svaante/sshub/internal/usbip"
)

// errNoPort means usbip port does not list the device.
var errNoPort = errors.New("not attached")

// errLost means an attached device went away: unplugged, reset (which
// re-enumerates it on the server), or the connection to the remote dropped.
var errLost = errors.New("connection to remote lost")

// pollInterval is how often Attach asks the remote for devices to attach:
// new ones, and ones back after an unplug or reset.
const pollInterval = 2 * time.Second

// Attach attaches every device the remote reached over t shares, and keeps
// at it until ctx is done: devices that appear later are attached too, and
// one that is lost is attached again once it is back. When ctx is done
// every device is detached. Only failing to reach the remote at all is an
// error. Progress messages go to log.
func Attach(ctx context.Context, t Transport, log io.Writer) error {
	devs, err := List(ctx, t)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "attaching everything %s shares; Ctrl-C to detach\n", t.Host)
	if len(devs) == 0 {
		fmt.Fprintln(log, "nothing to attach yet; waiting")
	}

	type result struct {
		dev usbip.Device
		err error
	}
	done := make(chan result)
	attached := map[string]bool{} // by remote busid
	failed := map[string]string{} // last error per busid, reported once
	var listErr string
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		for _, d := range devs {
			if attached[d.BusID] {
				continue
			}
			attached[d.BusID] = true
			go func() { done <- result{d, attachDevice(ctx, t, d, log)} }()
		}
		devs = nil

		select {
		case <-ctx.Done():
			for len(attached) > 0 {
				r := <-done
				delete(attached, r.dev.BusID)
			}
			return nil
		case r := <-done:
			delete(attached, r.dev.BusID)
			switch {
			case r.err == nil:
			case errors.Is(r.err, errLost):
				delete(failed, r.dev.BusID)
				fmt.Fprintf(log, "lost %s: %v; attaching it again when it is back\n", label(r.dev), r.err)
			case failed[r.dev.BusID] != r.err.Error():
				failed[r.dev.BusID] = r.err.Error()
				fmt.Fprintf(log, "%v\n", r.err)
			}
			continue
		case <-tick.C:
		}

		devs, err = List(ctx, t)
		if ctx.Err() != nil {
			continue // shut down at the top of the loop
		}
		if err != nil && err.Error() != listErr {
			fmt.Fprintf(log, "%v; retrying\n", err)
		}
		listErr = ""
		if err != nil {
			listErr = err.Error()
		}
	}
}

// label names a remote device in messages.
func label(d usbip.Device) string {
	return fmt.Sprintf("%04x:%04x (remote busid %s)", d.VendorID, d.ProductID, d.BusID)
}

// attachDevice attaches dev and keeps it attached until ctx is done (nil)
// or the device is lost (errLost).
func attachDevice(ctx context.Context, t Transport, dev usbip.Device, log io.Writer) error {
	b, err := ListenBridge()
	if err != nil {
		return err
	}
	defer b.Close()
	// The bridge outlives ctx: on Ctrl-C we detach first and let the
	// transport wind down, so the server releases the device cleanly.
	bctx, stopBridge := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBridge()
	bridged := make(chan struct{}) // closed when the bridge is done
	var bridgeErr error
	go func() {
		bridgeErr = b.Serve(bctx, t)
		close(bridged)
	}()
	// Wait for the transport to exit before returning, however we leave.
	defer func() {
		select {
		case <-bridged:
		case <-time.After(10 * time.Second):
			stopBridge()
			<-bridged
		}
	}()

	tcpPort := strconv.Itoa(b.Port())
	if _, err := runUsbip(ctx, "attach", tcpPort, dev.BusID); err != nil {
		stopBridge()
		return fmt.Errorf("attach %s: %w", label(dev), err)
	}
	port, err := waitPort(ctx, b.Port(), dev.BusID)
	if err != nil {
		// Dropping the connection makes vhci detach the device.
		stopBridge()
		return fmt.Errorf("attach %s: %w", label(dev), err)
	}
	fmt.Fprintf(log, "attached %s on vhci port %d\n", label(dev), port)

	select {
	case <-ctx.Done():
	case <-bridged:
		// vhci has already dropped the device. Ctrl-C in a terminal also
		// reaches the transport and may end it just before ctx is
		// cancelled: that is a detach, not a loss.
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
			if bridgeErr != nil {
				return fmt.Errorf("%w: %w", errLost, bridgeErr)
			}
			return errLost
		}
	}
	if err := detach(context.WithoutCancel(ctx), b.Port(), dev.BusID, port); err != nil {
		return err
	}
	fmt.Fprintf(log, "detached %s\n", label(dev))
	return nil
}

// detach detaches the vhci port. Ctrl-C in a terminal also reaches the
// transport, whose exit makes vhci drop the device by itself; that counts
// as success.
func detach(ctx context.Context, tcpPort int, busid string, port int) error {
	_, err := runUsbip(ctx, "detach", strconv.Itoa(port))
	if err == nil {
		return nil
	}
	if _, perr := vhciPort(ctx, tcpPort, busid); errors.Is(perr, errNoPort) {
		return nil
	}
	return fmt.Errorf("detach port %d: %w", port, err)
}

func vhciPort(ctx context.Context, tcpPort int, busid string) (int, error) {
	out, err := runUsbip(ctx, "port")
	if err != nil {
		return 0, err
	}
	return ParsePort(out, tcpPort, busid)
}

// waitPort waits for busid to show up in "usbip port". It is only listed
// once the local USB core has enumerated it, about a second after usbip
// attach returns; until then "usbip port" may also fail outright.
func waitPort(ctx context.Context, tcpPort int, busid string) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		port, err := vhciPort(ctx, tcpPort, busid)
		if err == nil || time.Now().After(deadline) {
			return port, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

var portLine = regexp.MustCompile(`^Port (\d+):`)

// ParsePort finds, in the output of "usbip port", the vhci port on which
// busid was attached through 127.0.0.1:tcpPort.
func ParsePort(out []byte, tcpPort int, busid string) (int, error) {
	want := fmt.Sprintf("-> usbip://127.0.0.1:%d/%s", tcpPort, busid)
	port := -1
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := portLine.FindStringSubmatch(line); m != nil {
			port, _ = strconv.Atoi(m[1])
			continue
		}
		if port >= 0 && strings.HasSuffix(line, want) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("usbip port: %s via port %d: %w", busid, tcpPort, errNoPort)
}

// runUsbip runs the usbip tool as root, through sshub-helper: sudo-rs
// rejects wildcard arguments in sudoers, so the rule names only the helper,
// which checks the arguments itself. -n: fail instead of prompting.
func runUsbip(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", export.DefaultHelper}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("usbip %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("usbip %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
