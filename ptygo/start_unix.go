//go:build unix

package ptygo

func startPlatform(opts Options) (Connection, error) {
	return startUnix(opts)
}
