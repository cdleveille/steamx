package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"sync"

	"fyne.io/systray"
)

const appTitle = "steamx"

// tray is the whole UI: a tray icon whose single switch toggles XInput mode.
// Left-clicking the icon flips the switch too.
type tray struct {
	mgr        *manager
	mu         sync.Mutex
	toggle     *systray.MenuItem
	closed     bool // the D-Bus connection is gone once the tray has exited
	iconNone   []byte
	iconSteam  []byte
	iconXInput []byte
}

func newTray(mgr *manager) *tray {
	return &tray{
		mgr:        mgr,
		iconNone:   renderIcon(color.NRGBA{0xff, 0xff, 0xff, 0xff}),
		iconSteam:  renderIcon(color.NRGBA{0x1a, 0x9f, 0xff, 0xff}),
		iconXInput: renderIcon(color.NRGBA{0x3d, 0xbe, 0x4b, 0xff}),
	}
}

// run shows the tray icon and blocks until systray.Quit is called.
func (t *tray) run() {
	systray.SetOnTapped(t.flip) // must be set before Run to take effect
	systray.SetTitle(appTitle)
	t.refresh()
	systray.Run(t.onReady, t.onExit)
}

func (t *tray) onExit() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

func (t *tray) onReady() {
	toggle := systray.AddMenuItemCheckbox("XInput mode (Xbox 360 controller)",
		"Present the Steam Controller as a plain Xbox 360 controller", false)
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Return to Steam mode and exit")

	t.mu.Lock()
	t.toggle = toggle
	t.mu.Unlock()
	t.refresh()

	go func() {
		for {
			select {
			case <-toggle.ClickedCh:
				t.flip()
			case <-quit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func (t *tray) flip() {
	t.mgr.setEnabled(!t.mgr.enabled())
}

// refresh updates the icon, tooltip and switch to match the manager's state.
func (t *tray) refresh() {
	on, pads, connected := t.mgr.status()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	// systray panics if the session bus connection fails; the UI must never take the daemon
	// down while it holds a controller.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tray update failed: %v", r)
		}
	}()
	switch {
	case !connected:
		systray.SetIcon(t.iconNone)
		systray.SetTooltip("steamx: Steam Controller disconnected")
	case !on:
		systray.SetIcon(t.iconSteam)
		systray.SetTooltip("steamx: Steam Controller connected in default mode")
	default:
		systray.SetIcon(t.iconXInput)
		noun := "Steam Controllers"
		if pads == 1 {
			noun = "Steam Controller"
		}
		systray.SetTooltip(fmt.Sprintf("steamx: %d %s connected in XInput mode", pads, noun))
	}
	if t.toggle != nil {
		if on {
			t.toggle.Check()
		} else {
			t.toggle.Uncheck()
		}
	}
}

// renderIcon draws a gamepad silhouette in colour c as an antialiased 64x64 PNG.
func renderIcon(c color.NRGBA) []byte {
	const size, ss = 64, 4 // canvas size, supersampling per axis
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			hits := 0
			for sy := range ss {
				for sx := range ss {
					if inGamepad(float64(x)+(float64(sx)+0.5)/ss, float64(y)+(float64(sy)+0.5)/ss) {
						hits++
					}
				}
			}
			if hits > 0 {
				img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, uint8(hits * 255 / (ss * ss))})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img) // cannot fail for an in-memory NRGBA image
	return buf.Bytes()
}

func inGamepad(x, y float64) bool {
	circle := func(cx, cy, r float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r }
	rect := func(x0, y0, x1, y1 float64) bool { return x >= x0 && x <= x1 && y >= y0 && y <= y1 }

	body := circle(20, 28, 14) || circle(44, 28, 14) || rect(20, 14, 44, 42) ||
		circle(16, 42, 10) || circle(48, 42, 10)
	dpad := rect(13, 25.5, 27, 30.5) || rect(17.5, 21, 22.5, 35)
	buttons := circle(44, 22, 3) || circle(44, 34, 3) || circle(38, 28, 3) || circle(50, 28, 3)
	return body && !dpad && !buttons
}
