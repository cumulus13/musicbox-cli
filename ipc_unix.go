//go:build !windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

func ipcPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("musicbox-mpv-%d.sock", os.Getpid()))
}

func dialIPC(path string) (net.Conn, error) { return net.Dial("unix", path) }

// enableVT is only needed on Windows consoles.
func enableVT() {}
