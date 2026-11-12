//go:build windows

package ptygo

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winpty DLL bindings. The DLLs (winpty.dll + winpty-agent.exe +
// winpty-debugserver.exe) live in ptygo/deps/winpty and are the same binaries
// shipped by the C# PtyTerminal project. The location can be overridden with
// the PTYGO_WINPTY_DLL environment variable.
const (
	winptyFlagColorEscapes        = 0x4
	winptySpawnFlagAutoShutdown   = 1
)

var winptyProcs struct {
	once sync.Once
	err  error

	errorCode         *windows.LazyProc
	errorMsg          *windows.LazyProc
	errorFree         *windows.LazyProc
	configNew         *windows.LazyProc
	configFree        *windows.LazyProc
	configSetInitSize *windows.LazyProc
	open              *windows.LazyProc
	coninName         *windows.LazyProc
	conoutName        *windows.LazyProc
	spawnConfigNew    *windows.LazyProc
	spawnConfigFree   *windows.LazyProc
	spawn             *windows.LazyProc
	setSize           *windows.LazyProc
	free              *windows.LazyProc
}

func loadWinpty() error {
	winptyProcs.once.Do(func() {
		path, err := findWinptyDll()
		if err != nil {
			winptyProcs.err = err
			return
		}
		dll := windows.NewLazyDLL(path)

		procs := map[string]**windows.LazyProc{
			"winpty_error_code":            &winptyProcs.errorCode,
			"winpty_error_msg":             &winptyProcs.errorMsg,
			"winpty_error_free":            &winptyProcs.errorFree,
			"winpty_config_new":            &winptyProcs.configNew,
			"winpty_config_free":           &winptyProcs.configFree,
			"winpty_config_set_initial_size": &winptyProcs.configSetInitSize,
			"winpty_open":                  &winptyProcs.open,
			"winpty_conin_name":            &winptyProcs.coninName,
			"winpty_conout_name":           &winptyProcs.conoutName,
			"winpty_spawn_config_new":      &winptyProcs.spawnConfigNew,
			"winpty_spawn_config_free":     &winptyProcs.spawnConfigFree,
			"winpty_spawn":                 &winptyProcs.spawn,
			"winpty_set_size":              &winptyProcs.setSize,
			"winpty_free":                  &winptyProcs.free,
		}
		for name, slot := range procs {
			p := dll.NewProc(name)
			if err := p.Find(); err != nil {
				winptyProcs.err = fmt.Errorf("winpty: %s not found in %s: %w", name, path, err)
				return
			}
			*slot = p
		}
	})
	return winptyProcs.err
}

// findWinptyDll locates winpty.dll. Search order:
//  1. PTYGO_WINPTY_DLL environment variable
//  2. <exe dir>/deps/winpty/winpty.dll
//  3. <exe dir>/winpty.dll
//  4. ./deps/winpty/winpty.dll (relative to the working directory)
func findWinptyDll() (string, error) {
	if p := os.Getenv("PTYGO_WINPTY_DLL"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("winpty: PTYGO_WINPTY_DLL not found: %w", err)
		}
		return p, nil
	}

	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		candidates := []string{
			filepath.Join(exeDir, "deps", "winpty", "winpty.dll"),
			filepath.Join(exeDir, "winpty.dll"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}

	cwd, err := os.Getwd()
	if err == nil {
		c := filepath.Join(cwd, "deps", "winpty", "winpty.dll")
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}

	return "", fmt.Errorf("winpty: winpty.dll not found; place deps/winpty next to the executable or set PTYGO_WINPTY_DLL")
}

func winptyError(what string, errPtr uintptr) error {
	if errPtr == 0 {
		return fmt.Errorf("winpty: %s failed", what)
	}
	msgPtr, _, _ := winptyProcs.errorMsg.Call(errPtr)
	code, _, _ := winptyProcs.errorCode.Call(errPtr)
	_, _, _ = winptyProcs.errorFree.Call(errPtr)
	msg := ""
	if msgPtr != 0 {
		msg = windows.UTF16PtrToString(ptrToUTF16(msgPtr))
	}
	return fmt.Errorf("winpty: %s failed: %s (%d)", what, msg, uint32(code))
}

// winptyConnection is the winpty backend on Windows.
type winptyConnection struct {
	wp     uintptr // winpty agent handle
	hProc  windows.Handle
	inFile *os.File  // written by us (conin pipe)
	outFile *os.File // read by us (conout pipe)
	proc   *os.Process

	done     chan struct{}
	waitErr  error
	exitCode int

	once     sync.Once
	closeErr error
}

func startWinpty(opts Options) (Connection, error) {
	if err := loadWinpty(); err != nil {
		return nil, err
	}

	var errPtr uintptr
	cfg, _, _ := winptyProcs.configNew.Call(winptyFlagColorEscapes, uintptr(unsafe.Pointer(&errPtr)))
	if cfg == 0 {
		return nil, winptyError("winpty_config_new", errPtr)
	}
	_, _, _ = winptyProcs.configSetInitSize.Call(cfg, uintptr(int32(opts.Cols)), uintptr(int32(opts.Rows)))

	wp, _, _ := winptyProcs.open.Call(cfg, uintptr(unsafe.Pointer(&errPtr)))
	_, _, _ = winptyProcs.configFree.Call(cfg)
	if wp == 0 {
		return nil, winptyError("winpty_open", errPtr)
	}

	appPtr, err := windows.UTF16PtrFromString(opts.App)
	if err != nil {
		_, _, _ = winptyProcs.free.Call(wp)
		return nil, err
	}
	cmdLinePtr, err := windows.UTF16PtrFromString(formatCommandLine(opts.App, opts.Args))
	if err != nil {
		_, _, _ = winptyProcs.free.Call(wp)
		return nil, err
	}
	cwdPtr, err := windows.UTF16PtrFromString(opts.Cwd)
	if err != nil {
		_, _, _ = winptyProcs.free.Call(wp)
		return nil, err
	}
	envBlock := buildEnvBlock(buildEnv(opts))
	var envPtr *uint16
	if len(envBlock) > 0 {
		envPtr = &envBlock[0]
	}

	spawnCfg, _, _ := winptyProcs.spawnConfigNew.Call(
		winptySpawnFlagAutoShutdown,
		uintptr(unsafe.Pointer(appPtr)),
		uintptr(unsafe.Pointer(cmdLinePtr)),
		uintptr(unsafe.Pointer(cwdPtr)),
		uintptr(unsafe.Pointer(envPtr)),
		uintptr(unsafe.Pointer(&errPtr)),
	)
	if spawnCfg == 0 {
		_, _, _ = winptyProcs.free.Call(wp)
		return nil, winptyError("winpty_spawn_config_new", errPtr)
	}

	var hProcess, hThread uintptr
	var procError uint32
	ok, _, _ := winptyProcs.spawn.Call(
		wp,
		spawnCfg,
		uintptr(unsafe.Pointer(&hProcess)),
		uintptr(unsafe.Pointer(&hThread)),
		uintptr(unsafe.Pointer(&procError)),
		uintptr(unsafe.Pointer(&errPtr)),
	)
	_, _, _ = winptyProcs.spawnConfigFree.Call(spawnCfg)
	if ok == 0 {
		_, _, _ = winptyProcs.free.Call(wp)
		if procError != 0 {
			return nil, fmt.Errorf("winpty: spawn failed: %w (%d)", windows.Errno(procError), procError)
		}
		return nil, winptyError("winpty_spawn", errPtr)
	}

	coninPtr, _, _ := winptyProcs.coninName.Call(wp)
	conoutPtr, _, _ := winptyProcs.conoutName.Call(wp)

	cleanup := func() {
		_ = windows.CloseHandle(windows.Handle(hProcess))
		_ = windows.CloseHandle(windows.Handle(hThread))
		_, _, _ = winptyProcs.free.Call(wp)
	}

	inFile, err := openWinptyPipe(coninPtr, windows.GENERIC_WRITE)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("winpty: open conin pipe failed: %w", err)
	}
	outFile, err := openWinptyPipe(conoutPtr, windows.GENERIC_READ)
	if err != nil {
		_ = inFile.Close()
		cleanup()
		return nil, fmt.Errorf("winpty: open conout pipe failed: %w", err)
	}

	pid, err := windows.GetProcessId(windows.Handle(hProcess))
	if err != nil {
		_ = inFile.Close()
		cleanup()
		return nil, fmt.Errorf("winpty: GetProcessId failed: %w", err)
	}
	proc, err := os.FindProcess(int(pid))
	if err != nil {
		_ = inFile.Close()
		cleanup()
		return nil, err
	}

	c := &winptyConnection{
		wp:      wp,
		hProc:   windows.Handle(hProcess),
		inFile:  inFile,
		outFile: outFile,
		proc:    proc,
		done:    make(chan struct{}),
	}
	go c.waitLoop()
	return c, nil
}

