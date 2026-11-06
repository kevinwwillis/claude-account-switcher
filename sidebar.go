package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf16"
)

// The Code sidebar keeps its layout per account ("scope" = "<account>/<org>")
// in the app's Local Storage, a LevelDB database under the claude.ai origin:
//
//   dframe-store                         {"state": {customGroupsByScope, codeSidebarByScope, ...}}
//   LSS-persisted.dframe-group-scopes    {"value": customGroupsByScope}
//   LSS-persisted.dframe-code-sections   {"value": codeSidebarByScope}
//
// customGroupsByScope[scope] holds the folders ({groups, assignments, order});
// codeSidebarByScope[scope] holds the sections, one "manual" section per
// folder. claude_desktop_config.json mirrors both under
// preferences.epitaxyPrefs, but the app treats Local Storage as the truth.
// Each account gets its own folder ids, so folders are matched by name.
//
// Pins and archiving are the isStarred and isArchived flags of each chat's
// entry file, per account. Pin
// order, grouping, sorting and collapsed groups are stored once for all
// accounts and need no copying.

// appRunning is desktopRunning, replaceable in tests.
var appRunning = desktopRunning

const lsPrefix = "_https://claude.ai\x00\x01"

// lsDB is an open Local Storage database.
type lsDB interface {
	get(key []byte) ([]byte, error)
	write(puts map[string][]byte) error // atomic and synced
	close() error
}

var errNotFound = errors.New("not found")

const (
	lsStore    = "dframe-store"
	lsGroups   = "LSS-persisted.dframe-group-scopes"
	lsSections = "LSS-persisted.dframe-code-sections"
)

func (d desktop) localStorageDir() string {
	return filepath.Join(d.dir, "Local Storage", "leveldb")
}

func (d desktop) prefsPath() string { return filepath.Join(d.dir, "claude_desktop_config.json") }

// sidebarResult says what a sidebar sync changed.
type sidebarResult struct {
	Scopes  int    // other accounts whose folders were rewritten
	Pins    int    // entry files whose pin or archived flag was changed
	Backup  string // where the old files are
	Changed bool
}

