package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Anthropic ships Claude Desktop for Linux as a .deb from its own apt
// repository. It installs /usr/lib/claude-desktop/claude-desktop, linked as
// /usr/bin/claude-desktop, and keeps its data in ~/.config/Claude.

const desktopName = "claude-desktop"

func claudeDesktop() desktop {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return desktop{dir: filepath.Join(base, "Claude")}
}

// desktopProcesses returns the app's processes, the main one first, and its
// executable. Electron's helpers carry the same name with a --type= argument.
func desktopProcesses() (pids []uint32, exe string) {
	entries, _ := os.ReadDir("/proc")
	var helpers []uint32
	for _, e := range entries {
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		path, err := os.Readlink(filepath.Join(dir, "exe"))
		if err != nil || filepath.Base(path) != desktopName {
			continue
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "resources", "app.asar")); err != nil {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		if strings.Contains(string(cmdline), "--type=") {
			helpers = append(helpers, uint32(pid))
			continue
		}
		pids = append(pids, uint32(pid))
		exe = path
	}
	return append(pids, helpers...), exe
}

func desktopRunning() bool {
	pids, _ := desktopProcesses()
	return len(pids) > 0
}

// quitDesktop asks the app to close the way the desktop does on logout,
// with SIGTERM to its main process, then ends what is left.
func quitDesktop() error {
	pids, _ := desktopProcesses()
	if len(pids) == 0 {
		return nil
	}
	began := time.Now()
	_ = syscall.Kill(int(pids[0]), syscall.SIGTERM)
	if waitGone(15 * time.Second) {
		log.Printf("Claude closed after %s", time.Since(began).Round(100*time.Millisecond))
		return nil
	}
	pids, _ = desktopProcesses()
	for _, p := range pids {
		_ = syscall.Kill(int(p), syscall.SIGKILL)
	}
	if waitGone(10 * time.Second) {
		log.Printf("Claude did not close on request, ended it after %s", time.Since(began).Round(100*time.Millisecond))
		return nil
	}
	return errors.New(tr("Claude did not quit", "Claude 没有退出"))
}

func waitGone(timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if !desktopRunning() {
			// Give the app's files a moment to be released.
			time.Sleep(700 * time.Millisecond)
			return true
		}
	}
	return false
}

// startDesktop starts the app through the user's systemd, like the dock
// does, never as a child of the switcher. A child would inherit the
// switcher's process flags (a no_new_privs switcher would leave sudo
// broken in every Claude terminal) and its session.
func startDesktop(exe string) error {
	if exe == "" {
		if p, err := exec.LookPath(desktopName); err == nil {
			exe = p
		} else {
			return errors.New(tr("could not find the Claude app", "找不到 Claude 应用"))
		}
	}
	return runDetached(fmt.Sprintf("app-com.anthropic.Claude-switcher-%d", time.Now().Unix()), exe)
}

// runDetached runs a program as a transient systemd user service, so it
// belongs to no process of ours. Without systemd it falls back to a new
// session.
func runDetached(unit, exe string, args ...string) error {
	if path, err := exec.LookPath("systemd-run"); err == nil {
		cmdArgs := append([]string{"--user", "--collect", "--quiet", "--slice=app.slice", "--unit=" + unit,
			"-p", "Type=exec", "--", exe}, args...)
		if out, err := exec.Command(path, cmdArgs...).CombinedOutput(); err == nil {
			return nil
		} else {
			log.Printf("systemd-run %s: %v %s", unit, err, strings.TrimSpace(string(out)))
		}
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// shellOpen opens a folder, or a web page in the default browser.
func shellOpen(target string) { _ = exec.Command("xdg-open", target).Start() }

// launch starts the switcher at exe on its own, so it keeps running after
// this process quits.
func launch(exe string) error {
	return runDetached(fmt.Sprintf("claude-account-switcher-%d", time.Now().UnixNano()), exe)
}

// askToQuitOld would tell the user to quit a switcher from before 0.3.3.
// Those never ran on Linux.
func askToQuitOld() {}

// insideDesktop reports whether one of this process's ancestors is the app,
// for example when the switcher was started from a Claude Code terminal.
// Quitting the app would end such a switcher halfway through a switch.
func insideDesktop() bool {
	for pid := os.Getppid(); pid > 1; {
		dir := filepath.Join("/proc", strconv.Itoa(pid))
		if path, err := os.Readlink(filepath.Join(dir, "exe")); err == nil && filepath.Base(path) == desktopName {
			return true
		}
		status, err := os.ReadFile(filepath.Join(dir, "status"))
		if err != nil {
			return false
		}
		next := 0
		for _, line := range strings.Split(string(status), "\n") {
			if v, ok := strings.CutPrefix(line, "PPid:"); ok {
				next, _ = strconv.Atoi(strings.TrimSpace(v))
				break
			}
		}
		pid = next
	}
	return false
}

// relaunchOutsideDesktop starts the switcher again outside the app when it
// runs inside it. A marker file stops a relaunch loop.
func relaunchOutsideDesktop(dataDir string) bool {
	if !insideDesktop() {
		return false
	}
	marker := filepath.Join(dataDir, "relaunched")
	if info, err := os.Stat(marker); err == nil && time.Since(info.ModTime()) < 30*time.Second {
		return false
	}
	exe, err := os.Executable()
	if err != nil || os.WriteFile(marker, nil, 0o600) != nil {
		return false
	}
	return launch(exe) == nil
}

func autostartPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "autostart", "claude-account-switcher.desktop")
}

func autostartEnabled() bool {
	_, err := os.Stat(autostartPath())
	return err == nil
}

func setAutostart(on bool) error {
	if !on {
		err := os.Remove(autostartPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	entry := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=Claude Account Switcher\nExec=%s\nX-GNOME-Autostart-enabled=true\nNoDisplay=true\n", desktopExecQuote(exe))
	if err := os.MkdirAll(filepath.Dir(autostartPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(autostartPath(), []byte(entry), 0o644)
}

// desktopExecQuote quotes a path for an Exec= line of a .desktop file.
func desktopExecQuote(s string) string {
	if !strings.ContainsAny(s, " \t\n\"'\\`$;&|<>()*?#~=%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", `$`, `\\$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

// systemLanguages lists the user's languages, preferred first, from the
// usual locale variables, such as "zh_CN.UTF-8".
func systemLanguages() []string {
	var langs []string
	for _, v := range []string{"LANGUAGE", "LC_ALL", "LC_MESSAGES", "LANG"} {
		for _, l := range strings.Split(os.Getenv(v), ":") {
			if l != "" && l != "C" && l != "POSIX" {
				langs = append(langs, l)
			}
		}
	}
	return langs
}
