package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Linux input event codes (linux/input-event-codes.h, linux/uinput.h).
const (
	evSyn    = 0x00
	evKey    = 0x01
	evAbs    = 0x03
	evFF     = 0x15
	evUinput = 0x0101

	synReport = 0

	keyBtnA      = 0x130
	keyBtnB      = 0x131
	keyBtnX      = 0x133
	keyBtnY      = 0x134
	keyBtnTL     = 0x136
	keyBtnTR     = 0x137
	keyBtnSelect = 0x13a
	keyBtnStart  = 0x13b
	keyBtnMode   = 0x13c
	keyBtnThumbL = 0x13d
	keyBtnThumbR = 0x13e

	absX     = 0x00
	absY     = 0x01
	absZ     = 0x02
	absRX    = 0x03
	absRY    = 0x04
	absRZ    = 0x05
	absHat0X = 0x10
	absHat0Y = 0x11

	ffRumble = 0x50

	uiFFUpload = 1
	uiFFErase  = 2

	busUSB = 0x03
)

// Identity of a wired Xbox 360 controller exactly as the kernel's xpad driver exposes it.
const (
	xboxVendor   = 0x045e
	xboxProduct  = 0x028e
	xboxVersion  = 0x0114
	xboxName     = "Microsoft X-Box 360 pad"
	ffMaxEffects = 16
)

// Keys of the virtual pad, in the same set the xpad driver registers for a 360 pad.
const (
	kA = iota
	kB
	kX
	kY
	kLB
	kRB
	kBack
	kStart
	kGuide
	kL3
	kR3
	numKeys
)

var xboxKeys = [numKeys]uint16{
	kA: keyBtnA, kB: keyBtnB, kX: keyBtnX, kY: keyBtnY,
	kLB: keyBtnTL, kRB: keyBtnTR,
	kBack: keyBtnSelect, kStart: keyBtnStart, kGuide: keyBtnMode,
	kL3: keyBtnThumbL, kR3: keyBtnThumbR,
}

const (
	axLX = iota
	axLY
	axLT
	axRX
	axRY
	axRT
	axHatX
	axHatY
	numAxes
)

// Axis ranges match xpad: 16-bit sticks, 8-bit triggers, D-pad as a hat.
var xboxAxes = [numAxes]struct {
	code                 uint16
	min, max, fuzz, flat int32
}{
	axLX:   {absX, -32768, 32767, 16, 128},
	axLY:   {absY, -32768, 32767, 16, 128},
	axLT:   {absZ, 0, 255, 0, 0},
	axRX:   {absRX, -32768, 32767, 16, 128},
	axRY:   {absRY, -32768, 32767, 16, 128},
	axRT:   {absRZ, 0, 255, 0, 0},
	axHatX: {absHat0X, -1, 1, 0, 0},
	axHatY: {absHat0Y, -1, 1, 0, 0},
}

type padState struct {
	keys [numKeys]bool
	axes [numAxes]int32
}

// Kernel structures (64-bit layout).
type inputID struct{ Bustype, Vendor, Product, Version uint16 }

type uinputSetup struct {
	ID           inputID
	Name         [80]byte
	FFEffectsMax uint32
}

type inputAbsInfo struct{ Value, Minimum, Maximum, Fuzz, Flat, Resolution int32 }

type uinputAbsSetup struct {
	Code uint16
	_    uint16
	Info inputAbsInfo
}

type ffEffect struct {
	Type      uint16
	ID        int16
	Direction uint16
	Trigger   [2]uint16
	Replay    [2]uint16 // length, delay in ms
	_         uint16
	U         [32]byte // union; FF_RUMBLE uses {strong, weak uint16}
}

type uinputFFUpload struct {
	RequestID uint32
	Retval    int32
	Effect    ffEffect
	Old       ffEffect
}

type uinputFFErase struct {
	RequestID uint32
	Retval    int32
	EffectID  uint32
}

