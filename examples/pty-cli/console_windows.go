//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

const (
	enableProcessedInput      = 0x0001
	enableLineInput           = 0x0002
	enableEchoInput           = 0x0004
	enableQuickEditMode       = 0x0040
	enableExtendedFlags       = 0x0080
	enableVirtualTerminalInput = 0x0200

	enableVirtualTerminalProcessing = 0x0004
	disableNewlineAutoReturn        = 0x0008
)

// enterRawMode puts the Windows console input into raw mode (no line input,
// no echo, no processed input) with VT input enabled so that every keystroke
// - including arrow/function key escape sequences - arrives as raw bytes.
// The console is switched to UTF-8 and VT output processing for rendering.
func enterRawMode() (func(), error) {
	hIn := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(hIn, &mode); err != nil {
		return nil, err
	}

	// enableQuickEditMode is deliberately kept so the user can still select
	// console text with the mouse; it does not affect the input byte stream.
	raw := mode &^ (enableProcessedInput | enableLineInput | enableEchoInput)
	raw |= enableExtendedFlags | enableQuickEditMode | enableVirtualTerminalInput
	if err := windows.SetConsoleMode(hIn, raw); err != nil {
		return nil, err
	}

	// UTF-8 in/out + ANSI rendering so pty bytes display correctly.
	_ = windows.SetConsoleCP(65001)
	hOut := windows.Handle(os.Stdout.Fd())
	var outMode uint32
	if err := windows.GetConsoleMode(hOut, &outMode); err == nil {
		_ = windows.SetConsoleMode(hOut, outMode|enableVirtualTerminalProcessing|disableNewlineAutoReturn)
	}
	_ = windows.SetConsoleOutputCP(65001)

	return func() { _ = windows.SetConsoleMode(hIn, mode) }, nil
}