// syncSidebar makes every account in scopes show the folders, folder
// contents and pins of source. The app must be closed. Before it writes,
// it copies everything it changes into a new folder under backupRoot (the
// first backup ever made is kept for good in backupRoot/first). If a step
// fails, what it had written is put back.
func syncSidebar(d desktop, source string, scopes []string, backupRoot string) (sidebarResult, error) {
	var res sidebarResult
	if appRunning() {
		return res, errors.New("Claude is running")
	}
	others := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s != source && !containsString(others, s) {
			others = append(others, s)
		}
	}
	if len(others) == 0 {
		return res, nil
	}

	// Build the new database in a copy next to the live one.
	live := d.localStorageDir()
	if _, err := os.Stat(filepath.Join(live, "CURRENT")); err != nil {
		if _, oerr := os.Stat(live + ".switcher-old"); oerr == nil {
			return res, fmt.Errorf("Local Storage is missing but %s.switcher-old exists: an earlier sync was interrupted, put that folder back by hand", live)
		}
		return res, fmt.Errorf("no Local Storage database: %w", err)
	}
	work := live + ".switcher-work"
	if err := os.RemoveAll(work); err != nil {
		return res, err
	}
	if err := copyDir(live, work); err != nil {
		_ = os.RemoveAll(work)
		return res, fmt.Errorf("copy Local Storage: %w", err)
	}
	defer os.RemoveAll(work)
	// A folder problem does not hold back the pins; it is reported at the end.
	changed, folderErr := rewriteLocalStorage(work, source, others)
	var mirror *object
	mirrorChanged := false
	if folderErr == nil {
		mirror, mirrorChanged, folderErr = planPrefsMirror(d.prefsPath(), source, others)
	}
	if folderErr != nil {
		changed, mirrorChanged = false, false
		folderErr = fmt.Errorf("folders not synced: %w", folderErr)
	}

	// The pins.
	root := d.sessionsDir()
	pinEdits, err := planPins(root, source, others)
	if err != nil {
		return res, err
	}
	if !changed && !mirrorChanged && len(pinEdits) == 0 {
		return res, folderErr
	}

	// Back up everything that is about to change.
	bk, err := newSidebarBackup(backupRoot)
	if err != nil {
		return res, err
	}
	res.Backup = bk
	if err := copyDir(live, filepath.Join(bk, "leveldb")); err != nil {
		return res, fmt.Errorf("back up Local Storage: %w", err)
	}
	if err := copyFile(d.prefsPath(), filepath.Join(bk, filepath.Base(d.prefsPath()))); err != nil {
		return res, fmt.Errorf("back up %s: %w", filepath.Base(d.prefsPath()), err)
	}
	for _, e := range pinEdits {
		rel, _ := filepath.Rel(root, e.path)
		if err := copyFile(e.path, filepath.Join(bk, "claude-code-sessions", rel)); err != nil {
			return res, fmt.Errorf("back up %s: %w", rel, err)
		}
	}
	keepFirstBackup(backupRoot, bk)
	pruneBackups(backupRoot)

	// Write: the database, the mirror, the pins. A failure puts all of it back.
	var pinsDone []pinEdit
	fail := func(werr error) (sidebarResult, error) {
		if rerr := restoreSidebar(d, bk, pinsDone); rerr != nil {
			return sidebarResult{Backup: bk}, fmt.Errorf("%w; putting the backup %s back also failed: %v", werr, bk, rerr)
		}
		return sidebarResult{Backup: bk}, fmt.Errorf("%w (the backup was put back)", werr)
	}
	if changed {
		if err := swapDir(work, live); err != nil {
			return fail(fmt.Errorf("replace Local Storage: %w", err))
		}
	}
	if mirrorChanged {
		if err := mirror.write(d.prefsPath()); err != nil {
			return fail(fmt.Errorf("update %s: %w", filepath.Base(d.prefsPath()), err))
		}
	}
	if changed || mirrorChanged {
		res.Scopes = len(others)
	}
	for _, e := range pinEdits {
		pinsDone = append(pinsDone, e)
		if err := setFlags(e.path, e.starred, e.archived); err != nil {
			return fail(fmt.Errorf("set pin: %w", err))
		}
	}
	res.Pins = len(pinEdits)
	res.Changed = true
	return res, folderErr
}

// newSidebarBackup makes a new, empty backup folder.
func newSidebarBackup(root string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, time.Now().Format("20060102-150405-"))
}

// keepFirstBackup copies the first backup ever made to root/first, which
// pruning never removes: the sidebar as it was before the switcher touched it.
func keepFirstBackup(root, bk string) {
	first := filepath.Join(root, "first")
	if _, err := os.Stat(first); err == nil {
		return
	}
	tmp := first + ".partial"
	_ = os.RemoveAll(tmp)
	err := filepath.WalkDir(bk, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(bk, p)
		return copyFile(p, filepath.Join(tmp, rel))
	})
	if err == nil {
		err = os.Rename(tmp, first)
	}
	if err != nil {
		_ = os.RemoveAll(tmp)
	}
}

