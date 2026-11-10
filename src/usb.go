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

type usbURB struct {
	Type         uint8
	Endpoint     uint8
	Status       int32
	Flags        uint32
	Buffer       unsafe.Pointer
	BufferLength int32
	ActualLength int32
	StartFrame   int32
	NumPackets   int32
	ErrorCount   int32
	Signr        uint32
	UserContext  unsafe.Pointer
}

const (
	usbDisconnectClaimExceptDriver = 0x02
	usbURBTypeInterrupt            = 1

	urbBufOffset = 64 // the URB struct, rounded up
	urbBufSize   = 64 // one full-speed interrupt packet, the largest report
)

var (
	usbdevfsControl          = ioc(iocRead|iocWrite, 'U', 0, unsafe.Sizeof(usbCtrlTransfer{}))
	usbdevfsBulk             = ioc(iocRead|iocWrite, 'U', 2, unsafe.Sizeof(usbBulkTransfer{}))
	usbdevfsSubmitURB        = ioc(iocRead, 'U', 10, unsafe.Sizeof(usbURB{}))
	usbdevfsReapURBNDelay    = ioc(iocWrite, 'U', 13, unsafe.Sizeof(unsafe.Pointer(nil)))
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

// usbSlot is one controller channel: a HID interface detached from the kernel driver
// through its own usbfs handle. While detached the controller is invisible to Steam and
// every other application.
type usbSlot struct {
	f           *os.File
	rc          syscall.RawConn
	ifnum       int
	epIn, epOut uint8 // epOut is 0 when output reports must go through SET_REPORT
	claimed     bool
	mu          sync.Mutex // serialises OUT and control transfers
	mem         []byte     // the queued URB and its IN buffer, outside the Go heap
	queued      bool
}

func usbNode(c usbController) string {
	return fmt.Sprintf("/dev/bus/usb/%03d/%03d", readDec(c.dir, "busnum"), readDec(c.dir, "devnum"))
}

func attachUSB(c usbController, padsChanged func(int32)) (*attachment, error) {
	ifaces := controllerInterfaces(c)
	if len(ifaces) == 0 {
		return nil, errors.New("no controller interface found")
	}
	var slots []*usbSlot
	closeAll := func() {
		for _, s := range slots {
			s.close()
		}
	}
	for _, it := range ifaces {
		s, err := openSlot(usbNode(c), it)
		if err != nil {
			closeAll()
			return nil, err
		}
		slots = append(slots, s)
	}
	links := make([]link, len(slots))
	for i, s := range slots {
		links[i] = s
	}
	return startSession(c.name, links, c.pid != pidWired, padsChanged, closeAll), nil
}

func openSlot(node string, it usbIface) (*usbSlot, error) {
	f, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", node, err)
	}
	s := &usbSlot{f: f, ifnum: it.num, epIn: it.epIn, epOut: it.epOut}
	if s.rc, err = f.SyscallConn(); err == nil {
		s.mem, err = syscall.Mmap(-1, 0, urbBufOffset+urbBufSize,
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	}
	if err == nil {
		err = s.claim()
	}
	if err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *usbSlot) claim() error {
	dc := usbDisconnectClaim{Interface: uint32(s.ifnum), Flags: usbDisconnectClaimExceptDriver}
	copy(dc.Driver[:], "usbfs") // never take an interface another program has claimed
	if _, err := rawIoctlPtr(s.rc, usbdevfsDisconnectClaim, unsafe.Pointer(&dc)); err != nil {
		return fmt.Errorf("claim USB interface %d: %w", s.ifnum, err)
	}
	s.claimed = true
	return nil
}

// close releases the interface and lets the kernel driver bind again, which makes the
// controller reappear to Steam as a regular Steam Controller.
func (s *usbSlot) close() {
	if s.claimed {
		// Best effort: these fail harmlessly when the controller has been unplugged.
		n := uint32(s.ifnum)
		_, _ = rawIoctlPtr(s.rc, usbdevfsReleaseInterface, unsafe.Pointer(&n))
		_ = reconnectDriver(s.rc, s.ifnum)
	}
	_ = s.f.Close() // also discards the queued URB
	if s.mem != nil {
		_ = syscall.Munmap(s.mem)
	}
}

func reconnectDriver(rc syscall.RawConn, ifnum int) error {
	req := usbIoctl{Ifno: int32(ifnum), IoctlCode: int32(usbdevfsConnect)}
	_, err := rawIoctlPtr(rc, usbdevfsIoctl, unsafe.Pointer(&req))
	return err
}

// transfer performs one synchronous interrupt transfer (usbfs runs USBDEVFS_BULK as an
// interrupt transfer on interrupt endpoints).
func (s *usbSlot) transfer(ep uint8, buf []byte, timeout time.Duration) (int, error) {
	t := usbBulkTransfer{Ep: uint32(ep), Len: uint32(len(buf)), Timeout: uint32(timeout.Milliseconds()), Data: unsafe.Pointer(&buf[0])}
	n, err := rawIoctlPtr(s.rc, usbdevfsBulk, unsafe.Pointer(&t))
	runtime.KeepAlive(buf)
	return n, err
}

func (s *usbSlot) control(reqType, req uint8, value uint16, buf []byte) (int, error) {
	t := usbCtrlTransfer{RequestType: reqType, Request: req, Value: value, Index: uint16(s.ifnum),
		Length: uint16(len(buf)), Timeout: usbControlTimeout, Data: unsafe.Pointer(&buf[0])}
	n, err := rawIoctlPtr(s.rc, usbdevfsControl, unsafe.Pointer(&t))
	runtime.KeepAlive(buf)
	return n, err
}

func (s *usbSlot) urb() *usbURB { return (*usbURB)(unsafe.Pointer(&s.mem[0])) }

func (s *usbSlot) queue() error {
	*s.urb() = usbURB{Type: usbURBTypeInterrupt, Endpoint: s.epIn,
		Buffer: unsafe.Pointer(&s.mem[urbBufOffset]), BufferLength: urbBufSize}
	if _, err := rawIoctlNoBlock(s.rc, usbdevfsSubmitURB, unsafe.Pointer(s.urb())); err != nil {
		return err
	}
	s.queued = true
	return nil
}

// read returns the next input report. It keeps an interrupt URB queued and waits for it in
// the Go poller (usbfs reports completed URBs as writable), so no thread sits blocked in
// the kernel between reports.
func (s *usbSlot) read(buf []byte, timeout time.Duration) (int, error) {
	if !s.queued {
		if err := s.queue(); err != nil {
			return 0, err
		}
	}
	if err := s.f.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	var reapErr error
	err := s.rc.Write(func(fd uintptr) bool {
		var done unsafe.Pointer
		_, reapErr = ioctlNoBlock(fd, usbdevfsReapURBNDelay, unsafe.Pointer(&done))
		return !errors.Is(reapErr, syscall.EAGAIN)
	})
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, errTimeout
	}
	if err == nil {
		err = reapErr
	}
	if err != nil {
		return 0, err
	}
	s.queued = false
	u := s.urb()
	n := copy(buf, s.mem[urbBufOffset:urbBufOffset+int(u.ActualLength)])
	switch status := syscall.Errno(-u.Status); status {
	case 0:
		return n, s.queue() // queue the next report before this one is processed
	case syscall.EPROTO, syscall.EILSEQ, syscall.ETIME, syscall.EOVERFLOW:
		// Transient bus errors (usbhid retries these too). An unplugged device
		// fails the next URB with ENODEV, which ends the session.
		time.Sleep(10 * time.Millisecond)
		return 0, errTimeout
	default:
		return 0, status
	}
}

