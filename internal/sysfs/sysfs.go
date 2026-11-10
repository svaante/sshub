// Package sysfs enumerates USB devices from /sys, building the records that
// USB/IP sends on the wire. It mirrors the kernel's
// tools/usb/usbip/libsrc (read_usb_device, read_usb_interface).
//
// Every function takes the sysfs mount point as root ("/sys" in production)
// so tests can point it at a fake tree.
package sysfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/svaante/sshub/internal/usbip"
)

// Device is a USB device as seen in sysfs: its USB/IP record plus the
// descriptor strings used for policy matching and display.
type Device struct {
	usbip.Device
	Product string
	Serial  string
}

// Status is a device's usbip-host state, from its usbip_status attribute.
type Status int

// Device states. The numeric values of Available, Used and Error match the
// kernel's SDEV_ST_* constants.
const (
	StatusUnbound   Status = 0 // not bound to usbip-host
	StatusAvailable Status = 1 // bound to usbip-host, not exported
	StatusUsed      Status = 2 // exported to a client
	StatusError     Status = 3 // usbip-host error state
)

func (s Status) String() string {
	switch s {
	case StatusUnbound:
		return "unbound"
	case StatusAvailable:
		return "available"
	case StatusUsed:
		return "used"
	case StatusError:
		return "error"
	}
	return "status(" + strconv.Itoa(int(s)) + ")"
}

// List returns the USB devices under root, sorted by busid. Root hubs and
// interfaces are skipped, as are devices that disappear while being read.
func List(root string) ([]Device, error) {
	entries, err := os.ReadDir(devicesDir(root))
	if err != nil {
		return nil, fmt.Errorf("list usb devices: %w", err)
	}
	var devs []Device
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "usb") || strings.Contains(name, ":") {
			continue
		}
		d, err := Read(root, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue // unplugged mid-scan
		}
		if err != nil {
			return nil, err
		}
		devs = append(devs, d)
	}
	slices.SortFunc(devs, func(a, b Device) int { return strings.Compare(a.BusID, b.BusID) })
	return devs, nil
}

// Read returns the device with the given busid.
func Read(root, busid string) (Device, error) {
	dir, err := deviceDir(root, busid)
	if err != nil {
		return Device{}, err
	}
	path, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Device{}, fmt.Errorf("device %s: %w", busid, err)
	}
	r := attrReader{dir: dir}
	d := Device{
		Device: usbip.Device{
			Path:        sysPath(root, path),
			BusID:       busid,
			BusNum:      uint32(r.uint("busnum", 10, 32)),
			DevNum:      uint32(r.uint("devnum", 10, 32)),
			Speed:       speed(r.string("speed")),
			VendorID:    uint16(r.uint("idVendor", 16, 16)),
			ProductID:   uint16(r.uint("idProduct", 16, 16)),
			BCDDevice:   uint16(r.uint("bcdDevice", 16, 16)),
			Class:       uint8(r.uint("bDeviceClass", 16, 8)),
			SubClass:    uint8(r.uint("bDeviceSubClass", 16, 8)),
			Protocol:    uint8(r.uint("bDeviceProtocol", 16, 8)),
			ConfigValue: uint8(r.uint("bConfigurationValue", 10, 8)),
			NumConfigs:  uint8(r.uint("bNumConfigurations", 10, 8)),
		},
		Product: r.optional("product"),
		Serial:  r.optional("serial"),
	}
	n := int(r.uint("bNumInterfaces", 10, 8))
	for i := range n {
		ir := attrReader{dir: filepath.Join(dir, fmt.Sprintf("%s:%d.%d", busid, d.ConfigValue, i))}
		d.Interfaces = append(d.Interfaces, usbip.Interface{
			Class:    uint8(ir.uint("bInterfaceClass", 16, 8)),
			SubClass: uint8(ir.uint("bInterfaceSubClass", 16, 8)),
			Protocol: uint8(ir.uint("bInterfaceProtocol", 16, 8)),
		})
		if ir.err != nil {
			return Device{}, fmt.Errorf("device %s: %w", busid, ir.err)
		}
	}
	if r.err != nil {
		return Device{}, fmt.Errorf("device %s: %w", busid, r.err)
	}
	return d, nil
}

// ReadStatus returns the usbip-host state of the device with the given busid.
func ReadStatus(root, busid string) (Status, error) {
	dir, err := deviceDir(root, busid)
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "usbip_status"))
	if errors.Is(err, fs.ErrNotExist) {
		// The attribute only exists while bound to usbip-host; tell that
		// apart from the device itself being gone.
		if _, err := os.Stat(dir); err != nil {
			return 0, fmt.Errorf("device %s: %w", busid, err)
		}
		return StatusUnbound, nil
	}
	if err != nil {
		return 0, fmt.Errorf("device %s: %w", busid, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < int(StatusAvailable) || n > int(StatusError) {
		return 0, fmt.Errorf("device %s: bad usbip_status %q", busid, strings.TrimSpace(string(b)))
	}
	return Status(n), nil
}

func devicesDir(root string) string {
	return filepath.Join(root, "bus", "usb", "devices")
}

func deviceDir(root, busid string) (string, error) {
	if busid == "" || busid == "." || busid == ".." || strings.ContainsRune(busid, '/') {
		return "", fmt.Errorf("invalid busid %q", busid)
	}
	return filepath.Join(devicesDir(root), busid), nil
}

// sysPath reports path as it appears under /sys, whatever root is, so the
// wire record does not depend on where sysfs was read from.
func sysPath(root, path string) string {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		if rel, err := filepath.Rel(r, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("/sys", rel)
		}
	}
	return path
}

// speed maps the sysfs speed attribute (Mbit/s) to its wire value.
func speed(s string) usbip.Speed {
	switch s {
	case "1.5":
		return usbip.SpeedLow
	case "12":
		return usbip.SpeedFull
	case "480":
		return usbip.SpeedHigh
	case "53.3-480":
		return usbip.SpeedWireless
	case "5000":
		return usbip.SpeedSuper
	case "10000", "20000":
		return usbip.SpeedSuperPlus
	}
	return usbip.SpeedUnknown
}

// attrReader reads sysfs attributes of one directory, keeping the first
// error so a device can be read without checking every attribute.
type attrReader struct {
	dir string
	err error
}

func (r *attrReader) string(name string) string {
	if r.err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.err = err
		return ""
	}
	return strings.TrimSpace(string(b))
}

// optional reads a string attribute that may be absent, such as a serial
// number.
func (r *attrReader) optional(name string) string {
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// uint parses a numeric attribute. Empty values (bConfigurationValue and
// bNumInterfaces of an unconfigured device) read as 0.
func (r *attrReader) uint(name string, base, bits int) uint64 {
	s := r.string(name)
	if r.err != nil || s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, base, bits)
	if err != nil {
		r.err = fmt.Errorf("attribute %s: %w", name, err)
		return 0
	}
	return n
}
