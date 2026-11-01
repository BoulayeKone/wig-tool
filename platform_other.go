//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func openWindow(url string)                     { fmt.Println("Open", url) }
func openExplorer(dir, selectFile string) error { fmt.Println("explorer", dir, selectFile); return nil }
func showError(msg string)                      { fmt.Fprintln(os.Stderr, msg) }
func hidden(cmd *exec.Cmd) *exec.Cmd            { return cmd }
func brandWindows()                             {}
