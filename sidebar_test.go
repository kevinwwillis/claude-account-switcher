//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	scopeA = "aaaa/orgA"
	scopeB = "bbbb/orgB"
	scopeC = "cccc/orgC"
)

func layout(ids map[string]string, assign map[string]string) (map[string]any, map[string]any) {
	var groups, sections []any
	order := map[string]any{}
	for _, name := range []string{"Alpha", "Beta"} {
		id, ok := ids[name]
		if !ok {
			continue
		}
		groups = append(groups, map[string]any{"id": id, "name": name, "mode": "code"})
		sections = append(sections, map[string]any{"id": id, "kind": "manual", "name": name, "collapsed": false})
		order[id] = []any{}
	}
	sections = append([]any{map[string]any{"id": "pinned", "kind": "pinned"}}, sections...)
	a := map[string]any{}
	for chat, name := range assign {
		a["code:"+chat] = ids[name]
	}
	return map[string]any{"groups": groups, "assignments": a, "order": order},
		map[string]any{"sections": sections, "sortBy": "recency"}
}

// fakeApp builds a data folder with three accounts. A has folders Alpha and
// Beta with two chats filed; B has only Alpha under its own id; C has none.
func fakeApp(t *testing.T) desktop {
	t.Helper()
	d := desktop{dir: t.TempDir()}
	gA, sA := layout(map[string]string{"Alpha": "cg-a1", "Beta": "cg-a2"}, map[string]string{"local_1": "Alpha", "local_2": "Beta"})
	gB, sB := layout(map[string]string{"Alpha": "cg-b1"}, nil)
	gC, sC := layout(map[string]string{}, nil)
	state := map[string]any{
		"customGroupsByScope": map[string]any{scopeA: gA, scopeB: gB, scopeC: gC},
		"codeSidebarByScope":  map[string]any{scopeA: sA, scopeB: sB, scopeC: sC},
		"pinnedOrder":         []any{"code:local_1"},
		"big":                 json.Number("1793727383050123456"),
		"note":                "chinese 中文 survives",
	}
	must(t, os.MkdirAll(d.localStorageDir(), 0o700))
	db, err := openLocalStorage(d.localStorageDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	// The persisted copies hold the same layout, with empty member lists, as
	// the app keeps them.
	pg, ps := deepCopy(state["customGroupsByScope"]), deepCopy(state["codeSidebarByScope"]).(map[string]any)
	for _, sc := range ps {
		sc.(map[string]any)["migratedFrom"] = "persisted"
	}
	must(t, lsPut(db, map[string]any{lsStore: map[string]any{"state": state, "version": json.Number("0")},
		lsGroups: map[string]any{"value": pg, "tabId": "t1"}, lsSections: map[string]any{"value": ps, "tabId": "t1"}}))
	must(t, db.write(map[string][]byte{lsPrefix + "unrelated": {1, 'x'}}))
	must(t, db.close())
	prefs := map[string]any{"preferences": map[string]any{"other": 1, "epitaxyPrefs": map[string]any{"keep": true,
		"dframe-group-scopes": state["customGroupsByScope"], "dframe-code-sections": state["codeSidebarByScope"]}}, "x": 2}
	data, _ := json.Marshal(prefs)
	must(t, os.WriteFile(d.prefsPath(), data, 0o600))
	for _, scope := range []string{scopeA, scopeB, scopeC} {
		dir := filepath.Join(d.sessionsDir(), filepath.FromSlash(scope))
		must(t, os.MkdirAll(dir, 0o700))
		star := scope == scopeA
		for _, id := range []string{"local_1", "local_2"} {
			e := map[string]any{"sessionId": id, "title": "t", "isStarred": star && id == "local_1", "lastActivityAt": 5}
			data, _ := json.Marshal(e)
			must(t, os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600))
		}
	}
	return d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readState(t *testing.T, d desktop) map[string]any {
	t.Helper()
	db, err := openLocalStorage(d.localStorageDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	store, err := lsGet(db, lsStore)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := db.get([]byte(lsPrefix + "unrelated")); err != nil || string(v) != "\x01x" {
		t.Errorf("unrelated key changed: %q %v", v, err)
	}
	g, err := lsGet(db, lsGroups)
	if err != nil {
		t.Fatal(err)
	}
	if g["tabId"] != "t1" {
		t.Errorf("persisted copy lost its other fields: %v", g)
	}
	return store["state"].(map[string]any)
}

func TestSyncSidebar(t *testing.T) {
	appRunning = func() bool { return false }
	d := fakeApp(t)
	bk := filepath.Join(t.TempDir(), "backups")
	res, err := syncSidebar(d, scopeA, []string{scopeA, scopeB, scopeC}, bk)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Scopes != 2 || res.Pins != 2 {
		t.Fatalf("result %+v", res)
	}
	st := readState(t, d)
	db, err := openLocalStorage(d.localStorageDir(), false)
	must(t, err)
	secCopy, err := lsGet(db, lsSections)
	db.close()
	if err != nil {
		t.Fatal(err)
	}
	// Each copy is rebuilt from its own source entry and keeps the
	// target's bookkeeping.
	if mf := secCopy["value"].(map[string]any)[scopeB].(map[string]any)["migratedFrom"]; mf != "persisted" {
		t.Errorf("persisted sections of B: migratedFrom %v", mf)
	}
	if !jsonEqual(stripMigrated(secCopy["value"]), stripMigrated(st["codeSidebarByScope"])) {
		t.Error("persisted sections differ from the store")
	}
	groups := st["customGroupsByScope"].(map[string]any)
	// B keeps its own id for Alpha and gets A's id for Beta, which it lacked.
	b := groups[scopeB].(map[string]any)
	a := b["assignments"].(map[string]any)
	if a["code:local_1"] != "cg-b1" || a["code:local_2"] != "cg-a2" {
		t.Errorf("B assignments %v", a)
	}
	if _, ok := b["order"].(map[string]any)["cg-b1"]; !ok {
		t.Errorf("B order not remapped: %v", b["order"])
	}
	secs := st["codeSidebarByScope"].(map[string]any)[scopeB].(map[string]any)["sections"].([]any)
	if secs[1].(map[string]any)["id"] != "cg-b1" {
		t.Errorf("B section ids %v", secs)
	}
	c := groups[scopeC].(map[string]any)["assignments"].(map[string]any)
	if c["code:local_1"] != "cg-a1" || c["code:local_2"] != "cg-a2" {
		t.Errorf("C assignments %v", c)
	}
	if !jsonEqual(groups[scopeA], mustLayoutA()) {
		t.Errorf("source changed: %v", groups[scopeA])
	}
	if st["big"].(json.Number) != "1793727383050123456" || st["note"] != "chinese 中文 survives" {
		t.Errorf("other state changed: %v %v", st["big"], st["note"])
	}
	// Pins follow A.
	for _, scope := range []string{scopeB, scopeC} {
		p := filepath.Join(d.sessionsDir(), filepath.FromSlash(scope), "local_1.json")
		if on, _ := starred(p); !on {
			t.Errorf("%s local_1 not pinned", scope)
		}
		var e map[string]any
		data, _ := os.ReadFile(p)
		must(t, json.Unmarshal(data, &e))
		if e["title"] != "t" || e["sessionId"] != "local_1" {
			t.Errorf("entry fields changed: %v", e)
		}
	}
	// The mirror.
	cfg, _ := os.ReadFile(d.prefsPath())
	var m map[string]any
	must(t, json.Unmarshal(cfg, &m))
	ep := m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
	if ep["keep"] != true || ep["dframe-group-scopes"] == nil || m["x"] != float64(2) {
		t.Errorf("mirror %v", m)
	}
	// A backup with the old database, mirror and entries.
	for _, f := range []string{"leveldb/CURRENT", "claude_desktop_config.json", "claude-code-sessions/bbbb/orgB/local_1.json"} {
		for _, dir := range []string{res.Backup, filepath.Join(bk, "first")} {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
				t.Errorf("backup %s lacks %s", dir, f)
			}
		}
	}
	// A second run finds nothing to do.
	res, err = syncSidebar(d, scopeA, []string{scopeA, scopeB, scopeC}, bk)
	if err != nil || res.Changed {
		t.Errorf("second run %+v %v", res, err)
	}
}

func mustLayoutA() map[string]any {
	g, _ := layout(map[string]string{"Alpha": "cg-a1", "Beta": "cg-a2"}, map[string]string{"local_1": "Alpha", "local_2": "Beta"})
	return g
}

func TestSyncSidebarRefusesWhileRunning(t *testing.T) {
	appRunning = func() bool { return true }
	defer func() { appRunning = func() bool { return false } }()
	if _, err := syncSidebar(fakeApp(t), scopeA, []string{scopeA, scopeB}, t.TempDir()); err == nil {
		t.Fatal("synced while Claude runs")
	}
}

func TestSyncSidebarUnknownSourceChangesNothing(t *testing.T) {
	appRunning = func() bool { return false }
	d := fakeApp(t)
	before := readState(t, d)
	if _, err := syncSidebar(d, "zzzz/orgZ", []string{scopeA, scopeB}, t.TempDir()); err == nil {
		t.Fatal("no error for a source without layout")
	}
	if !jsonEqual(before, readState(t, d)) {
		t.Error("state changed")
	}
}

func TestLocalStorageEncoding(t *testing.T) {
	for _, s := range []string{"", "plain", "é latin", "中文", "emoji 😀"} {
		got, err := lsDecode(lsEncode(s))
		if err != nil || got != s {
			t.Errorf("%q -> %q %v", s, got, err)
		}
	}
	if lsEncode("é")[0] != 1 || lsEncode("中")[0] != 0 {
		t.Error("wrong encoding chosen")
	}
}

func stripMigrated(v any) any {
	c := deepCopy(v).(map[string]any)
	for _, sc := range c {
		delete(sc.(map[string]any), "migratedFrom")
	}
	return c
}

func TestSyncSidebarRefusesEmptySource(t *testing.T) {
	appRunning = func() bool { return false }
	d := fakeApp(t)
	before := readState(t, d)
	if _, err := syncSidebar(d, scopeC, []string{scopeA, scopeB, scopeC}, t.TempDir()); err == nil {
		t.Fatal("copied an account without folders over the others")
	}
	if !jsonEqual(before, readState(t, d)) {
		t.Error("state changed")
	}
}

func TestRebuildScopeRefusesDuplicateNames(t *testing.T) {
	g, s := layout(map[string]string{"Alpha": "x1"}, nil)
	g["groups"] = append(g["groups"].([]any), map[string]any{"id": "x2", "name": "Alpha"})
	tg, ts := layout(map[string]string{"Alpha": "y1"}, nil)
	if _, _, err := rebuildScope(g, s, tg, ts); err == nil {
		t.Error("source with two folders of one name accepted")
	}
	if _, _, err := rebuildScope(tg, ts, g, s); err == nil {
		t.Error("target with two folders of one name accepted")
	}
}

func TestPinsFollowLocalID(t *testing.T) {
	appRunning = func() bool { return false }
	d := fakeApp(t)
	p := filepath.Join(d.sessionsDir(), "bbbb", "orgB", "local_1.json")
	must(t, os.WriteFile(p, []byte(`{"sessionId":"local_1","cliSessionId":"other","isStarred":false}`), 0o600))
	if _, err := syncSidebar(d, scopeA, []string{scopeA, scopeB, scopeC}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if on, _ := starred(p); !on {
		t.Error("the same sidebar row in B was not pinned")
	}
}

func TestSyncSidebarRollsBackOnPinFailure(t *testing.T) {
	appRunning = func() bool { return false }
	d := fakeApp(t)
	before := readState(t, d)
	prefsBefore, _ := os.ReadFile(d.prefsPath())
	// A pin target that cannot be replaced: its folder is read-only.
	dir := filepath.Join(d.sessionsDir(), "cccc", "orgC")
	must(t, os.Chmod(dir, 0o500))
	defer os.Chmod(dir, 0o700)
	_, err := syncSidebar(d, scopeA, []string{scopeA, scopeB, scopeC}, t.TempDir())
	if err == nil {
		t.Skip("folder permissions not enforced here")
	}
	if !jsonEqual(before, readState(t, d)) {
		t.Error("Local Storage not put back")
	}
	if after, _ := os.ReadFile(d.prefsPath()); string(after) != string(prefsBefore) {
		t.Error("preferences not put back")
	}
	if on, _ := starred(filepath.Join(d.sessionsDir(), "bbbb", "orgB", "local_1.json")); on {
		t.Error("pin of B not put back")
	}
}

func TestPinsSyncEvenWhenFoldersCannot(t *testing.T) {
	appRunning = func() bool { return false }
	d := fakeApp(t)
	// C has two folders of one name, so folders cannot be matched.
	db, err := openLocalStorage(d.localStorageDir(), false)
	must(t, err)
	store, err := lsGet(db, lsStore)
	must(t, err)
	c := store["state"].(map[string]any)["customGroupsByScope"].(map[string]any)[scopeC].(map[string]any)
	c["groups"] = []any{map[string]any{"id": "d1", "name": "Dup"}, map[string]any{"id": "d2", "name": "Dup"}}
	must(t, lsPut(db, map[string]any{lsStore: store}))
	must(t, db.close())
	res, err := syncSidebar(d, scopeA, []string{scopeA, scopeB, scopeC}, t.TempDir())
	if err == nil {
		t.Error("no error for duplicate folder names")
	}
	if res.Pins != 2 {
		t.Errorf("pins not synced: %+v", res)
	}
	if on, _ := starred(filepath.Join(d.sessionsDir(), "cccc", "orgC", "local_1.json")); !on {
		t.Error("C not pinned")
	}
}
