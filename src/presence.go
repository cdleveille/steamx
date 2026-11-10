package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// A connected controller streams ~250 state reports a second, even when idle.
	slotProbeTimeout = 150 * time.Millisecond
	// Brief gaps, e.g. a mode switch handing a controller back to the kernel, are ignored.
	presenceGrace = 1500 * time.Millisecond
)

// updatePresence counts the connected Steam Controllers and updates the tray.
func (m *manager) updatePresence() {
	xinput := m.on.Load()
	n := len(findBluetoothControllers())
	var probes []string
	for _, c := range listUSBControllers() {
		switch {
		case c.pid == pidWired:
			n++
		case !xinput: // in XInput mode a receiver's controllers are counted as pads
			for _, s := range receiverSlots(c) {
				if s.registered {
					n++
				} else if s.node != "" {
					probes = append(probes, s.node)
				}
			}
		}
	}
	// The pads include the wired and Bluetooth controllers already counted.
	n = max(n+countStreaming(probes), int(m.pads.Load()))

	if n < int(m.controllers.Load()) {
		if m.lostAt.IsZero() {
			m.lostAt = time.Now()
		}
		if time.Since(m.lostAt) < presenceGrace {
			return
		}
	}
	m.lostAt = time.Time{}
	if m.controllers.Swap(int32(n)) != int32(n) {
		m.changed()
	}
}

// receiverSlot is one controller slot of a wireless receiver.
type receiverSlot struct {
	node       string // hidraw node carrying the slot's reports
	registered bool   // hid-steam has registered the slot's controller
}

func receiverSlots(c usbController) []receiverSlot {
	var out []receiverSlot
	for _, it := range controllerInterfaces(c) {
		var slot receiverSlot
		hids, _ := filepath.Glob(filepath.Join(it.dir, "0003:*"))
		for _, h := range hids {
			if nodes, _ := os.ReadDir(filepath.Join(h, "hidraw")); len(nodes) > 0 {
				slot.node = "/dev/" + nodes[0].Name()
			}
			slot.registered = slot.registered || steamRegistered(h)
		}
		out = append(out, slot)
	}
	return out
}

// steamRegistered reports whether hid-steam has registered a gamepad, motion sensors or a
// battery for a connected controller on a HID device. It does that only while no program
// has the slot's hidraw node open, e.g. when Steam isn't running.
func steamRegistered(hidDir string) bool {
	if supplies, _ := os.ReadDir(filepath.Join(hidDir, "power_supply")); len(supplies) > 0 {
		return true
	}
	uevent, _ := os.ReadFile(filepath.Join(hidDir, "uevent"))
	hidName := ""
	for _, line := range strings.Split(string(uevent), "\n") {
		if v, ok := strings.CutPrefix(line, "HID_NAME="); ok {
			hidName = v
		}
	}
	names, _ := filepath.Glob(filepath.Join(hidDir, "input", "input*", "name"))
	for _, n := range names {
		// The receiver's own mouse/keyboard emulation input is named after the HID device.
		if name, _ := os.ReadFile(n); !strings.HasPrefix(string(name), hidName) {
			return true
		}
	}
	return false
}

// countStreaming probes receiver slots concurrently and counts those a controller is
// streaming on.
func countStreaming(nodes []string) int {
	streaming := make(chan bool, len(nodes))
	for _, node := range nodes {
		go func() { streaming <- probeSlot(node) }()
	}
	n := 0
	for range nodes {
		if <-streaming {
			n++
		}
	}
	return n
}

// probeSlot briefly listens to a receiver slot without sending anything. hid-steam is only
// told about a hidraw node's first open and last close, so an extra reader alongside Steam
// goes unnoticed by both; without other readers no controller is registered, so there is
// nothing to disturb.
func probeSlot(node string) bool {
	f, err := os.Open(node)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if f.SetReadDeadline(time.Now().Add(slotProbeTimeout)) != nil {
		return false
	}
	buf := make([]byte, 64)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return false
		}
		if n < 2 {
			continue
		}
		switch buf[0] {
		case reportState, reportStateBLE, reportStateTimestamp:
			return true
		case reportWireless, reportWirelessX:
			if buf[1] == wirelessDisconnect {
				return false
			}
		}
	}
}
