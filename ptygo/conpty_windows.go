//go:build windows

package ptygo

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procCreatePipe               = modKernel32.NewProc("CreatePipe")
	procCreatePseudoConsole      = modKernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole      = modKernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole       = modKernel32.NewProc("ClosePseudoConsole")
	procInitializeThreadAttrList = modKernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateThreadAttr         = modKernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteThreadAttrList     = modKernel32.NewProc("DeleteProcThreadAttributeList")
	procCreateProcessW           = modKernel32.NewProc("CreateProcessW")
)

const (
	procThreadAttributePseudoConsole = 0x00020016
	extendedStartupinfoPresent       = 0x00080000
	createUnicodeEnvironment         = 0x00000400
	startfUseStdHandles              = 0x00000100
)

// conptyAvailable reports whether the current Windows provides ConPTY
// (Windows 10 1809+ / Server 2019+).
func conptyAvailable() bool {
	// A successful GetProcAddress for CreatePseudoConsole means the API exists.
	proc := modKernel32.NewProc("CreatePseudoConsole")
	return proc.Find() == nil
}

// coord mirrors the native COORD structure.
type coord struct {
	X int16
	Y int16
}

// startupInfoEx mirrors the native STARTUPINFOEX structure.
type startupInfoEx struct {
	StartupInfo    windows.StartupInfo
	lpAttributeList uintptr
}

// processInformation mirrors the native PROCESS_INFORMATION structure.
type processInformation struct {
	Process   uintptr
	Thread    uintptr
	ProcessID uint32
	ThreadID  uint32
}

func createPipe() (read, write windows.Handle, err error) {
	var sa windows.SecurityAttributes
	sa.Length = uint32(unsafe.Sizeof(sa))
	sa.InheritHandle = 1

	r1, _, e1 := procCreatePipe.Call(
		uintptr(unsafe.Pointer(&read)),
		uintptr(unsafe.Pointer(&write)),
		uintptr(unsafe.Pointer(&sa)),
		0,
	)
	if r1 == 0 {
		return 0, 0, fmt.Errorf("CreatePipe failed: %w", e1)
	}
	return read, write, nil
}

func createPseudoConsole(size coord, hInput, hOutput windows.Handle) (windows.Handle, error) {
	var hPC windows.Handle
	// COORD is passed BY VALUE: pack X into the low 16 bits and Y into bits
	// 16-31 (the native COORD layout). Passing a pointer here makes conhost
	// interpret a heap address as the console size, which breaks child
	// initialization (cmd.exe exits with 0xC0000142 or exits immediately).
	packed := uint32(uint16(size.Y))<<16 | uint32(uint16(size.X))
	hr, _, _ := procCreatePseudoConsole.Call(
		uintptr(packed),
		uintptr(hInput),
		uintptr(hOutput),
		0,
		uintptr(unsafe.Pointer(&hPC)),
	)
	if hr != 0 {
		return 0, fmt.Errorf("CreatePseudoConsole failed: HRESULT 0x%08X", uint32(hr))
	}
	return hPC, nil
}

func initializeProcThreadAttributeList(count uint32) (list uintptr, buf []byte, err error) {
	var size uintptr
	r1, _, e1 := procInitializeThreadAttrList.Call(0, uintptr(count), 0, uintptr(unsafe.Pointer(&size)))
	if r1 == 0 && e1 != windows.ERROR_INSUFFICIENT_BUFFER {
		return 0, nil, fmt.Errorf("InitializeProcThreadAttributeList (size query) failed: %w", e1)
	}

	buf = make([]byte, size)
	r1, _, e1 = procInitializeThreadAttrList.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(count),
		0,
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		return 0, nil, fmt.Errorf("InitializeProcThreadAttributeList failed: %w", e1)
	}
	return uintptr(unsafe.Pointer(&buf[0])), buf, nil
}

func updateProcThreadAttribute(list uintptr, attribute uintptr, value uintptr) error {
	r1, _, e1 := procUpdateThreadAttr.Call(
		list,
		0,
		attribute,
		value,
		unsafe.Sizeof(value),
		0,
		0,
	)
	if r1 == 0 {
		return fmt.Errorf("UpdateProcThreadAttribute failed: %w", e1)
	}
	return nil
}

func deleteProcThreadAttributeList(list uintptr) {
	_, _, _ = procDeleteThreadAttrList.Call(list)
}