var (
	uiDevCreate     = ioc(iocNone, 'U', 1, 0)
	uiDevSetup      = ioc(iocWrite, 'U', 3, unsafe.Sizeof(uinputSetup{}))
	uiAbsSetup      = ioc(iocWrite, 'U', 4, unsafe.Sizeof(uinputAbsSetup{}))
	uiSetEvBit      = ioc(iocWrite, 'U', 100, 4)
	uiSetKeyBit     = ioc(iocWrite, 'U', 101, 4)
	uiSetAbsBit     = ioc(iocWrite, 'U', 103, 4)
	uiSetFFBit      = ioc(iocWrite, 'U', 107, 4)
	uiBeginFFUpload = ioc(iocRead|iocWrite, 'U', 200, unsafe.Sizeof(uinputFFUpload{}))
	uiEndFFUpload   = ioc(iocWrite, 'U', 201, unsafe.Sizeof(uinputFFUpload{}))
	uiBeginFFErase  = ioc(iocRead|iocWrite, 'U', 202, unsafe.Sizeof(uinputFFErase{}))
	uiEndFFErase    = ioc(iocWrite, 'U', 203, unsafe.Sizeof(uinputFFErase{}))
)

const (
	timevalSize    = int(unsafe.Sizeof(syscall.Timeval{}))
	inputEventSize = timevalSize + 8

	// The Steam Controller stops rumbling ~50 ms after the last rumble packet, so active
	// rumble is refreshed at this interval (which also caps the packet rate).
	rumbleInterval = 40 * time.Millisecond
)

// xpad is a virtual Xbox 360 controller backed by /dev/uinput.
type xpad struct {
	f      *os.File
	rc     syscall.RawConn
	rumble func(strong, weak uint16)
	last   padState
	wbuf   []byte
	ffDone chan struct{}
}

func newXpad(rumble func(strong, weak uint16)) (*xpad, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	p := &xpad{f: f, rc: rc, rumble: rumble, ffDone: make(chan struct{})}
	if err := p.create(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("create virtual Xbox 360 pad: %w", err)
	}
	go p.ffLoop()
	return p, nil
}

func (p *xpad) create() error {
	for _, ev := range []uintptr{evKey, evAbs, evFF} {
		if err := rawIoctlVal(p.rc, uiSetEvBit, ev); err != nil {
			return err
		}
	}
	for _, k := range xboxKeys {
		if err := rawIoctlVal(p.rc, uiSetKeyBit, uintptr(k)); err != nil {
			return err
		}
	}
	for _, a := range xboxAxes {
		if err := rawIoctlVal(p.rc, uiSetAbsBit, uintptr(a.code)); err != nil {
			return err
		}
	}
	if err := rawIoctlVal(p.rc, uiSetFFBit, ffRumble); err != nil {
		return err
	}

	setup := uinputSetup{
		ID:           inputID{Bustype: busUSB, Vendor: xboxVendor, Product: xboxProduct, Version: xboxVersion},
		FFEffectsMax: ffMaxEffects,
	}
	copy(setup.Name[:], xboxName)
	if _, err := rawIoctlPtr(p.rc, uiDevSetup, unsafe.Pointer(&setup)); err != nil {
		return err
	}
	for _, a := range xboxAxes {
		abs := uinputAbsSetup{Code: a.code, Info: inputAbsInfo{Minimum: a.min, Maximum: a.max, Fuzz: a.fuzz, Flat: a.flat}}
		if _, err := rawIoctlPtr(p.rc, uiAbsSetup, unsafe.Pointer(&abs)); err != nil {
			return err
		}
	}
	return rawIoctlVal(p.rc, uiDevCreate, 0)
}

// update emits the events needed to move the virtual pad to state s.
func (p *xpad) update(s padState) error {
	b := p.wbuf[:0]
	for i, code := range xboxKeys {
		if s.keys[i] != p.last.keys[i] {
			v := int32(0)
			if s.keys[i] {
				v = 1
			}
			b = appendEvent(b, evKey, code, v)
		}
	}
	for i, a := range xboxAxes {
		if s.axes[i] != p.last.axes[i] {
			b = appendEvent(b, evAbs, a.code, s.axes[i])
		}
	}
	if len(b) == 0 {
		return nil
	}
	b = appendEvent(b, evSyn, synReport, 0)
	p.wbuf = b
	if _, err := p.f.Write(b); err != nil {
		return err
	}
	p.last = s
	return nil
}

func appendEvent(b []byte, typ, code uint16, value int32) []byte {
	var ev [inputEventSize]byte // the kernel timestamps injected events itself
	binary.NativeEndian.PutUint16(ev[timevalSize:], typ)
	binary.NativeEndian.PutUint16(ev[timevalSize+2:], code)
	binary.NativeEndian.PutUint32(ev[timevalSize+4:], uint32(value))
	return append(b, ev[:]...)
}

// Close removes the virtual pad and stops any rumble it started.
func (p *xpad) Close() {
	_ = p.f.Close()
	<-p.ffDone
}

