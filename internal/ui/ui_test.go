package ui

import (
	"os"
	"testing"
)

func TestDevNullIsNotATerminal(t *testing.T) {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skip(err)
	}
	defer null.Close()
	if IsTTY(null) {
		t.Error("IsTTY(/dev/null) = true; `2>/dev/null` would turn styling on")
	}
}