func createProcess(cmdLine string, envBlock []uint16, cwd string, si *startupInfoEx) (*processInformation, error) {
	cmdLinePtr, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return nil, err
	}

	var envPtr *uint16
	if len(envBlock) > 0 {
		envPtr = &envBlock[0]
	}

	var cwdPtr *uint16
	if cwd != "" {
		cwdPtr, err = windows.UTF16PtrFromString(cwd)
		if err != nil {
			return nil, err
		}
	}

	// Mirror the C# implementation: pass real (empty) security attributes
	// instead of NULL; empirically this makes ConPTY creation more reliable.
	var sa windows.SecurityAttributes
	sa.Length = uint32(unsafe.Sizeof(sa))

	var pi processInformation
	r1, _, e1 := procCreateProcessW.Call(
		0, // lpApplicationName
		uintptr(unsafe.Pointer(cmdLinePtr)),
		uintptr(unsafe.Pointer(&sa)), // lpProcessAttributes
		uintptr(unsafe.Pointer(&sa)), // lpThreadAttributes
		0,                            // bInheritHandles
		extendedStartupinfoPresent|createUnicodeEnvironment,
		uintptr(unsafe.Pointer(envPtr)),
		uintptr(unsafe.Pointer(cwdPtr)),
		uintptr(unsafe.Pointer(si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("CreateProcess failed: %w", e1)
	}
	return &pi, nil
}

// buildEnvBlock builds a sorted, double-null terminated UTF-16 environment
// block from "KEY=VALUE" entries (required by CreateProcess).
func buildEnvBlock(env []string) []uint16 {
	type kv struct{ key, value string }
	pairs := make([]kv, 0, len(env))
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			pairs = append(pairs, kv{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		return lessOrdinalIgnoreCase(pairs[i].key, pairs[j].key)
	})

	var block []uint16
	for _, p := range pairs {
		// NOTE: windows.UTF16FromString appends a trailing NUL, so use
		// utf16.Encode directly and add exactly one NUL per entry. Two NULs
		// between entries produce an empty entry in the environment block,
		// which breaks child initialization (cmd.exe exits with 0xC0000142).
		encoded := utf16.Encode([]rune(p.key + "=" + p.value))
		block = append(block, encoded...)
		block = append(block, 0)
	}
	block = append(block, 0) // extra null terminates the block
	return block
}

// lessOrdinalIgnoreCase compares two strings with Windows' OrdinalIgnoreCase
// semantics (ASCII): character by character, folding ASCII letters to upper
// case, keeping the original ordinal values otherwise.
func lessOrdinalIgnoreCase(a, b string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ca, cb := a[i], b[i]
		if ca >= 'a' && ca <= 'z' {
			ca -= 32
		}
		if cb >= 'a' && cb <= 'z' {
			cb -= 32
		}
		if ca != cb {
			return ca < cb
		}
	}
	return len(a) < len(b)
}

// formatCommandLine quotes the application and its arguments for
// CreateProcess, mirroring the C# PtyTerminal behavior: the app is quoted only
// when it contains spaces.
func formatCommandLine(app string, args []string) string {
	var sb strings.Builder
	if strings.ContainsAny(app, " \t") && !strings.HasPrefix(app, "\"") && !strings.HasSuffix(app, "\"") {
		sb.WriteString(quoteWinArg(app))
	} else {
		sb.WriteString(app)
	}
	for _, arg := range args {
		sb.WriteByte(' ')
		sb.WriteString(quoteWinArg(arg))
	}
	return sb.String()
}

func quoteWinArg(arg string) string {
	if arg == "" {
		return `""`
	}
	return `"` + strings.ReplaceAll(arg, `"`, `""`) + `"`
}

// conptyConnection is the ConPTY backend on Windows.
type conptyConnection struct {
	hPC     windows.Handle
	inFile  *os.File // written by us (pty input)
	outFile *os.File // read by us (pty output)
	hProc   windows.Handle
	pid     int
	attrBuf []byte // keeps the attribute list memory alive until CreateProcess is done

	// Pseudo-console side pipe handles; kept open for the lifetime of the
	// session (closing them early breaks the ConPTY host connection).
	ptyInRead   windows.Handle
	ptyOutWrite windows.Handle

	outputCh chan []byte // pre-read pty output (fed by outputPump)
	pending  []byte     // leftover bytes not yet consumed by Read

	readyCh   chan struct{} // closed once the ConPTY host has attached the child
	readyOnce sync.Once
	writeOnce sync.Once

	done     chan struct{}
	waitErr  error
	exitCode int

	once     sync.Once
	closeErr error
}

