package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const sysUSBDevices = "/sys/bus/usb/devices"

// usbfs ioctl structures (linux/usbdevice_fs.h, 64-bit layout).
type usbCtrlTransfer struct {
	RequestType uint8
	Request     uint8
	Value       uint16
	Index       uint16
	Length      uint16
	Timeout     uint32 // ms
	Data        unsafe.Pointer
}

type usbBulkTransfer struct {
	Ep      uint32
	Len     uint32
	Timeout uint32 // ms
	Data    unsafe.Pointer
}

type usbIoctl struct {
	Ifno      int32
	IoctlCode int32
	Data      unsafe.Pointer
}

type usbDisconnectClaim struct {
	Interface uint32
	Flags     uint32
	Driver    [256]byte
}

const usbDisconnectClaimExceptDriver = 0x02

var (
	usbdevfsControl          = ioc(iocRead|iocWrite, 'U', 0, unsafe.Sizeof(usbCtrlTransfer{}))
	usbdevfsBulk             = ioc(iocRead|iocWrite, 'U', 2, unsafe.Sizeof(usbBulkTransfer{}))
	usbdevfsReleaseInterface = ioc(iocRead, 'U', 16, 4)
	usbdevfsIoctl            = ioc(iocRead|iocWrite, 'U', 18, unsafe.Sizeof(usbIoctl{}))
	usbdevfsConnect          = ioc(iocNone, 'U', 23, 0)
	usbdevfsDisconnectClaim  = ioc(iocRead, 'U', 27, unsafe.Sizeof(usbDisconnectClaim{}))
)

// HID class requests.
const (
	hidGetReport      = 0x01
	hidSetReport      = 0x09
	hidReportOutput   = 0x0200
	hidReportFeature  = 0x0300
	usbReqTypeHIDIn   = 0xa1
	usbReqTypeHIDOut  = 0x21
	usbControlTimeout = 1000 // ms
)

type usbController struct {
	dir  string // sysfs device directory, e.g. /sys/bus/usb/devices/1-2.4
	pid  uint16
	name string
}

type usbIface struct {
	dir         string
	num         int
	epIn, epOut uint8
}

// listUSBControllers finds wired Steam Controllers and Steam Controller wireless receivers.
func listUSBControllers() []usbController {
	entries, err := os.ReadDir(sysUSBDevices)
	if err != nil {
		return nil
	}
	var out []usbController
	for _, e := range entries {
		if strings.Contains(e.Name(), ":") {
			continue // interface, not a device
		}
		dir := filepath.Join(sysUSBDevices, e.Name())
		if readHex(dir, "idVendor") != valveVendor {
			continue
		}
		pid := uint16(readHex(dir, "idProduct"))
		var kind string
		switch pid {
		case pidWired:
			kind = "USB cable"
		case pidPuck, pidNereid:
			kind = "wireless receiver"
		default:
			continue
		}
		out = append(out, usbController{dir: dir, pid: pid,
			name: fmt.Sprintf("Steam Controller via %s (USB %s)", kind, e.Name())})
	}
	return out
}

func findUSBControllers() []candidate {
	var out []candidate
	for _, c := range listUSBControllers() {
		out = append(out, candidate{key: c.name, attach: func(padsChanged func(int32)) (*attachment, error) {
			return attachUSB(c, padsChanged)
		}})
	}
	return out
}

// controllerInterfaces lists the HID interfaces that carry controller data: the single
// interface of a wired controller, or the four controller slots (2-5) of a receiver.
func controllerInterfaces(c usbController) []usbIface {
	dirs, _ := filepath.Glob(c.dir + ":*")
	var out []usbIface
	for _, dir := range dirs {
		num := int(readHex(dir, "bInterfaceNumber"))
		if readHex(dir, "bInterfaceClass") != 3 || (c.pid != pidWired && (num < 2 || num > 5)) {
			continue
		}
		it := usbIface{dir: dir, num: num}
		eps, _ := filepath.Glob(filepath.Join(dir, "ep_*"))
		for _, ep := range eps {
			if readString(ep, "type") != "Interrupt" {
				continue
			}
			if addr := uint8(readHex(ep, "bEndpointAddress")); addr&0x80 != 0 {
				it.epIn = addr
			} else {
				it.epOut = addr
			}
		}
		if it.epIn != 0 {
			out = append(out, it)
		}
	}
	return out
}

// usbDevice is a controller whose interfaces were detached from the kernel driver. While
// detached the controller is invisible to Steam and every other application.
type usbDevice struct {
	fd      int
	mu      sync.Mutex // serialises OUT and control transfers
	claimed []int
}

func openUSBDevice(c usbController) (*usbDevice, error) {
	node := fmt.Sprintf("/dev/bus/usb/%03d/%03d", readDec(c.dir, "busnum"), readDec(c.dir, "devnum"))
	fd, err := syscall.Open(node, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", node, err)
	}
	return &usbDevice{fd: fd}, nil
}

func attachUSB(c usbController, padsChanged func(int32)) (*attachment, error) {
	ifaces := controllerInterfaces(c)
	if len(ifaces) == 0 {
		return nil, errors.New("no controller interface found")
	}
	dev, err := openUSBDevice(c)
	if err != nil {
		return nil, err
	}
	var links []link
	for _, it := range ifaces {
		if err := dev.claim(it.num); err != nil {
			dev.close()
			return nil, err
		}
		links = append(links, &usbSlot{dev: dev, ifnum: it.num, epIn: it.epIn, epOut: it.epOut})
	}
	return startSession(c.name, links, c.pid != pidWired, padsChanged, dev.close), nil
}