// restoreSidebar puts a sidebar backup back: the database, the mirror and
// the given entry files.
func restoreSidebar(d desktop, bk string, pins []pinEdit) error {
	var errs []error
	if _, err := os.Stat(filepath.Join(bk, "leveldb", "CURRENT")); err == nil {
		tmp := d.localStorageDir() + ".switcher-restore"
		_ = os.RemoveAll(tmp)
		if err := copyDir(filepath.Join(bk, "leveldb"), tmp); err != nil {
			errs = append(errs, err)
		} else if err := swapDir(tmp, d.localStorageDir()); err != nil {
			errs = append(errs, err)
		}
		_ = os.RemoveAll(tmp)
	}
	if _, err := os.Stat(filepath.Join(bk, filepath.Base(d.prefsPath()))); err == nil {
		if err := copyFile(filepath.Join(bk, filepath.Base(d.prefsPath())), d.prefsPath()); err != nil {
			errs = append(errs, err)
		}
	}
	for _, e := range pins {
		rel, _ := filepath.Rel(d.sessionsDir(), e.path)
		if err := copyFile(filepath.Join(bk, "claude-code-sessions", rel), e.path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rewriteLocalStorage changes the database at dir so the folders of every
// scope in others match source, and says whether anything changed. Each of
// the three copies the app keeps is rebuilt from its own source entry.
func rewriteLocalStorage(dir, source string, others []string) (changed bool, err error) {
	db, err := openLocalStorage(dir, false)
	if err != nil {
		return false, fmt.Errorf("open Local Storage: %w", err)
	}
	defer func() {
		if cerr := db.close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	store, err := lsGet(db, lsStore)
	if err != nil {
		return false, err
	}
	state, ok := store["state"].(map[string]any)
	if !ok {
		return false, errors.New("dframe-store has no state")
	}
	groups, _ := state["customGroupsByScope"].(map[string]any)
	sections, _ := state["codeSidebarByScope"].(map[string]any)
	ch, err := syncScopes(groups, sections, source, others, true)
	if err != nil {
		return false, fmt.Errorf("dframe-store: %w", err)
	}
	puts := map[string]any{}
	if ch {
		puts[lsStore] = store
	}

	// The persisted copies, when the app keeps them.
	pg, gerr := lsGet(db, lsGroups)
	ps, serr := lsGet(db, lsSections)
	for _, e := range []error{gerr, serr} {
		if e != nil && !errors.Is(e, errNotFound) {
			return false, e
		}
	}
	if pg != nil && ps != nil {
		g, _ := pg["value"].(map[string]any)
		s, _ := ps["value"].(map[string]any)
		ch, err := syncScopes(g, s, source, others, false)
		if err != nil {
			return false, fmt.Errorf("persisted sidebar: %w", err)
		}
		if ch {
			puts[lsGroups], puts[lsSections] = pg, ps
		}
	}
	if len(puts) == 0 {
		return false, nil
	}
	return true, lsPut(db, puts)
}

// syncScopes rewrites groups[scope] and sections[scope] of every scope in
// others from source. With required, a missing source layout is an error;
// without, the containers are left alone. It refuses a source without
// folders, which would empty the other accounts, and duplicate folder
// names, which cannot be matched.
func syncScopes(groups, sections map[string]any, source string, others []string, required bool) (bool, error) {
	srcGroups, _ := groups[source].(map[string]any)
	srcSections, _ := sections[source].(map[string]any)
	if srcGroups == nil || srcSections == nil {
		if required {
			return false, fmt.Errorf("no sidebar layout for the account being left (%s)", source)
		}
		return false, nil
	}
	if len(listOf(srcGroups, "groups")) == 0 {
		return false, errors.New("the account being left has no folders; not copying an empty sidebar over the others")
	}
	changed := false
	for _, scope := range others {
		tgtGroups, _ := groups[scope].(map[string]any)
		tgtSections, _ := sections[scope].(map[string]any)
		g, s, err := rebuildScope(srcGroups, srcSections, tgtGroups, tgtSections)
		if err != nil {
			return false, fmt.Errorf("scope %s: %w", scope, err)
		}
		if !jsonEqual(g, groups[scope]) || !jsonEqual(s, sections[scope]) {
			changed = true
		}
		kept := map[string]bool{}
		for _, gr := range listOf(g, "groups") {
			kept[str(gr, "name")] = true
		}
		for _, gr := range listOf(tgtGroups, "groups") {
			if !kept[str(gr, "name")] {
				log.Printf("sidebar sync: folder %q of %s is not in the account being left and is removed (it is in the backup)", str(gr, "name"), scope)
			}
		}
		groups[scope], sections[scope] = g, s
	}
	return changed, nil
}

// folderIDs maps folder names to ids, and fails on a name used twice.
func folderIDs(groups, sections map[string]any) (map[string]string, error) {
	ids := map[string]string{}
	for _, g := range listOf(groups, "groups") {
		name, id := str(g, "name"), str(g, "id")
		if id == "" {
			return nil, errors.New("a folder has no id")
		}
		if _, dup := ids[name]; dup {
			return nil, fmt.Errorf("two folders are named %q", name)
		}
		ids[name] = id
	}
	for _, s := range listOf(sections, "sections") {
		if str(s, "kind") != "manual" {
			continue
		}
		if _, ok := ids[str(s, "name")]; !ok && str(s, "id") != "" {
			ids[str(s, "name")] = str(s, "id")
		}
	}
	return ids, nil
}

// rebuildScope is the source layout with the target's own folder ids,
// matched by folder name. A folder the target does not have keeps the
// source's id. Entries of the target's scope objects other than the
// layout itself (bookkeeping such as migratedFrom) are kept.
func rebuildScope(srcGroups, srcSections, tgtGroups, tgtSections map[string]any) (map[string]any, map[string]any, error) {
	srcIDs, err := folderIDs(srcGroups, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("account being left: %w", err)
	}
	tgtIDs, err := folderIDs(tgtGroups, tgtSections)
	if err != nil {
		return nil, nil, err
	}
	idmap := map[string]string{}
	for name, id := range srcIDs {
		idmap[id] = id
		if t, ok := tgtIDs[name]; ok {
			idmap[id] = t
		}
	}

	g := map[string]any{}
	for k, v := range tgtGroups {
		g[k] = v
	}
	for _, k := range []string{"groups", "assignments", "order"} {
		delete(g, k)
		if v, ok := srcGroups[k]; ok {
			g[k] = deepCopy(v)
		}
	}
	for _, gr := range listOf(g, "groups") {
		gr["id"] = idmap[str(gr, "id")]
	}
	if a, ok := g["assignments"].(map[string]any); ok {
		for k, v := range a {
			if id, ok := v.(string); ok {
				if n, ok := idmap[id]; ok {
					a[k] = n
				}
			}
		}
	}
	if o, ok := g["order"].(map[string]any); ok {
		n := map[string]any{}
		for k, v := range o {
			if m, ok := idmap[k]; ok {
				k = m
			}
			n[k] = v
		}
		g["order"] = n
	}

	s := map[string]any{}
	for k, v := range tgtSections {
		s[k] = v
	}
	for k, v := range srcSections {
		if _, kept := s[k]; !kept || k == "sections" {
			s[k] = deepCopy(v)
		}
	}
	for _, sec := range listOf(s, "sections") {
		if str(sec, "kind") != "manual" {
			continue
		}
		if n, ok := idmap[str(sec, "id")]; ok {
			sec["id"] = n
		}
	}
	return g, s, nil
}

// planPrefsMirror rebuilds the copy of the folders in the app's preferences
// from its own source entry. It returns the file to write and whether it
// changed.
func planPrefsMirror(path, source string, others []string) (*object, bool, error) {
	cfg, err := readObject(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	raw, ok := cfg.get("preferences")
	if !ok {
		return nil, false, nil
	}
	var prefs map[string]any
	if err := decodeJSON(raw, &prefs); err != nil {
		return nil, false, err
	}
	ep, _ := prefs["epitaxyPrefs"].(map[string]any)
	g, _ := ep["dframe-group-scopes"].(map[string]any)
	s, _ := ep["dframe-code-sections"].(map[string]any)
	changed, err := syncScopes(g, s, source, others, false)
	if err != nil {
		return nil, false, fmt.Errorf("preferences mirror: %w", err)
	}
	if !changed {
		return nil, false, nil
	}
	if raw, err = encodeJSON(prefs); err != nil {
		return nil, false, err
	}
	cfg.set("preferences", raw)
	return cfg, true, nil
}

// pinEdit is an entry file whose pin and archived flag are to be set.
type pinEdit struct {
	path              string
	starred, archived bool
}

// planPins lists the entry files of the other accounts whose pin or
// archived flag differs from the same chat in source. A chat is its sidebar row, the local id,
// as for the folders; chats an account does not have are left alone.
func planPins(root, source string, others []string) ([]pinEdit, error) {
	src := filepath.Join(root, filepath.FromSlash(source))
	files, err := filepath.Glob(filepath.Join(src, "local_*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var edits []pinEdit
	for _, f := range files {
		want, err := readPin(f)
		if err != nil {
			continue // An entry the app cannot read either is not ours to judge.
		}
		for _, scope := range others {
			dst := filepath.Join(root, filepath.FromSlash(scope), filepath.Base(f))
			have, err := readPin(dst)
			if err != nil || (have.Starred == want.Starred && have.Archived == want.Archived) {
				continue
			}
			edits = append(edits, pinEdit{dst, want.Starred, want.Archived})
		}
	}
	return edits, nil
}

type pinState struct {
	Starred      bool   `json:"isStarred"`
	Archived     bool   `json:"isArchived"`
	CLISessionID string `json:"cliSessionId"`
}

func readPin(path string) (pinState, error) {
	var p pinState
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(data, &p)
	return p, err
}

func starred(path string) (bool, error) {
	p, err := readPin(path)
	return p.Starred, err
}

// setFlags changes only the isStarred and isArchived entries of an entry
// file, and keeps its modification time, which the switcher reads as chat
// activity.
func setFlags(path string, starred, archived bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	o, err := readObject(path)
	if err != nil {
		return err
	}
	o.set("isStarred", json.RawMessage(fmt.Sprint(starred)))
	if _, had := o.get("isArchived"); had || archived {
		o.set("isArchived", json.RawMessage(fmt.Sprint(archived)))
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteString(", ")
		}
		key, _ := json.Marshal(k)
		buf.Write(key)
		buf.WriteString(": ")
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	if err := writeFileAtomic(path, buf.Bytes()); err != nil {
		return err
	}
	return os.Chtimes(path, info.ModTime(), info.ModTime())
}

// Local Storage values are one byte of encoding and the text:
// 1 = Latin-1, 0 = UTF-16LE.

func lsGet(db lsDB, key string) (map[string]any, error) {
	raw, err := db.get([]byte(lsPrefix + key))
	if err != nil {
		return nil, err
	}
	text, err := lsDecode(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	var obj map[string]any
	if err := decodeJSON([]byte(text), &obj); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return obj, nil
}

// lsPut writes the objects under their keys in one batch.
func lsPut(db lsDB, objs map[string]any) error {
	puts := map[string][]byte{}
	for key, obj := range objs {
		data, err := encodeJSON(obj)
		if err != nil {
			return err
		}
		puts[lsPrefix+key] = lsEncode(string(data))
	}
	return db.write(puts)
}

func lsDecode(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("empty value")
	}
	body := raw[1:]
	switch raw[0] {
	case 1:
		r := make([]rune, len(body))
		for i, b := range body {
			r[i] = rune(b)
		}
		return string(r), nil
	case 0:
		if len(body)%2 != 0 {
			return "", errors.New("odd UTF-16 length")
		}
		u := make([]uint16, len(body)/2)
		for i := range u {
			u[i] = uint16(body[2*i]) | uint16(body[2*i+1])<<8
		}
		return string(utf16.Decode(u)), nil
	}
	return "", fmt.Errorf("unknown value encoding %d", raw[0])
}

func lsEncode(s string) []byte {
	latin := true
	for _, r := range s {
		if r > 0xff {
			latin = false
			break
		}
	}
	if latin {
		out := make([]byte, 1, len(s)+1)
		out[0] = 1
		for _, r := range s {
			out = append(out, byte(r))
		}
		return out
	}
	u := utf16.Encode([]rune(s))
	out := make([]byte, 1, 1+2*len(u))
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

// decodeJSON keeps numbers as written, so large ids and timestamps survive.
func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func jsonEqual(a, b any) bool {
	x, err1 := encodeJSON(a)
	y, err2 := encodeJSON(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func deepCopy(v any) any {
	data, _ := encodeJSON(v)
	var out any
	_ = decodeJSON(data, &out)
	return out
}

func listOf(m map[string]any, key string) []map[string]any {
	items, _ := m[key].([]any)
	var out []map[string]any
	for _, it := range items {
		if mm, ok := it.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// copyDir copies the files of a flat directory, leaving out the lock file.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "LOCK" {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// swapDir puts the directory next in place of cur. The old directory is
// renamed aside first, so a failure leaves cur as it was.
func swapDir(next, cur string) error {
	old := cur + ".switcher-old"
	if _, err := os.Stat(cur); err != nil {
		return fmt.Errorf("%s is missing: %w", cur, err)
	}
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	if err := os.Rename(cur, old); err != nil {
		return err
	}
	if err := os.Rename(next, cur); err != nil {
		if rerr := os.Rename(old, cur); rerr != nil {
			return fmt.Errorf("%v; and putting the old one back failed: %v", err, rerr)
		}
		return err
	}
	// The old copy is in the backup; a leftover is removed on the next swap.
	_ = os.RemoveAll(old)
	return nil
}

// scopeKey is how the app names an account's sidebar.
func scopeKey(account, org string) string { return account + "/" + org }