func (s *usbSlot) writeOutput(report []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.epOut != 0 {
		_, err = s.transfer(s.epOut, report, 100*time.Millisecond)
	} else {
		_, err = s.control(usbReqTypeHIDOut, hidSetReport, hidReportOutput|uint16(report[0]), report)
	}
	return err
}

func (s *usbSlot) setFeature(report []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for range 5 {
		// Feature reports occasionally stall (EPIPE); Valve's drivers just retry.
		_, err = s.control(usbReqTypeHIDOut, hidSetReport, hidReportFeature|uint16(report[0]), report)
		if !errors.Is(err, syscall.EPIPE) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

func (s *usbSlot) getFeature(buf []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.control(usbReqTypeHIDIn, hidGetReport, hidReportFeature|uint16(buf[0]), buf)
}

// reattachOrphanedInterfaces hands controller interfaces that were left without a driver
// (e.g. if this program was killed while in XInput mode) back to the kernel.
func reattachOrphanedInterfaces() {
	for _, c := range listUSBControllers() {
		var f *os.File
		for _, it := range controllerInterfaces(c) {
			if _, err := os.Lstat(filepath.Join(it.dir, "driver")); err == nil {
				continue
			}
			if f == nil {
				var err error
				if f, err = os.OpenFile(usbNode(c), os.O_RDWR, 0); err != nil {
					break
				}
			}
			if rc, err := f.SyscallConn(); err == nil {
				_ = reconnectDriver(rc, it.num)
			}
		}
		if f != nil {
			_ = f.Close()
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
