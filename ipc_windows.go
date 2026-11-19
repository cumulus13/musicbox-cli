//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func ipcPath() string { return fmt.Sprintf(`\\.\pipe\musicbox-mpv-%d`, os.Getpid()) }

func dialIPC(path string) (net.Conn, error) {
	d := 2 * time.Second
	return winio.DialPipe(path, &d)
}

// enableVT turns on ANSI escape handling for the console output and input.
func enableVT() {
	out := windows.Handle(os.Stdout.Fd())
	var m uint32
	if windows.GetConsoleMode(out, &m) == nil {
		_ = windows.SetConsoleMode(out, m|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
	in := windows.Handle(os.Stdin.Fd())
	if windows.GetConsoleMode(in, &m) == nil {
		_ = windows.SetConsoleMode(in, m|windows.ENABLE_VIRTUAL_TERMINAL_INPUT)
	}
}
