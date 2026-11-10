package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// Linux _IOC() encoding.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

// ioctlPtr issues an ioctl whose argument is a pointer and returns the syscall's result value.
func ioctlPtr(fd uintptr, req uintptr, arg unsafe.Pointer) (int, error) {
	for {
		r, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return 0, errno
		}
		return int(r), nil
	}
}

// ioctlVal issues an ioctl whose argument is a plain integer.
func ioctlVal(fd uintptr, req uintptr, val uintptr) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, val)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

// rawIoctlPtr runs ioctlPtr on a file managed by the Go runtime poller without
// switching the descriptor to blocking mode (which (*os.File).Fd would do).
func rawIoctlPtr(rc syscall.RawConn, req uintptr, arg unsafe.Pointer) (n int, err error) {
	cerr := rc.Control(func(fd uintptr) { n, err = ioctlPtr(fd, req, arg) })
	return n, errors.Join(cerr, err)
}

func rawIoctlVal(rc syscall.RawConn, req uintptr, val uintptr) (err error) {
	cerr := rc.Control(func(fd uintptr) { err = ioctlVal(fd, req, val) })
	return errors.Join(cerr, err)
}
