//go:build windows

package ptygo

func startPlatform(opts Options) (Connection, error) {
	if opts.ForceWinPty {
		return startWinpty(opts)
	}
	if !conptyAvailable() {
		// Pre-ConPTY Windows (before 1809 / Server 2019): fall back to winpty.
		return startWinpty(opts)
	}
	return startConpty(opts)
}
