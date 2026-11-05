//go:build !windows && !darwin && !linux

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// There is no official Claude desktop app for this platform. These stubs keep
// the project building so the shared code can be tested here.

var errUnsupported = errors.New("the Claude desktop app is not supported on this platform")

func claudeDesktop() desktop {
	home, _ := os.UserHomeDir()
	return desktop{dir: filepath.Join(home, ".config", "Claude")}
}

func desktopRunning() bool                 { return false }
func desktopProcesses() ([]uint32, string) { return nil, "" }
func quitDesktop() error                   { return nil }
func startDesktop(string) error            { return errUnsupported }
func shellOpen(string)                     {}
func autostartEnabled() bool               { return false }
func setAutostart(bool) error              { return errUnsupported }
func decryptValue([]byte) ([]byte, error)  { return nil, errUnsupported }

func relaunchOutsideDesktop(string) bool { return false }
func launch(string) error                { return errUnsupported }
func askToQuitOld()                      {}

func nativeResume(titles []string, _ time.Duration) []bool {
	keep := make([]bool, len(titles))
	for i := range keep {
		keep[i] = true
	}
	return keep
}

func nativeAskNumber(string, string, string) (string, bool) { return "", false }

func showWebDialog(dialogWindow, string) (json.RawMessage, error) { return nil, errUnsupported }

func systemLanguages() []string { return []string{os.Getenv("LANG")} }