func (d *usbDevice) claim(ifnum int) error {
	dc := usbDisconnectClaim{Interface: uint32(ifnum), Flags: usbDisconnectClaimExceptDriver}
	copy(dc.Driver[:], "usbfs") // never take an interface another program has claimed
	if _, err := ioctlPtr(uintptr(d.fd), usbdevfsDisconnectClaim, unsafe.Pointer(&dc)); err != nil {
		return fmt.Errorf("claim USB interface %d: %w", ifnum, err)
	}
	d.claimed = append(d.claimed, ifnum)
	return nil
}

// close releases the interfaces and lets the kernel driver bind again, which makes the
// controller reappear to Steam as a regular Steam Controller.
func (d *usbDevice) close() {
	// Best effort: these fail harmlessly when the controller has been unplugged.
	for _, ifnum := range d.claimed {
		n := uint32(ifnum)
		_, _ = ioctlPtr(uintptr(d.fd), usbdevfsReleaseInterface, unsafe.Pointer(&n))
		_ = d.reconnectDriver(ifnum)
	}
	_ = syscall.Close(d.fd)
}

func (d *usbDevice) reconnectDriver(ifnum int) error {
	req := usbIoctl{Ifno: int32(ifnum), IoctlCode: int32(usbdevfsConnect)}
	_, err := ioctlPtr(uintptr(d.fd), usbdevfsIoctl, unsafe.Pointer(&req))
	return err
}

// interrupt performs one interrupt transfer (usbfs runs USBDEVFS_BULK as an interrupt
// transfer on interrupt endpoints).
func (d *usbDevice) interrupt(ep uint8, buf []byte, timeout time.Duration) (int, error) {
	t := usbBulkTransfer{Ep: uint32(ep), Len: uint32(len(buf)), Timeout: uint32(timeout.Milliseconds()), Data: unsafe.Pointer(&buf[0])}
	n, err := ioctlPtr(uintptr(d.fd), usbdevfsBulk, unsafe.Pointer(&t))
	runtime.KeepAlive(buf)
	return n, err
}

func (d *usbDevice) control(reqType, req uint8, value, index uint16, buf []byte) (int, error) {
	t := usbCtrlTransfer{RequestType: reqType, Request: req, Value: value, Index: index,
		Length: uint16(len(buf)), Timeout: usbControlTimeout, Data: unsafe.Pointer(&buf[0])}
	n, err := ioctlPtr(uintptr(d.fd), usbdevfsControl, unsafe.Pointer(&t))
	runtime.KeepAlive(buf)
	return n, err
}

// usbSlot is one controller channel of a usbDevice (one HID interface).
type usbSlot struct {
	dev         *usbDevice
	ifnum       int
	epIn, epOut uint8 // epOut is 0 when output reports must go through SET_REPORT
}

func (s *usbSlot) read(buf []byte, timeout time.Duration) (int, error) {
	n, err := s.dev.interrupt(s.epIn, buf, timeout)
	switch {
	case errors.Is(err, syscall.ETIMEDOUT):
		return 0, errTimeout
	case errors.Is(err, syscall.EPROTO), errors.Is(err, syscall.EILSEQ),
		errors.Is(err, syscall.ETIME), errors.Is(err, syscall.EOVERFLOW):
		// Transient bus errors (usbhid retries these too). An unplugged device
		// fails the next transfer with ENODEV, which ends the session.
		time.Sleep(10 * time.Millisecond)
		return 0, errTimeout
	}
	return n, err
}

func (s *usbSlot) writeOutput(report []byte) error {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	var err error
	if s.epOut != 0 {
		_, err = s.dev.interrupt(s.epOut, report, 100*time.Millisecond)
	} else {
		_, err = s.dev.control(usbReqTypeHIDOut, hidSetReport, hidReportOutput|uint16(report[0]), uint16(s.ifnum), report)
	}
	return err
}

func (s *usbSlot) setFeature(report []byte) error {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	var err error
	for range 5 {
		// Feature reports occasionally stall (EPIPE); Valve's drivers just retry.
		_, err = s.dev.control(usbReqTypeHIDOut, hidSetReport, hidReportFeature|uint16(report[0]), uint16(s.ifnum), report)
		if !errors.Is(err, syscall.EPIPE) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

func (s *usbSlot) getFeature(buf []byte) (int, error) {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	return s.dev.control(usbReqTypeHIDIn, hidGetReport, hidReportFeature|uint16(buf[0]), uint16(s.ifnum), buf)
}

// reattachOrphanedInterfaces hands controller interfaces that were left without a driver
// (e.g. if this program was killed while in XInput mode) back to the kernel.
func reattachOrphanedInterfaces() {
	for _, c := range listUSBControllers() {
		var dev *usbDevice
		for _, it := range controllerInterfaces(c) {
			if _, err := os.Lstat(filepath.Join(it.dir, "driver")); err == nil {
				continue
			}
			if dev == nil {
				var err error
				if dev, err = openUSBDevice(c); err != nil {
					break
				}
			}
			_ = dev.reconnectDriver(it.num)
		}
		if dev != nil {
			_ = syscall.Close(dev.fd)
		}
	}
}

func readString(dir, file string) string {
	b, _ := os.ReadFile(filepath.Join(dir, file))
	return strings.TrimSpace(string(b))
}

func readHex(dir, file string) uint64 {
	v, _ := strconv.ParseUint(readString(dir, file), 16, 32)
	return v
}

func readDec(dir, file string) uint64 {
	v, _ := strconv.ParseUint(readString(dir, file), 10, 32)
	return v
}
