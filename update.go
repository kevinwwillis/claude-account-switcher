package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	repoURL = "https://github.com/SnlperStripes/claude-account-switcher"
	// GitHub is asked for the latest release at most this often.
	updateEvery = 24 * time.Hour
)

var latestAPI = "https://api.github.com/repos/SnlperStripes/claude-account-switcher/releases/latest"

// latestRelease asks GitHub for the version of the latest release, like "0.3.0".
// Nothing about the user or their accounts goes along.
func latestRelease(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "claude-account-switcher/"+version)
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release: %s", res.Status)
	}
	var body struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(body.Tag, "v")
	if parseVersion(v) == nil {
		return "", fmt.Errorf("latest release: unexpected tag %q", body.Tag)
	}
	return v, nil
}

// parseVersion splits "1.2.3" into its numbers, nil when v is not a version.
func parseVersion(v string) []int {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		nums[i] = n
	}
	return nums
}

// newerVersion says whether version a comes after version b.
func newerVersion(a, b string) bool {
	x, y := parseVersion(a), parseVersion(b)
	if x == nil || y == nil {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// checkUpdate asks GitHub for the latest release when the check is on and
// the last answer is a day old. A failed check is tried again next time.
func (s *switcher) checkUpdate(ctx context.Context) {
	s.mu.Lock()
	due := !s.st.NoUpdateCheck && time.Since(s.st.UpdateChecked) >= updateEvery
	s.mu.Unlock()
	if !due {
		return
	}
	v, err := latestRelease(ctx)
	if err != nil {
		log.Printf("update check: %v", err)
		return
	}
	if newerVersion(v, version) {
		log.Printf("version %s is available", v)
	}
	s.mu.Lock()
	s.st.UpdateChecked, s.st.LatestVersion = time.Now(), v
	s.saveState()
	s.mu.Unlock()
	s.onChange()
}

var (
	releasesURL = repoURL + "/releases"
	downloader  = &http.Client{Timeout: 5 * time.Minute}
	updating    atomic.Bool
)

// releaseAsset is the name of this platform's file in a release.
func releaseAsset() string {
	switch runtime.GOOS {
	case "windows":
		return "claude-account-switcher-windows-amd64.exe"
	case "darwin":
		return "claude-account-switcher-macos"
	case "linux":
		return "claude-account-switcher-linux-" + runtime.GOARCH
	}
	return ""
}

// installUpdate puts version v in place of the running file and starts it,
// and the new process then asks this one to quit. A file the switcher
// downloads itself carries no mark of the web, so SmartScreen and Gatekeeper
// stay quiet. When that does not work, the download page opens instead.
func (s *switcher) installUpdate(ctx context.Context, v string) {
	if !updating.CompareAndSwap(false, true) {
		return
	}
	defer updating.Store(false)
	s.setStatus(fmt.Sprintf(tr("Downloading version %s…", "正在下载版本 %s…"), v))
	err := func() error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		bin, err := fetchRelease(ctx, v)
		if err != nil {
			return err
		}
		if err := replaceExe(exe, bin); err != nil {
			return err
		}
		if err := launch(exe); err != nil {
			restoreExe(exe)
			return err
		}
		return nil
	}()
	if err != nil {
		s.setStatus(tr("Update failed: ", "更新失败：") + err.Error())
		shellOpen(releasesURL + "/latest")
		return
	}
	s.setStatus(fmt.Sprintf(tr("Starting version %s…", "正在启动版本 %s…"), v))
}

// fetchRelease downloads this platform's file of version v and checks it
// against the checksum published next to it.
func fetchRelease(ctx context.Context, v string) ([]byte, error) {
	asset := releaseAsset()
	if asset == "" {
		return nil, errors.New("no release build for this platform")
	}
	url := releasesURL + "/download/v" + v + "/" + asset
	bin, err := download(ctx, url, 64<<20)
	if err != nil {
		return nil, err
	}
	sum, err := download(ctx, url+".sha256", 1<<10)
	if err != nil {
		return nil, err
	}
	want, _, _ := strings.Cut(strings.TrimSpace(string(sum)), " ")
	got := sha256.Sum256(bin)
	if !strings.EqualFold(want, hex.EncodeToString(got[:])) {
		return nil, errors.New(tr("the download does not match its checksum", "下载的文件与校验值不符"))
	}
	return bin, nil
}

func download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "claude-account-switcher/"+version)
	res, err := downloader.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s", res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("download: file too large")
	}
	return data, nil
}

// replaceExe writes bin in place of exe and keeps the old file as exe.old.
// Windows cannot overwrite a running program, but it can rename it.
func replaceExe(exe string, bin []byte) error {
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	_ = os.Remove(exe + ".old")
	if err := os.Rename(exe, exe+".old"); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		restoreExe(exe)
		os.Remove(tmp)
		return err
	}
	return nil
}

// restoreExe puts exe.old back when the new file cannot be started.
func restoreExe(exe string) {
	_ = os.Remove(exe)
	if err := os.Rename(exe+".old", exe); err != nil {
		log.Printf("restore %s: %v", exe, err)
	}
}

// removeReplaced deletes the file an update left behind once the old process,
// which still runs from it for a moment, has quit.
func removeReplaced() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return
	}
	old := exe + ".old"
	for range 20 {
		if err := os.Remove(old); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}
