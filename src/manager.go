package main

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	scanInterval = time.Second
	retryDelay   = 3 * time.Second
)

// candidate is a Steam Controller present on the system that can be put in XInput mode.
type candidate struct {
	key    string
	attach func(padsChanged func(int32)) (*attachment, error)
}

// attachment is a controller (or receiver) currently held in XInput mode.
type attachment struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error // valid once done is closed
}

// manager switches every connected Steam Controller between Steam mode (left alone) and
// XInput mode (taken over and re-exposed as an Xbox 360 pad), including hot-plugged ones.
type manager struct {
	on        atomic.Bool
	pads      atomic.Int32
	connected atomic.Bool
	onChange  func()

	kick    chan struct{}
	quit    chan struct{}
	stopped chan struct{}

	// Owned by run.
	active  map[string]*attachment
	retryAt map[string]time.Time
	lastErr map[string]string
	lostAt  time.Time // when the last connected controller was first found missing
	// lastStreaming is the receiver slot a controller was last seen streaming on.
	lastStreaming string
}

func newManager() *manager {
	return &manager{
		kick:    make(chan struct{}, 1),
		quit:    make(chan struct{}),
		stopped: make(chan struct{}),
		active:  map[string]*attachment{},
		retryAt: map[string]time.Time{},
		lastErr: map[string]string{},
	}
}

func (m *manager) enabled() bool { return m.on.Load() }

// status reports the mode, how many controllers are emulated as Xbox 360 pads, and
// whether any Steam Controller is connected.
func (m *manager) status() (xinput bool, pads int, connected bool) {
	pads = int(m.pads.Load())
	return m.on.Load(), pads, pads > 0 || m.connected.Load()
}

func (m *manager) setEnabled(on bool) {
	if on {
		// Without uinput there is nothing to present, so leave the controller alone.
		if err := syscall.Access("/dev/uinput", 6 /* R_OK|W_OK */); err != nil {
			log.Printf("cannot enable XInput mode: /dev/uinput: %v", err)
			m.changed()
			return
		}
	}
	if m.on.Swap(on) != on {
		if on {
			log.Print("XInput mode enabled")
		} else {
			log.Print("XInput mode disabled")
		}
		m.wake()
	}
	m.changed()
}

func (m *manager) padsChanged(delta int32) {
	m.pads.Add(delta)
	m.wake()
	m.changed()
}

// wake makes run rescan without waiting for the next tick.
func (m *manager) wake() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *manager) changed() {
	if m.onChange != nil {
		m.onChange()
	}
}

func (m *manager) run() {
	defer close(m.stopped)
	tick := time.NewTicker(scanInterval)
	defer tick.Stop()
	for {
		m.sync()
		m.updatePresence()
		select {
		case <-tick.C:
		case <-m.kick:
		case <-m.quit:
			m.releaseAll()
			return
		}
	}
}

// shutdown returns every controller to Steam mode and stops the manager.
func (m *manager) shutdown() {
	close(m.quit)
	<-m.stopped
}

func (m *manager) sync() {
	now := time.Now()
	for key, a := range m.active {
		select {
		case <-a.done:
			delete(m.active, key)
			if a.err != nil {
				m.report(key, a.err)
				m.retryAt[key] = now.Add(retryDelay)
			}
		default:
		}
	}
	if !m.on.Load() {
		m.releaseAll()
		return
	}
	for _, c := range append(findUSBControllers(), findBluetoothControllers()...) {
		if m.active[c.key] != nil || now.Before(m.retryAt[c.key]) {
			continue
		}
		a, err := c.attach(m.padsChanged)
		if err != nil {
			m.report(c.key, err)
			m.retryAt[c.key] = now.Add(retryDelay)
			continue
		}
		delete(m.lastErr, c.key)
		m.active[c.key] = a
	}
}

func (m *manager) releaseAll() {
	for _, a := range m.active {
		a.cancel()
	}
	for key, a := range m.active {
		<-a.done
		delete(m.active, key)
	}
}

// report logs an error unless it repeats the last one logged for that controller.
func (m *manager) report(key string, err error) {
	if msg := err.Error(); m.lastErr[key] != msg {
		m.lastErr[key] = msg
		log.Printf("%s: %v", key, err)
	}
}

// startSession runs one controller loop per link and calls cleanup once all have stopped.
func startSession(name string, links []link, wireless bool, padsChanged func(int32), cleanup func()) *attachment {
	ctx, cancel := context.WithCancel(context.Background())
	a := &attachment{cancel: cancel, done: make(chan struct{})}
	log.Printf("%s: XInput mode", name)
	go func() {
		defer close(a.done)
		var wg sync.WaitGroup
		var once sync.Once
		for _, l := range links {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := runController(ctx, l, wireless, padsChanged); err != nil {
					once.Do(func() { a.err = err })
					cancel()
				}
			}()
		}
		wg.Wait()
		cancel()
		cleanup()
		log.Printf("%s: Steam mode", name)
	}()
	return a
}
