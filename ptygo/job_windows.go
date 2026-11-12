//go:build windows

package ptygo

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// newProcessJob assigns an already-created process to a fresh Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so Kill/Close can terminate the whole
// process tree via TerminateJobObject, not just the direct child. Best effort:
// returns 0 when the job cannot be created (callers fall back to
// TerminateProcess), because spawning must not fail in restricted job
// environments.
func newProcessJob(hProc windows.Handle) windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	if err := windows.AssignProcessToJobObject(job, hProc); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	return job
}

func terminateJob(job windows.Handle) {
	if job == 0 {
		return
	}
	_ = windows.TerminateJobObject(job, 1)
}

func closeJob(job windows.Handle) {
	if job == 0 {
		return
	}
	_ = windows.CloseHandle(job)
}