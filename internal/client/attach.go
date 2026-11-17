package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/svaante/sshub/internal/export"
	"github.com/svaante/sshub/internal/usbip"
	"golang.org/x/sys/unix"
)

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
//
// The transport gets one end of a socket pair as stdin and stdout; over the
// other we import dev, then hand that end to the kernel through
// sshub-helper. Unlike a TCP endpoint for usbip attach, no other local
// user can reach it.
func attachDevice(ctx context.Context, t Transport, dev usbip.Device, log io.Writer) error {
	sock, remote, err := socketPair()
	if err != nil {
		return fmt.Errorf("attach %s: %w", label(dev), err)
	}
	defer sock.Close()
	// The transport outlives ctx: on Ctrl-C we detach first and let it
	// wind down, so the server releases the device cleanly.
	tctx, stopTransport := context.WithCancel(context.WithoutCancel(ctx))
	defer stopTransport()
	cmd := t.Command(tctx)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = remote, remote, os.Stderr
	// On cancellation ask the transport to exit before killing it.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Start()
	remote.Close()
	if err != nil {
		return fmt.Errorf("attach %s: start transport: %w", label(dev), err)
	}
	exited := make(chan struct{}) // closed when the transport has exited
	var transportErr error
	go func() {
		transportErr = cmd.Wait()
		close(exited)
	}()
	// Wait for the transport to exit before returning, however we leave.
	defer func() {
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			stopTransport()
			<-exited
		}
	}()

	// Until the kernel has the socket, Ctrl-C stops the transport, which
	// also ends a pending read on sock.
	stop := context.AfterFunc(ctx, stopTransport)
	port, err := importAndAttach(ctx, sock, dev.BusID)
	stop()
	// The kernel holds its own reference to the socket.
	sock.Close()
	if err != nil {
		stopTransport()
		return fmt.Errorf("attach %s: %w", label(dev), err)
	}
	fmt.Fprintf(log, "attached %s on vhci port %d\n", label(dev), port)

	select {
	case <-ctx.Done():
	case <-exited:
		// vhci has already dropped the device. Ctrl-C in a terminal also
		// reaches the transport and may end it just before ctx is
		// cancelled: that is a detach, not a loss.
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
			if transportErr != nil {
				return fmt.Errorf("%w: transport: %w", errLost, transportErr)
			}
			return errLost
		}
	}
	// The helper counts a port vhci has already freed as detached.
	if _, err := runHelper(context.WithoutCancel(ctx), nil, "detach", strconv.Itoa(port)); err != nil {
		return fmt.Errorf("detach %s: %w", label(dev), err)
	}
	fmt.Fprintf(log, "detached %s\n", label(dev))
	return nil
}

// importAndAttach imports busid from sshub-server over sock, then attaches
// it to a vhci port, which it returns.
func importAndAttach(ctx context.Context, sock *os.File, busid string) (int, error) {
	if err := usbip.WriteImportRequest(sock, busid); err != nil {
		return 0, err
	}
	d, err := usbip.ReadImportReply(sock)
	if err != nil {
		return 0, err
	}
	out, err := runHelper(ctx, sock, "attach",
		strconv.FormatUint(uint64(d.Speed), 10),
		strconv.FormatUint(uint64(d.BusNum), 10),
		strconv.FormatUint(uint64(d.DevNum), 10))
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("sshub-helper attach: unexpected output %q", out)
	}
	return port, nil
}

// socketPair returns a connected pair of stream sockets, both close-on-exec;
// exec.Cmd clears the flag on the ends it passes on.
func socketPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return os.NewFile(uintptr(fds[0]), "usbip"), os.NewFile(uintptr(fds[1]), "usbip-transport"), nil
}

// runHelper runs sshub-helper as root, with stdin as its stdin if not nil.
// -n: fail instead of prompting.
func runHelper(ctx context.Context, stdin *os.File, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", export.DefaultHelper}, args...)...)
	if stdin != nil {
		cmd.Stdin = stdin // an *os.File is passed as fd 0 as is, not copied
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("sshub-helper %s: %w: %s", args[0], err, msg)
		}
		return nil, fmt.Errorf("sshub-helper %s: %w", args[0], err)
	}
	return out, nil
}
