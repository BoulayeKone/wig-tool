//go:build windows

package main

// The UI runs in an Edge app window, which Windows would group under Edge's
// taskbar icon. Here we give that window our own taskbar identity and icon.

import (
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")
	pEnumWin = user32.NewProc("EnumWindows")
	pGetText = user32.NewProc("GetWindowTextW")
	pGetCls  = user32.NewProc("GetClassNameW")
	pVisible = user32.NewProc("IsWindowVisible")
	pSendMsg = user32.NewProc("SendMessageW")
	pExtract = shell32.NewProc("ExtractIconExW")
	pPropSt  = shell32.NewProc("SHGetPropertyStoreForWindow")
	pCoInit  = ole32.NewProc("CoInitializeEx")
)

type guid struct {
	D1 uint32
	D2 uint16
	D3 uint16
	D4 [8]byte
}

type propKey struct {
	Fmt guid
	Pid uint32
}

type propVariant struct {
	Vt       uint16
	r1, r2   uint16
	r3       uint16
	Val      uintptr
	_padding uintptr
}

var (
	iidPropertyStore = guid{0x886D8EEB, 0x8CF2, 0x4446, [8]byte{0x8D, 0x02, 0xCD, 0xBA, 0x1D, 0xBD, 0xCF, 0x99}}
	appModelFmt      = guid{0x9F4C2855, 0x9F79, 0x4B39, [8]byte{0xA8, 0xD0, 0xE1, 0xD4, 0x2D, 0xE1, 0xD5, 0xF3}}
	keyRelaunchCmd   = propKey{appModelFmt, 2}
	keyRelaunchIcon  = propKey{appModelFmt, 3}
	keyRelaunchName  = propKey{appModelFmt, 4}
	keyAppID         = propKey{appModelFmt, 5}
)

const appUserModelID = "BlayUGC.WigTool"

func windowText(proc *syscall.LazyProc, hwnd uintptr) string {
	buf := make([]uint16, 256)
	proc.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

// Created once: Windows callbacks made with NewCallback are never freed.
var (
	found  []uintptr
	enumCB = syscall.NewCallback(enumProc)
)

func enumProc(hwnd, _ uintptr) uintptr {
	if v, _, _ := pVisible.Call(hwnd); v == 0 {
		return 1
	}
	if !strings.HasPrefix(windowText(pGetCls, hwnd), "Chrome_WidgetWin") {
		return 1
	}
	if strings.HasPrefix(windowText(pGetText, hwnd), appName) {
		found = append(found, hwnd)
	}
	return 1
}

func findAppWindows() []uintptr {
	found = found[:0]
	pEnumWin.Call(enumCB, 0)
	return append([]uintptr(nil), found...)
}

func setStringProp(store unsafe.Pointer, key *propKey, val string) {
	s, _ := syscall.UTF16PtrFromString(val)
	pv := propVariant{Vt: 31 /* VT_LPWSTR */, Val: uintptr(unsafe.Pointer(s))}
	vtbl := *(*[8]uintptr)(*(*unsafe.Pointer)(store))
	syscall.SyscallN(vtbl[6], uintptr(store), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&pv))) // SetValue
	runtime.KeepAlive(s)
}

func tagWindow(hwnd uintptr, exe string) {
	var store unsafe.Pointer
	r, _, _ := pPropSt.Call(hwnd, uintptr(unsafe.Pointer(&iidPropertyStore)), uintptr(unsafe.Pointer(&store)))
	if r != 0 || store == nil {
		return
	}
	vtbl := *(*[8]uintptr)(*(*unsafe.Pointer)(store))
	setStringProp(store, &keyRelaunchCmd, `"`+exe+`"`)
	setStringProp(store, &keyRelaunchIcon, exe+",0")
	setStringProp(store, &keyRelaunchName, appName)
	setStringProp(store, &keyAppID, appUserModelID)
	syscall.SyscallN(vtbl[7], uintptr(store)) // Commit
	syscall.SyscallN(vtbl[2], uintptr(store)) // Release
}

// brandWindows runs in the background and keeps our icon on the app window(s).
func brandWindows() {
	runtime.LockOSThread()
	pCoInit.Call(0, 2 /* COINIT_APARTMENTTHREADED */)

	exe, err := os.Executable()
	if err != nil {
		return
	}
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	var big, small uintptr
	pExtract.Call(uintptr(unsafe.Pointer(exePtr)), 0, uintptr(unsafe.Pointer(&big)), uintptr(unsafe.Pointer(&small)), 1)

	tagged := map[uintptr]bool{}
	for {
		for _, hwnd := range findAppWindows() {
			if !tagged[hwnd] {
				tagWindow(hwnd, exe)
				tagged[hwnd] = true
			}
			if big != 0 {
				pSendMsg.Call(hwnd, 0x80 /* WM_SETICON */, 1, big)
			}
			if small != 0 {
				pSendMsg.Call(hwnd, 0x80, 0, small)
			}
		}
		time.Sleep(2 * time.Second)
	}
}