// ptrToUTF16 converts a native LPWSTR value (returned as uintptr by a proc
// call) back into a *uint16. go vet flags direct uintptr->unsafe.Pointer
// conversions, so the pointer value is moved through memory instead.
func ptrToUTF16(p uintptr) *uint16 {
	var ptr unsafe.Pointer
	*(*uintptr)(unsafe.Pointer(&ptr)) = p
	return (*uint16)(ptr)
}

// openWinptyPipe opens one of the winpty named pipes (conin/conout).
func openWinptyPipe(namePtr uintptr, access uint32) (*os.File, error) {
	if namePtr == 0 {
		return nil, fmt.Errorf("winpty: null pipe name")
	}
	name := windows.UTF16PtrToString(ptrToUTF16(namePtr))
	namePtr16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		namePtr16,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}

func (c *winptyConnection) waitLoop() {
	const infinite = 0xFFFFFFFF
	_, err := windows.WaitForSingleObject(c.hProc, infinite)
	c.waitErr = err
	var code uint32
	if err := windows.GetExitCodeProcess(c.hProc, &code); err == nil {
		c.exitCode = int(code)
	}
	_ = windows.CloseHandle(c.hProc)
	close(c.done)
}

func (c *winptyConnection) Read(p []byte) (int, error) {
	return c.outFile.Read(p)
}

