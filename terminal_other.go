//go:build !linux

package main



func isTerminal(fd uintptr) bool {
	// Conservative: assume not a terminal on unsupported platforms
	return false
}