// statusDllInitFailed is the exit code of a child that crashed while its
// DLLs were initializing. With ConPTY this happens when input is written to
// the pipe before the ConPTY host has attached the child.
const statusDllInitFailed = 0xC0000142

// debugf prints diagnostics when PTYGO_DEBUG is set.
func debugf(format string, args ...interface{}) {
	if os.Getenv("PTYGO_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[ptygo] "+format+"\n", args...)
	}
}

// startConpty spawns a process attached to a ConPTY pseudo console, retrying
// when the child crashes during initialization (a known ConPTY race where
// conhost has not finished attaching the child yet). The first Write blocks
// until the host is ready, so callers can write immediately after Start.
func startConpty(opts Options) (Connection, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		conn, err := tryStartConpty(opts)
		if err != nil {
			lastErr = err
			continue
		}
		c := conn.(*conptyConnection)
		if c.waitReady() {
			return conn, nil
		}
		lastErr = fmt.Errorf("ptygo: child crashed during initialization (0xC0000142); retrying")
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return nil, lastErr
}

// waitReady blocks until the ConPTY host has attached the child. The reliable
// signal is the first byte of output: the host can only deliver output once it
// finished attaching, so writing input afterwards cannot hit the
// initialization race. Falls back to a timeout so silent programs still work.
// The readyCh is closed on success (or timeout) so that the first Write can
// wait for readiness.
func (c *conptyConnection) waitReady() bool {
	defer c.readyOnce.Do(func() { close(c.readyCh) })

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	select {
	case data, ok := <-c.outputCh:
		if ok {
			// Keep the first chunk for Read to consume.
			c.pending = data
			debugf("waitReady: got first output (%d bytes), pid=%d", len(data), c.pid)
			return true
		}
		select {
		case <-c.done:
			debugf("waitReady: outputCh closed, child exited code=%d, pid=%d", c.exitCode, c.pid)
			return false
		case <-time.After(100 * time.Millisecond):
			return false
		}
	case <-c.done:
		debugf("waitReady: child exited code=%d without output, pid=%d", c.exitCode, c.pid)
		return false
	case <-timer.C:
		debugf("waitReady: timeout, pid=%d", c.pid)
		return true
	}
}

// outputPump continuously reads the pty output pipe into outputCh so that
// Read can consume it and the connection can observe the first output byte.
func (c *conptyConnection) outputPump() {
	buf := make([]byte, 8192)
	for {
		n, err := c.outFile.Read(buf)
		if n > 0 {
			debugf("outputPump: read %d bytes", n)
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case c.outputCh <- data:
			case <-c.done:
				// Close the channel so a consumer blocked in Read gets EOF
				// instead of leaking; the err path below is mutually exclusive.
				close(c.outputCh)
				return
			}
		}
		if err != nil {
			debugf("outputPump: read ended err=%v", err)
			close(c.outputCh)
			return
		}
	}
}

