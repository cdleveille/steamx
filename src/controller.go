package main

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"time"
)

// Steam Controller (2026) HID protocol, as implemented by the Linux hid-steam driver
// ("Ibex") and SDL ("Triton").
const (
	valveVendor = 0x28de

	pidWired  = 0x1302
	pidBLE    = 0x1303
	pidPuck   = 0x1304
	pidNereid = 0x1305 // Steam Machine built-in receiver

	reportFeatureController = 0x01
	reportFeatureDongle     = 0x02
	reportState             = 0x42
	reportStateBLE          = 0x45
	reportWirelessX         = 0x46
	reportStateTimestamp    = 0x47
	reportWireless          = 0x79
	reportRumble            = 0x80

	cmdSetSettings         = 0x87
	cmdDongleWirelessState = 0xb4
	settingLizardMode      = 9

	wirelessDisconnect = 1
	wirelessConnect    = 2

	featureReportSize = 64

	// The firmware's watchdog turns lizard (mouse/keyboard) mode back on unless the host
	// keeps disabling it; this also means it recovers by itself if we stop.
	lizardRefresh = 3 * time.Second
	readTimeout   = 250 * time.Millisecond
)

// Steam Controller button bits mapped to Xbox 360 buttons. The back grip buttons, both
// trackpads and the quick access button have no XInput equivalent and are ignored.
var buttonMap = [numKeys]uint32{
	kA:     1 << 0,
	kB:     1 << 1,
	kX:     1 << 2,
	kY:     1 << 3,
	kR3:    1 << 5,
	kStart: 1 << 6, // Menu
	kRB:    1 << 9,
	kBack:  1 << 14, // View
	kL3:    1 << 15,
	kGuide: 1 << 16, // Steam
	kLB:    1 << 19,
}

const (
	dpadDown  = 1 << 10
	dpadRight = 1 << 11
	dpadLeft  = 1 << 12
	dpadUp    = 1 << 13
)

// link is one controller channel: a wired or Bluetooth controller, or one slot of a
// wireless receiver. Implementations must allow writes concurrently with read.
type link interface {
	read(buf []byte, timeout time.Duration) (int, error) // errTimeout if nothing arrived
	writeOutput(report []byte) error
	setFeature(report []byte) error
	getFeature(buf []byte) (int, error)
}

var errTimeout = errors.New("timeout")

// runController mirrors one controller channel onto a virtual Xbox 360 pad until ctx is
// cancelled or the device goes away.
func runController(ctx context.Context, l link, wireless bool, padsChanged func(int32)) error {
	var pad *xpad
	closePad := func() {
		if pad != nil {
			pad.Close()
			pad = nil
			padsChanged(-1)
		}
	}
	defer closePad()

	connected := !wireless || dongleSlotConnected(l)
	var nextLizard time.Time
	lizardWarned := false
	buf := make([]byte, 64)
	for ctx.Err() == nil {
		if connected {
			if pad == nil {
				rumbleWarned := false
				p, err := newXpad(func(strong, weak uint16) {
					// A lost packet is superseded by the next refresh, so only report the first failure.
					if err := l.writeOutput(rumbleReport(strong, weak)); err != nil && !rumbleWarned {
						log.Printf("sending rumble failed: %v", err)
						rumbleWarned = true
					}
				})
				if err != nil {
					return err
				}
				pad = p
				padsChanged(+1)
			}
			if now := time.Now(); !now.Before(nextLizard) {
				if err := l.setFeature(lizardOffReport()); err != nil && !lizardWarned {
					log.Printf("disabling lizard mode failed: %v", err)
					lizardWarned = true
				}
				nextLizard = now.Add(lizardRefresh)
			}
		}

		n, err := l.read(buf, readTimeout)
		if errors.Is(err, errTimeout) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if n < 2 {
			continue
		}
		switch buf[0] {
		case reportState, reportStateBLE, reportStateTimestamp:
			if n < 18 {
				continue
			}
			if !connected {
				connected, nextLizard = true, time.Time{}
				continue
			}
			if err := pad.update(toXbox(buf[:n])); err != nil {
				return err
			}
		case reportWireless, reportWirelessX:
			switch buf[1] {
			case wirelessConnect:
				if !connected {
					connected, nextLizard = true, time.Time{}
				}
			case wirelessDisconnect:
				connected = false
				closePad()
			}
		}
	}
	return nil
}

// toXbox converts a controller state report (report ID included) into Xbox 360 pad state.
func toXbox(r []byte) padState {
	buttons := binary.LittleEndian.Uint32(r[2:])
	axis := func(off int) int32 { return int32(int16(binary.LittleEndian.Uint16(r[off:]))) }

	var s padState
	for i, mask := range buttonMap {
		s.keys[i] = buttons&mask != 0
	}
	s.axes[axLT] = trigger(axis(6))
	s.axes[axRT] = trigger(axis(8))
	s.axes[axLX] = axis(10)
	s.axes[axLY] = invertY(axis(12))
	s.axes[axRX] = axis(14)
	s.axes[axRY] = invertY(axis(16))
	s.axes[axHatX] = bit(buttons, dpadRight) - bit(buttons, dpadLeft)
	s.axes[axHatY] = bit(buttons, dpadDown) - bit(buttons, dpadUp)
	return s
}

// invertY converts the controller's up-is-positive stick Y axis to evdev's down-is-positive.
func invertY(v int32) int32 { return min(-v, 32767) }

// trigger scales the controller's 0..32767 trigger travel to the Xbox 360's 0..255.
func trigger(v int32) int32 { return max(v, 0) * 255 / 32767 }

func bit(buttons, mask uint32) int32 {
	if buttons&mask != 0 {
		return 1
	}
	return 0
}

func lizardOffReport() []byte {
	r := make([]byte, featureReportSize)
	r[0] = reportFeatureController
	r[1] = cmdSetSettings
	r[2] = 3 // payload length: one setting (u8 id, u16 value)
	r[3] = settingLizardMode
	// r[4:6] = 0: lizard mode off
	return r
}

func rumbleReport(strong, weak uint16) []byte {
	r := make([]byte, 10)
	r[0] = reportRumble
	// r[1] type, r[2:4] intensity: 0
	binary.LittleEndian.PutUint16(r[4:], strong) // left (low-frequency) motor, gain r[6] = 0 dB
	binary.LittleEndian.PutUint16(r[7:], weak)   // right (high-frequency) motor, gain r[9] = 0 dB
	return r
}

// dongleSlotConnected asks a wireless receiver whether a controller is paired to this slot.
func dongleSlotConnected(l link) bool {
	req := make([]byte, featureReportSize)
	req[0], req[1] = reportFeatureDongle, cmdDongleWirelessState
	if l.setFeature(req) != nil {
		return false
	}
	resp := make([]byte, featureReportSize)
	resp[0] = reportFeatureDongle
	n, err := l.getFeature(resp)
	return err == nil && n >= 4 && resp[1] == cmdDongleWirelessState && resp[3] == wirelessConnect
}
