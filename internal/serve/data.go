// Package serve implements the server side of a sshub session, spoken
// over stdin/stdout: the USB/IP operation phase, then USB/IP traffic.
package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"

	"github.com/svaante/sshub/internal/share"
	"github.com/svaante/sshub/internal/sysfs"
	"github.com/svaante/sshub/internal/usbip"
	"golang.org/x/sys/unix"
)

// Exporter hands a device over to whatever serves USB/IP for it, and takes
// it back.
type Exporter interface {
	// Export makes busid reachable over sock, a connected stream socket.
	// Export must not keep sock after returning; the caller closes it.
	Export(ctx context.Context, busid string, sock *os.File) error
	// Release undoes Export. It must be safe to call after a failed or
	// partial Export.
	Release(ctx context.Context, busid string) error
}

// Data serves data mode: the USB/IP operation phase, then, after a
// successful import, raw USB/IP traffic between the client and the
// exported device.
//
// Only shared devices are listed or imported; the zero value shares
// nothing.
type Data struct {
	Sys      string // sysfs root, "/sys" in production
	Exporter Exporter
	Shares   share.List
}

// Serve handles one session: a single devlist or import request read from r,
// answered on w. After an import it relays traffic until either side closes
// or ctx is done, then releases the device.
//
// Serve may return while a read from r is still pending; the caller should
// close r or exit afterwards.
func (d *Data) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	h, err := usbip.ReadHeader(r)
	if err != nil {
		return err
	}
	switch h.Code {
	case usbip.OpReqDevlist:
		return d.devlist(w)
	case usbip.OpReqImport:
		busid, err := usbip.ReadImportRequest(r)
		if err != nil {
			return err
		}
		return d.importDevice(ctx, busid, r, w)
	}
	return fmt.Errorf("unsupported usbip op %#04x", h.Code)
}

// devlist answers with the devices that could be imported right now.
func (d *Data) devlist(w io.Writer) error {
	devs, err := sysfs.List(d.Sys)
	if err != nil {
		return err
	}
	var list []usbip.Device
	for _, dev := range devs {
		if !d.Shares.Shared(dev) {
			continue
		}
		st, err := sysfs.ReadStatus(d.Sys, dev.BusID)
		if err != nil {
			return err
		}
		// Like stock usbipd, only list what an import could get.
		if st == sysfs.StatusUnbound || st == sysfs.StatusAvailable {
			list = append(list, dev.Device)
		}
	}
	return usbip.WriteDevlistReply(w, list)
}

func (d *Data) importDevice(ctx context.Context, busid string, r io.Reader, w io.Writer) (err error) {
	dev, err := sysfs.Read(d.Sys, busid)
	if errors.Is(err, fs.ErrNotExist) {
		return reject(w, usbip.StatusNoDev, fmt.Errorf("import %s: no such device", busid))
	}
	if err != nil {
		return reject(w, usbip.StatusError, fmt.Errorf("import %s: %w", busid, err))
	}
	if !d.Shares.Shared(dev) {
		// Exactly the answer for a missing device, error text included
		// (stderr reaches the client): don't reveal what else is plugged
		// in.
		return reject(w, usbip.StatusNoDev, fmt.Errorf("import %s: no such device", busid))
	}
	st, err := sysfs.ReadStatus(d.Sys, busid)
	if err != nil {
		return reject(w, usbip.StatusError, fmt.Errorf("import %s: %w", busid, err))
	}
	switch st {
	case sysfs.StatusUsed:
		return reject(w, usbip.StatusDevBusy, fmt.Errorf("import %s: device in use", busid))
	case sysfs.StatusError:
		return reject(w, usbip.StatusDevErr, fmt.Errorf("import %s: device in error state", busid))
	}

	conn, kernelEnd, err := socketPair()
	if err != nil {
		return reject(w, usbip.StatusError, fmt.Errorf("import %s: %w", busid, err))
	}
	defer conn.Close()
	// Release must run even when ctx was cancelled by a signal.
	defer func() {
		err = errors.Join(err, d.Exporter.Release(context.WithoutCancel(ctx), busid))
	}()
	err = d.Exporter.Export(ctx, busid, kernelEnd)
	kernelEnd.Close()
	if err != nil {
		return reject(w, usbip.StatusNA, err)
	}
	if err := usbip.WriteImportReply(w, usbip.StatusOK, &dev.Device); err != nil {
		return err
	}
	return relay(ctx, conn, r, w)
}

// reject sends a failed import reply and returns err.
func reject(w io.Writer, st usbip.Status, err error) error {
	if werr := usbip.WriteImportReply(w, st, nil); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

// relay copies between the client (r, w) and the exported device's socket
// until one side is done or ctx is cancelled.
func relay(ctx context.Context, conn *net.UnixConn, r io.Reader, w io.Writer) error {
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, r)
		// Client finished: let the device side see EOF.
		conn.CloseWrite()
		done <- err
	}()
	go func() {
		_, err := io.Copy(w, conn)
		done <- err
	}()
	select {
	case err := <-done:
		// Closing conn ends the other copy, unless it is blocked
		// reading r; see Serve.
		conn.Close()
		return err
	case <-ctx.Done():
		conn.Close()
		return ctx.Err()
	}
}

// socketPair returns a connected pair of stream sockets: one end as a
// net.UnixConn to relay through, the other as a file to hand to the kernel.
func socketPair() (*net.UnixConn, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	// Keep both ends out of unrelated child processes; exec.Cmd clears
	// the flag on the end it passes as stdin.
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	f := os.NewFile(uintptr(fds[0]), "usbip")
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		unix.Close(fds[1])
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	return c.(*net.UnixConn), os.NewFile(uintptr(fds[1]), "usbip-kernel"), nil
}