// ffSlot is one uploaded FF_RUMBLE effect.
type ffSlot struct {
	strong, weak  uint16
	length, delay time.Duration
	playing       bool
	count         int
	start, end    time.Time // end is zero for effects that play until stopped
}

func (s *ffSlot) play(now time.Time, count int) {
	s.playing, s.count = true, count
	s.start = now.Add(s.delay)
	s.end = time.Time{}
	if s.length > 0 {
		s.end = now.Add(time.Duration(count) * (s.delay + s.length))
	}
}

// ffLoop services force-feedback requests from games (effect uploads, playback) and
// turns them into Steam Controller rumble packets.
func (p *xpad) ffLoop() {
	defer close(p.ffDone)
	var (
		fx         [ffMaxEffects]ffSlot
		sentStrong uint16
		sentWeak   uint16
		sentAt     time.Time
		buf        = make([]byte, inputEventSize*32)
	)
	for {
		now := time.Now()
		strong, weak, wake := mixRumble(&fx, now)
		pending := strong != sentStrong || weak != sentWeak
		if pending || strong|weak != 0 {
			due := sentAt.Add(rumbleInterval)
			if !now.Before(due) {
				p.rumble(strong, weak)
				sentStrong, sentWeak, sentAt = strong, weak, now
				due, pending = now.Add(rumbleInterval), false
			}
			if pending || strong|weak != 0 {
				wake = earliest(wake, due)
			}
		}

		if err := p.f.SetReadDeadline(wake); err != nil {
			break
		}
		n, err := p.f.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			break // device closed
		}
		for off := 0; off+inputEventSize <= n; off += inputEventSize {
			ev := buf[off+timevalSize:]
			p.handleFF(&fx, binary.NativeEndian.Uint16(ev), binary.NativeEndian.Uint16(ev[2:]),
				int32(binary.NativeEndian.Uint32(ev[4:])))
		}
	}
	if sentStrong|sentWeak != 0 {
		p.rumble(0, 0)
	}
}

func (p *xpad) handleFF(fx *[ffMaxEffects]ffSlot, typ, code uint16, value int32) {
	now := time.Now()
	switch {
	case typ == evUinput && code == uiFFUpload:
		up := uinputFFUpload{RequestID: uint32(value)}
		if _, err := rawIoctlPtr(p.rc, uiBeginFFUpload, unsafe.Pointer(&up)); err != nil {
			return
		}
		if e := up.Effect; e.Type == ffRumble && e.ID >= 0 && int(e.ID) < ffMaxEffects {
			s := &fx[e.ID]
			s.strong = binary.NativeEndian.Uint16(e.U[0:])
			s.weak = binary.NativeEndian.Uint16(e.U[2:])
			s.length = time.Duration(e.Replay[0]) * time.Millisecond
			s.delay = time.Duration(e.Replay[1]) * time.Millisecond
			if s.playing {
				s.play(now, s.count) // like the kernel's memless FF: updating a running effect restarts it
			}
		} else {
			up.Retval = -int32(syscall.EINVAL)
		}
		_, _ = rawIoctlPtr(p.rc, uiEndFFUpload, unsafe.Pointer(&up))

	case typ == evUinput && code == uiFFErase:
		er := uinputFFErase{RequestID: uint32(value)}
		if _, err := rawIoctlPtr(p.rc, uiBeginFFErase, unsafe.Pointer(&er)); err != nil {
			return
		}
		if er.EffectID < ffMaxEffects {
			fx[er.EffectID] = ffSlot{}
		}
		_, _ = rawIoctlPtr(p.rc, uiEndFFErase, unsafe.Pointer(&er))

	case typ == evFF && int(code) < ffMaxEffects:
		if value > 0 {
			fx[code].play(now, int(value))
		} else {
			fx[code].playing = false
		}
	}
}

// mixRumble sums all running effects and reports when the result may next change.
func mixRumble(fx *[ffMaxEffects]ffSlot, now time.Time) (strong, weak uint16, wake time.Time) {
	var s, w uint32
	for i := range fx {
		e := &fx[i]
		if !e.playing {
			continue
		}
		if !e.end.IsZero() {
			if !now.Before(e.end) {
				e.playing = false
				continue
			}
			wake = earliest(wake, e.end)
		}
		if now.Before(e.start) {
			wake = earliest(wake, e.start)
			continue
		}
		s += uint32(e.strong)
		w += uint32(e.weak)
	}
	return uint16(min(s, 0xffff)), uint16(min(w, 0xffff)), wake
}

// earliest returns the earlier of two times, treating the zero time as "never".
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
