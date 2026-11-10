// steamx is a small tray daemon that switches a Steam Controller (2026)
// between its normal Steam mode and an XInput mode in which it is hidden from Steam and
// other applications and presented as a plain wired Xbox 360 controller instead.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"fyne.io/systray"
)

func main() {
	// steamx mostly waits; one scheduler thread avoids waking idle threads on many-core CPUs.
	runtime.GOMAXPROCS(1)
	log.SetFlags(log.LstdFlags)

	lock, err := lockSingleInstance()
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = lock.Close() }()

	reattachOrphanedInterfaces()

	mgr := newManager()
	ui := newTray(mgr)
	mgr.onChange = ui.refresh
	go mgr.run()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigs
		systray.Quit()
	}()

	ui.run()
	mgr.shutdown() // always hand the controllers back to the kernel driver and Steam
}

func lockSingleInstance() (*os.File, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, "steamx.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("already running (lock held on %s)", path)
	}
	return f, nil
}
