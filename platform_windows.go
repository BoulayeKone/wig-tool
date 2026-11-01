//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

func findEdge() string {
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		p := filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

// openWindow shows the UI as its own app window (Edge app mode, no tabs/address bar).
func openWindow(url string) {
	if edge := findEdge(); edge != "" {
		if hidden(exec.Command(edge, "--app="+url, "--window-size=1280,860")).Start() == nil {
			return
		}
	}
	hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", url)).Start()
}

func openExplorer(dir, selectFile string) error {
	if selectFile != "" {
		return exec.Command("explorer.exe", "/select,", selectFile).Start()
	}
	return exec.Command("explorer.exe", dir).Start()
}

func showError(msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	box := user32.NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(appName)
	m, _ := syscall.UTF16PtrFromString(msg)
	box.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
}

// recycle moves files to the Recycle Bin (recoverable), not a permanent delete.
func recycle(paths []string) error {
	var from []uint16
	for _, p := range paths {
		u, err := syscall.UTF16FromString(p)
		if err != nil {
			return err
		}
		from = append(from, u...) // includes the terminating 0
	}
	from = append(from, 0) // list ends with a double 0
	op := struct {
		hwnd                  uintptr
		wFunc                 uint32
		pFrom                 *uint16
		pTo                   *uint16
		fFlags                uint16
		fAnyOperationsAborted int32
		hNameMappings         uintptr
		lpszProgressTitle     *uint16
	}{
		wFunc:  3, // FO_DELETE
		pFrom:  &from[0],
		fFlags: 0x40 | 0x10 | 0x4 | 0x400, // ALLOWUNDO | NOCONFIRMATION | SILENT | NOERRORUI
	}
	r, _, _ := syscall.NewLazyDLL("shell32.dll").NewProc("SHFileOperationW").Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return fmt.Errorf("could not move to Recycle Bin (code %d)", r)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("delete was cancelled")
	}
	return nil
}
