//go:build unix

package main

import (
	"os"

	"golang.org/x/term"
)

// enterRawMode puts stdin into raw mode (termios: -icanon -echo -isig ...)
// and returns a restore function.
func enterRawMode() (func(), error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(fd, state) }, nil
}
