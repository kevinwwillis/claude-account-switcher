//go:build !linux

package main

import "errors"

// The sidebar sync is built for Linux only so far: it needs the LevelDB
// library, which the Windows and macOS builds do not ship.

func openLocalStorage(string, bool) (lsDB, error) {
	return nil, errors.New("the sidebar sync is only available on Linux so far")
}
