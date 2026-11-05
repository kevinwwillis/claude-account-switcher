package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// On Linux the dialogs are zenity's, which GNOME and most desktops ship.
// There is no web view, so the web dialogs fall back to these.

func showWebDialog(dialogWindow, string) (json.RawMessage, error) {
	return nil, errors.New("no web view on Linux")
}

// nativeAskNumber asks for a number under prompt, with value filled in.
func nativeAskNumber(title, prompt, value string) (string, bool) {
	out, err := exec.Command("zenity", "--entry", "--title="+title, "--text="+prompt, "--entry-text="+value).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// nativeResume asks which of the chats titled titles should go on. After
// wait without an answer, or without zenity, all of them go on.
func nativeResume(titles []string, wait time.Duration) []bool {
	keep := make([]bool, len(titles))
	for i := range keep {
		keep[i] = true
	}
	args := []string{"--list", "--checklist", "--title=" + tr("Continue chats?", "继续聊天？"),
		"--text=" + tr("These chats were working when Claude closed for the switch. Claude continues the selected ones when it starts again.",
			"切换时 Claude 关闭，这些聊天正在进行中。Claude 再次启动后会继续选中的聊天。"),
		"--ok-label=" + tr("Continue", "继续"), "--cancel-label=" + tr("Don't continue", "不继续"),
		"--timeout=" + strconv.Itoa(max(1, int(wait/time.Second))),
		"--column=", "--column=#", "--column=" + tr("Chat", "聊天"), "--hide-column=2", "--print-column=2", "--separator=\n"}
	// The rows go on stdin, one value per line, so a title cannot be read
	// as an option.
	var rows strings.Builder
	for i, t := range titles {
		t = strings.ReplaceAll(strings.ReplaceAll(t, "\r", " "), "\n", " ")
		fmt.Fprintf(&rows, "TRUE\n%d\n%s\n", i+1, t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "zenity", args...)
	cmd.Stdin = strings.NewReader(rows.String())
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return keep
	case errors.As(err, &exit) && exit.ExitCode() == 5: // zenity's timeout
		return keep
	case errors.As(err, &exit) && exit.ExitCode() == 1: // Don't continue, or closed
		return make([]bool, len(titles))
	case err != nil:
		return keep
	}
	keep = make([]bool, len(titles))
	for _, line := range strings.Split(string(out), "\n") {
		if i, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && i >= 1 && i <= len(titles) {
			keep[i-1] = true
		}
	}
	return keep
}
