//go:build !windows

package ui

import "os"

// Every modern unix terminal parses ANSI already.
func vtReady(f *os.File) bool { return true }
