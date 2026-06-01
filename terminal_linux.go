package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal returns true if fd is connected to a terminal.
func isTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd,
		syscall.TCGETS, uintptr(unsafe.Pointer(&termios)))
	return err == 0
}

// Ensure os is imported (used in main).
var _ = os.Stdout
