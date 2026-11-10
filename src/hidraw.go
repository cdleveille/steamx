package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	sysHidraw    = "/sys/class/hidraw"
	busBluetooth = 0x05
)

// findBluetoothControllers finds Steam Controllers connected over Bluetooth LE. They are
// driven through their hidraw node (the hid-steam "client" node on recent kernels).
func findBluetoothControllers() []candidate {
	entries, err := os.ReadDir(sysHidraw)
	if err != nil {
		return nil
	}
	var out []candidate
	for _, e := range entries {
		uevent, err := os.ReadFile(filepath.Join(sysHidraw, e.Name(), "device", "uevent"))
		if err != nil {
			continue
		}
		bus, vid, pid, ok := parseHIDID(string(uevent))
		if !ok || bus != busBluetooth || vid != valveVendor || pid != pidBLE {
			continue
		}
		node := "/dev/" + e.Name()
		name := fmt.Sprintf("Steam Controller via Bluetooth (%s)", e.Name())
		out = append(out, candidate{key: name, attach: func(padsChanged func(int32)) (*attachment, error) {
			return attachHidraw(node, name, padsChanged)
		}})
	}
	return out
}

// parseHIDID parses the "HID_ID=bus:vendor:product" line of a HID device's uevent file.
func parseHIDID(uevent string) (bus, vid, pid uint64, ok bool) {
	for _, line := range strings.Split(uevent, "\n") {
		v, found := strings.CutPrefix(line, "HID_ID=")
		if !found {
			continue
		}
		parts := strings.Split(v, ":")
		if len(parts) != 3 {
			return 0, 0, 0, false
		}
		var errs [3]error
		bus, errs[0] = strconv.ParseUint(parts[0], 16, 32)
		vid, errs[1] = strconv.ParseUint(parts[1], 16, 32)
		pid, errs[2] = strconv.ParseUint(parts[2], 16, 32)
		return bus, vid, pid, errors.Join(errs[:]...) == nil
	}
	return 0, 0, 0, false
}

func attachHidraw(node, name string, padsChanged func(int32)) (*attachment, error) {
	f, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	h := &hidrawLink{f: f, rc: rc}
	return startSession(name, []link{h}, false, padsChanged, func() { f.Close() }), nil
}

type hidrawLink struct {
	f  *os.File
	rc syscall.RawConn
	mu sync.Mutex // serialises writes and feature reports
}

func (h *hidrawLink) read(buf []byte, timeout time.Duration) (int, error) {
	if err := h.f.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	n, err := h.f.Read(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, errTimeout
	}
	return n, err
}

func (h *hidrawLink) writeOutput(report []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.f.Write(report)
	return err
}

// HIDIOCSFEATURE / HIDIOCGFEATURE (linux/hidraw.h).
func hidiocFeature(nr uintptr, size int) uintptr {
	return ioc(iocRead|iocWrite, 'H', nr, uintptr(size))
}

func (h *hidrawLink) setFeature(report []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := rawIoctlPtr(h.rc, hidiocFeature(0x06, len(report)), unsafe.Pointer(&report[0]))
	return err
}

func (h *hidrawLink) getFeature(buf []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return rawIoctlPtr(h.rc, hidiocFeature(0x07, len(buf)), unsafe.Pointer(&buf[0]))
}