func tryStartConpty(opts Options) (Connection, error) {	inRead, inWrite, err := createPipe()
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := createPipe()
	if err != nil {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		return nil, err
	}

	cleanupHandles := func() {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
		_ = windows.CloseHandle(outWrite)
	}

	hPC, err := createPseudoConsole(coord{X: int16(opts.Cols), Y: int16(opts.Rows)}, inRead, outWrite)
	if err != nil {
		cleanupHandles()
		return nil, err
	}

	attrList, attrBuf, err := initializeProcThreadAttributeList(1)
	if err != nil {
		_, _, _ = procClosePseudoConsole.Call(uintptr(hPC))
		cleanupHandles()
		return nil, err
	}

	if err := updateProcThreadAttribute(attrList, procThreadAttributePseudoConsole, uintptr(hPC)); err != nil {
		deleteProcThreadAttributeList(attrList)
		_, _, _ = procClosePseudoConsole.Call(uintptr(hPC))
		cleanupHandles()
		return nil, err
	}

	si := &startupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(startupInfoEx{})),
			Flags: startfUseStdHandles,
		},
		lpAttributeList: attrList,
	}

	cmdLine := formatCommandLine(opts.App, opts.Args)
	envBlock := buildEnvBlock(buildEnv(opts))
	if os.Getenv("PTYGO_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[ptygo] conpty: app=%q args=%v cwd=%q cmdLine=%q envEntries=%d hPC=%v\n",
			opts.App, opts.Args, opts.Cwd, cmdLine, len(buildEnv(opts)), hPC)
	}
	pi, err := createProcess(cmdLine, envBlock, opts.Cwd, si)
	if err != nil {
		deleteProcThreadAttributeList(attrList)
		_, _, _ = procClosePseudoConsole.Call(uintptr(hPC))
		cleanupHandles()
		return nil, err
	}

	// NOTE: the pseudo-console side pipe handles (inRead/outWrite) are kept
	// open for the lifetime of the session and only closed in Close(). Closing
	// them right after CreateProcess breaks the ConPTY host's pipe connection
	// and the child fails to initialize (0xC0000142).
	deleteProcThreadAttributeList(attrList)

	inFile := os.NewFile(uintptr(inWrite), "pty-in")
	outFile := os.NewFile(uintptr(outRead), "pty-out")

	c := &conptyConnection{
		hPC:         hPC,
		inFile:      inFile,
		outFile:     outFile,
		hProc:       windows.Handle(pi.Process),
		pid:         int(pi.ProcessID),
		attrBuf:     attrBuf,
		ptyInRead:   inRead,
		ptyOutWrite: outWrite,
		outputCh:    make(chan []byte, 64),
		readyCh:     make(chan struct{}),
		done:        make(chan struct{}),
	}
	go c.waitLoop()
	go c.outputPump()
	return c, nil
}

func (c *conptyConnection) waitLoop() {
	const infinite = 0xFFFFFFFF
	_, err := windows.WaitForSingleObject(c.hProc, infinite)
	c.waitErr = err
	var code uint32
	if err := windows.GetExitCodeProcess(c.hProc, &code); err == nil {
		c.exitCode = int(code)
	}
	debugf("waitLoop: child pid=%d exited code=%d", c.pid, c.exitCode)
	_ = windows.CloseHandle(c.hProc)
	close(c.done)
}

func (c *conptyConnection) Read(p []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	data, ok := <-c.outputCh
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, data)
	if n < len(data) {
		c.pending = data[n:]
	}
	return n, nil
}

func (c *conptyConnection) Write(p []byte) (int, error) {
	// The first write waits until the ConPTY host has attached the child
	// (waitReady ran during Start). Writing earlier can crash the child with
	// 0xC0000142, so callers may write immediately after Start returns.
	c.writeOnce.Do(func() { <-c.readyCh })
	return c.inFile.Write(p)
}

func (c *conptyConnection) Close() error {
	c.once.Do(func() {
		select {
		case <-c.done:
		default:
			// Do not leave the child running when the session is closed.
			_ = windows.TerminateProcess(c.hProc, 1)
		}
		_, _, _ = procClosePseudoConsole.Call(uintptr(c.hPC))
		// Closing a pipe handle does not cancel a pending synchronous ReadFile
		// on Windows; without this the outputPump goroutine leaks forever.
		_ = windows.CancelIoEx(windows.Handle(c.outFile.Fd()), nil)
		_ = c.inFile.Close()
		_ = c.outFile.Close()
		_ = windows.CloseHandle(c.ptyInRead)
		_ = windows.CloseHandle(c.ptyOutWrite)
	})
	return c.closeErr
}

func (c *conptyConnection) Pid() int {
	return c.pid
}

func (c *conptyConnection) ExitCode() int {
	select {
	case <-c.done:
		return c.exitCode
	default:
		return 0
	}
}

func (c *conptyConnection) Resize(cols, rows int) error {
	// The pseudo console is gone once the child exited; resize would fail (or
	// worse) during teardown.
	select {
	case <-c.done:
		return nil
	default:
	}

	// COORD is passed BY VALUE (see createPseudoConsole).
	packed := uint32(uint16(rows))<<16 | uint32(uint16(cols))
	hr, _, _ := procResizePseudoConsole.Call(uintptr(c.hPC), uintptr(packed))
	if hr != 0 {
		return fmt.Errorf("ResizePseudoConsole failed: HRESULT 0x%08X", uint32(hr))
	}
	return nil
}

func (c *conptyConnection) Kill() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	return windows.TerminateProcess(c.hProc, 1)
}

func (c *conptyConnection) Wait() error {
	<-c.done
	return c.waitErr
}

func (c *conptyConnection) Done() <-chan struct{} {
	return c.done
}
