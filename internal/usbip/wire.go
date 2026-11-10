// Package usbip implements the USB/IP operation phase (OP_REQ_DEVLIST and
// OP_REQ_IMPORT; the client side only lists devices), as laid out in the kernel's
// tools/usb/usbip/src/usbip_network.h.
//
// Reads are exact and unbuffered, so after an import request the reader is
// positioned at the first URB byte and nothing is lost when the connection is
// handed over to the kernel.
package usbip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the USB/IP protocol version spoken by this package.
const Version = 0x0111

// Operation codes.
const (
	OpReqDevlist = 0x8005
	OpRepDevlist = 0x0005
	OpReqImport  = 0x8003
	OpRepImport  = 0x0003
)

// Status is the status field of an operation reply.
type Status uint32

// Reply statuses, from usbip_network.h.
const (
	StatusOK      Status = 0 // request completed successfully
	StatusNA      Status = 1 // request failed
	StatusDevBusy Status = 2 // device busy (exported)
	StatusDevErr  Status = 3 // device in error state
	StatusNoDev   Status = 4 // device not found
	StatusError   Status = 5 // unexpected response
)

// Speed is a USB device speed as encoded on the wire (enum usb_device_speed).
type Speed uint32

// Device speeds.
const (
	SpeedUnknown Speed = iota
	SpeedLow
	SpeedFull
	SpeedHigh
	SpeedWireless
	SpeedSuper
	SpeedSuperPlus
)

// ErrVersion is returned by ReadHeader for an unsupported protocol version.
var ErrVersion = errors.New("unsupported usbip version")

// Header is the common header of every operation request and reply.
type Header struct {
	Version uint16
	Code    uint16
	Status  Status
}

// Interface is the per-interface record that follows a device in a devlist
// reply.
type Interface struct {
	Class    uint8
	SubClass uint8
	Protocol uint8
}

// Device is a USB device record (struct usbip_usb_device).
type Device struct {
	Path        string // sysfs path, at most 255 bytes
	BusID       string // e.g. "1-2.3", at most 31 bytes
	BusNum      uint32
	DevNum      uint32
	Speed       Speed
	VendorID    uint16
	ProductID   uint16
	BCDDevice   uint16
	Class       uint8
	SubClass    uint8
	Protocol    uint8
	ConfigValue uint8
	NumConfigs  uint8
	Interfaces  []Interface
}

const (
	pathSize  = 256
	busIDSize = 32
)

type wireDevice struct {
	Path          [pathSize]byte
	BusID         [busIDSize]byte
	BusNum        uint32
	DevNum        uint32
	Speed         uint32
	VendorID      uint16
	ProductID     uint16
	BCDDevice     uint16
	Class         uint8
	SubClass      uint8
	Protocol      uint8
	ConfigValue   uint8
	NumConfigs    uint8
	NumInterfaces uint8
}

type wireInterface struct {
	Class    uint8
	SubClass uint8
	Protocol uint8
	_        uint8
}

// ReadHeader reads an operation header from r and checks its version.
func ReadHeader(r io.Reader) (Header, error) {
	var h Header
	if err := binary.Read(r, binary.BigEndian, &h); err != nil {
		return Header{}, fmt.Errorf("read op header: %w", err)
	}
	if h.Version != Version {
		return h, fmt.Errorf("%w: %#04x", ErrVersion, h.Version)
	}
	return h, nil
}

// ReadImportRequest reads the busid that follows an OP_REQ_IMPORT header.
func ReadImportRequest(r io.Reader) (string, error) {
	var b [busIDSize]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("read import request: %w", err)
	}
	return cString(b[:]), nil
}

// WriteDevlistRequest writes an OP_REQ_DEVLIST request.
func WriteDevlistRequest(w io.Writer) error {
	h := Header{Version: Version, Code: OpReqDevlist}
	if err := binary.Write(w, binary.BigEndian, h); err != nil {
		return fmt.Errorf("write devlist request: %w", err)
	}
	return nil
}

// maxDevlist bounds the device count read from a devlist reply, so a bad
// reply cannot make ReadDevlistReply allocate without limit.
const maxDevlist = 1024

