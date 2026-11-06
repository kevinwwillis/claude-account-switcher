package main

import (
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func claudeDesktop() desktop {
	home, _ := os.UserHomeDir()
	return desktop{dir: filepath.Join(home, "Library", "Application Support", "Claude")}
}

// The main process is named "Claude"; helpers are "Claude Helper (...)".
func desktopRunning() bool {
	return exec.Command("pgrep", "-x", "Claude").Run() == nil
}

func desktopProcesses() ([]uint32, string) {
	if desktopRunning() {
		return []uint32{0}, ""
	}
	return nil, ""
}

func quitDesktop() error {
	if !desktopRunning() {
		return nil
	}
	_ = exec.Command("osascript", "-e", `tell application "Claude" to quit`).Run()
	if waitGone(8 * time.Second) {
		return nil
	}
	_ = exec.Command("pkill", "-9", "-x", "Claude").Run()
	if waitGone(10 * time.Second) {
		return nil
	}
	return errors.New(tr("Claude did not quit", "Claude 没有退出"))
}

func waitGone(timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if !desktopRunning() {
			time.Sleep(700 * time.Millisecond)
			return true
		}
	}
	return false
}

func startDesktop(string) error {
	return exec.Command("open", "-a", "Claude").Run()
}

// shellOpen opens a folder, or a web page in the default browser.
func shellOpen(target string) { _ = exec.Command("open", target).Start() }

// launch starts the switcher at exe in its own session, so it keeps running
// after this process quits.
func launch(exe string) error {
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// askToQuitOld would tell the user to quit a switcher from before 0.3.3.
// Those were never tested on a Mac, so only the log says it.
func askToQuitOld() {}

func launchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "io.github.snlperstripes.claude-account-switcher.plist")
}

func autostartEnabled() bool {
	_, err := os.Stat(launchAgentPath())
	return err == nil
}

func setAutostart(on bool) error {
	if !on {
		return os.Remove(launchAgentPath())
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>io.github.snlperstripes.claude-account-switcher</string>
	<key>ProgramArguments</key><array><string>%s</string></array>
	<key>RunAtLoad</key><true/>
</dict>
</plist>
`, html.EscapeString(exe))
	if err := os.MkdirAll(filepath.Dir(launchAgentPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(launchAgentPath(), []byte(plist), 0o644)
}

func relaunchOutsideDesktop(string) bool { return false }

// systemLanguages lists the user's languages, preferred first, such as
// "zh-Hans-CN". A login item gets no LANG, so they come from the defaults.
func systemLanguages() []string {
	out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output()
	if err != nil {
		return nil
	}
	return parseAppleLanguages(string(out))
}

// installLauncher adds the switcher to the app list where the platform needs it.
func installLauncher() error { return nil }