func (c *winptyConnection) Write(p []byte) (int, error) {
	return c.inFile.Write(p)
}

func (c *winptyConnection) Close() error {
	c.once.Do(func() {
		select {
		case <-c.done:
		default:
			_ = windows.TerminateProcess(c.hProc, 1)
		}
		// Closing a pipe handle does not cancel a pending synchronous ReadFile
		// on Windows; without this a reader blocked in Read leaks forever.
		_ = windows.CancelIoEx(windows.Handle(c.outFile.Fd()), nil)
		_ = c.inFile.Close()
		_ = c.outFile.Close()
		_, _, _ = winptyProcs.free.Call(c.wp)
	})
	return c.closeErr
}

func (c *winptyConnection) Pid() int {
	return c.proc.Pid
}

func (c *winptyConnection) ExitCode() int {
	select {
	case <-c.done:
		return c.exitCode
	default:
		return 0
	}
}

func (c *winptyConnection) Resize(cols, rows int) error {
	// winpty_set_size crashes (access violation) once the agent has exited;
	// the child may still be tearing down while resize requests arrive.
	select {
	case <-c.done:
		return nil
	default:
	}
	if st, err := windows.WaitForSingleObject(c.hProc, 0); err == nil && st == uint32(windows.WAIT_OBJECT_0) {
		// Child process already exited.
		return nil
	}

	var errPtr uintptr
	ok, _, _ := winptyProcs.setSize.Call(c.wp, uintptr(int32(cols)), uintptr(int32(rows)), uintptr(unsafe.Pointer(&errPtr)))
	if ok == 0 {
		return winptyError("winpty_set_size", errPtr)
	}
	return nil
}

func (c *winptyConnection) Kill() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	return windows.TerminateProcess(c.hProc, 1)
}

func (c *winptyConnection) Wait() error {
	<-c.done
	return c.waitErr
}

func (c *winptyConnection) Done() <-chan struct{} {
	return c.done
}