// ReadDevlistReply reads an OP_REP_DEVLIST reply, as written by
// WriteDevlistReply.
func ReadDevlistReply(r io.Reader) ([]Device, error) {
	h, err := ReadHeader(r)
	if err != nil {
		return nil, err
	}
	if h.Code != OpRepDevlist {
		return nil, fmt.Errorf("read devlist reply: unexpected op %#04x", h.Code)
	}
	if h.Status != StatusOK {
		return nil, fmt.Errorf("read devlist reply: status %d", h.Status)
	}
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, fmt.Errorf("read devlist reply: %w", err)
	}
	if n > maxDevlist {
		return nil, fmt.Errorf("read devlist reply: %d devices", n)
	}
	devs := make([]Device, 0, n)
	for range n {
		var wd wireDevice
		if err := binary.Read(r, binary.BigEndian, &wd); err != nil {
			return nil, fmt.Errorf("read devlist reply: %w", err)
		}
		d := Device{
			Path:        cString(wd.Path[:]),
			BusID:       cString(wd.BusID[:]),
			BusNum:      wd.BusNum,
			DevNum:      wd.DevNum,
			Speed:       Speed(wd.Speed),
			VendorID:    wd.VendorID,
			ProductID:   wd.ProductID,
			BCDDevice:   wd.BCDDevice,
			Class:       wd.Class,
			SubClass:    wd.SubClass,
			Protocol:    wd.Protocol,
			ConfigValue: wd.ConfigValue,
			NumConfigs:  wd.NumConfigs,
		}
		for range wd.NumInterfaces {
			var wi wireInterface
			if err := binary.Read(r, binary.BigEndian, &wi); err != nil {
				return nil, fmt.Errorf("read devlist reply: %w", err)
			}
			d.Interfaces = append(d.Interfaces, Interface{Class: wi.Class, SubClass: wi.SubClass, Protocol: wi.Protocol})
		}
		devs = append(devs, d)
	}
	return devs, nil
}

// WriteDevlistReply writes an OP_REP_DEVLIST reply listing devs.
func WriteDevlistReply(w io.Writer, devs []Device) error {
	h := Header{Version: Version, Code: OpRepDevlist, Status: StatusOK}
	if err := binary.Write(w, binary.BigEndian, h); err != nil {
		return fmt.Errorf("write devlist reply: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(devs))); err != nil {
		return fmt.Errorf("write devlist reply: %w", err)
	}
	for _, d := range devs {
		if err := writeDevice(w, d); err != nil {
			return fmt.Errorf("write devlist reply: %w", err)
		}
		for _, in := range d.Interfaces {
			wi := wireInterface{Class: in.Class, SubClass: in.SubClass, Protocol: in.Protocol}
			if err := binary.Write(w, binary.BigEndian, wi); err != nil {
				return fmt.Errorf("write devlist reply: %w", err)
			}
		}
	}
	return nil
}

// WriteImportReply writes an OP_REP_IMPORT reply. The device record is only
// sent when st is StatusOK, in which case d must not be nil.
func WriteImportReply(w io.Writer, st Status, d *Device) error {
	h := Header{Version: Version, Code: OpRepImport, Status: st}
	if err := binary.Write(w, binary.BigEndian, h); err != nil {
		return fmt.Errorf("write import reply: %w", err)
	}
	if st != StatusOK {
		return nil
	}
	if d == nil {
		return errors.New("write import reply: missing device")
	}
	if err := writeDevice(w, *d); err != nil {
		return fmt.Errorf("write import reply: %w", err)
	}
	return nil
}

func writeDevice(w io.Writer, d Device) error {
	// Leave room for the NUL terminator the C side expects.
	if len(d.Path) >= pathSize {
		return fmt.Errorf("device %s: path too long", d.BusID)
	}
	if len(d.BusID) >= busIDSize {
		return fmt.Errorf("busid %q too long", d.BusID)
	}
	if len(d.Interfaces) > 255 {
		return fmt.Errorf("device %s: too many interfaces", d.BusID)
	}
	wd := wireDevice{
		BusNum:        d.BusNum,
		DevNum:        d.DevNum,
		Speed:         uint32(d.Speed),
		VendorID:      d.VendorID,
		ProductID:     d.ProductID,
		BCDDevice:     d.BCDDevice,
		Class:         d.Class,
		SubClass:      d.SubClass,
		Protocol:      d.Protocol,
		ConfigValue:   d.ConfigValue,
		NumConfigs:    d.NumConfigs,
		NumInterfaces: uint8(len(d.Interfaces)),
	}
	copy(wd.Path[:], d.Path)
	copy(wd.BusID[:], d.BusID)
	return binary.Write(w, binary.BigEndian, wd)
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
