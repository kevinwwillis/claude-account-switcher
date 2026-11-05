//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

// TestSyncSidebarOnCopy runs the sync on a copy of a real data folder, for
// checking a new app version by hand:
//
//	SIDEBAR_COPY=<copy of the data folder> SIDEBAR_SOURCE=<account/org> SIDEBAR_SCOPES=<a/o,b/p,...> go test -run OnCopy
func TestSyncSidebarOnCopy(t *testing.T) {
	dir := os.Getenv("SIDEBAR_COPY")
	if dir == "" {
		t.Skip("SIDEBAR_COPY not set")
	}
	appRunning = func() bool { return false }
	res, err := syncSidebar(desktop{dir: dir}, os.Getenv("SIDEBAR_SOURCE"), strings.Split(os.Getenv("SIDEBAR_SCOPES"), ","), dir+".backups")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", res)
}
