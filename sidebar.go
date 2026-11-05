package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// Pins are the isStarred flag of each chat's entry file, per account. Pin
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
	Pins    int    // entry files whose pin was changed
	Backup  string // where the old files are
	Changed bool
}

// syncSidebar makes every account in scopes show the folders, folder
// contents and pins of source. The app must be closed. Before it writes,
// it copies everything it changes into a new folder under backupRoot. If a
// step fails, what it had written is put back.
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
	groupsByScope, sectionsByScope, changed, err := rewriteLocalStorage(work, source, others)
	if err != nil {
		return res, err
	}

	// The pins.
	root := d.sessionsDir()
	pinEdits, err := planPins(root, source, others)
	if err != nil {
		return res, err
	}
	if !changed && len(pinEdits) == 0 {
		return res, nil
	}

	// Back up everything that is about to change.
	bk := filepath.Join(backupRoot, time.Now().Format("20060102-150405.000"))
	if err := os.MkdirAll(bk, 0o700); err != nil {
		return res, err
	}
	res.Backup = bk
	if changed {
		if err := copyDir(live, filepath.Join(bk, "leveldb")); err != nil {
			return res, fmt.Errorf("back up Local Storage: %w", err)
		}
		if err := copyFile(d.prefsPath(), filepath.Join(bk, filepath.Base(d.prefsPath()))); err != nil {
			return res, fmt.Errorf("back up %s: %w", filepath.Base(d.prefsPath()), err)
		}
	}
	for _, e := range pinEdits {
		rel, _ := filepath.Rel(root, e.path)
		if err := copyFile(e.path, filepath.Join(bk, "claude-code-sessions", rel)); err != nil {
			return res, fmt.Errorf("back up %s: %w", rel, err)
		}
	}
	pruneBackups(backupRoot)

	// Write: the database first, then the mirror, then the pins.
	if changed {
		if err := swapDir(work, live); err != nil {
			return res, fmt.Errorf("replace Local Storage: %w", err)
		}
		if err := writePrefsMirror(d.prefsPath(), groupsByScope, sectionsByScope); err != nil {
			restoreSidebar(d, bk, nil)
			return res, fmt.Errorf("update %s: %w", filepath.Base(d.prefsPath()), err)
		}
		res.Scopes = len(others)
	}
	for i, e := range pinEdits {
		if err := setStarred(e.path, e.starred); err != nil {
			restoreSidebar(d, bk, pinEdits[:i+1])
			return res, fmt.Errorf("set pin: %w", err)
		}
	}
	res.Pins = len(pinEdits)
	res.Changed = true
	return res, nil
}

// restoreSidebar puts a sidebar backup back: the database and mirror when
// the backup has them, and the given entry files.
func restoreSidebar(d desktop, bk string, pins []pinEdit) {
	if _, err := os.Stat(filepath.Join(bk, "leveldb", "CURRENT")); err == nil {
		tmp := d.localStorageDir() + ".switcher-restore"
		_ = os.RemoveAll(tmp)
		if copyDir(filepath.Join(bk, "leveldb"), tmp) == nil {
			_ = swapDir(tmp, d.localStorageDir())
		}
		_ = os.RemoveAll(tmp)
		_ = copyFile(filepath.Join(bk, filepath.Base(d.prefsPath())), d.prefsPath())
	}
	for _, e := range pins {
		rel, _ := filepath.Rel(d.sessionsDir(), e.path)
		_ = copyFile(filepath.Join(bk, "claude-code-sessions", rel), e.path)
	}
}

// rewriteLocalStorage changes the database at dir so the folders of every
// scope in others match source. It returns the new per-scope maps and
// whether anything changed.
func rewriteLocalStorage(dir, source string, others []string) (groups, sections map[string]any, changed bool, err error) {
	db, err := openLocalStorage(dir, false)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open Local Storage: %w", err)
	}
	defer func() {
		if cerr := db.close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	store, err := lsGet(db, lsStore)
	if err != nil {
		return nil, nil, false, err
	}
	state, ok := store["state"].(map[string]any)
	if !ok {
		return nil, nil, false, errors.New("dframe-store has no state")
	}
	groups, _ = state["customGroupsByScope"].(map[string]any)
	sections, _ = state["codeSidebarByScope"].(map[string]any)
	if groups == nil || sections == nil {
		return nil, nil, false, errors.New("dframe-store has no folders")
	}
	srcGroups, _ := groups[source].(map[string]any)
	srcSections, _ := sections[source].(map[string]any)
	if srcGroups == nil || srcSections == nil {
		return nil, nil, false, fmt.Errorf("no sidebar layout for the account being left (%s)", source)
	}

	for _, scope := range others {
		tgtGroups, _ := groups[scope].(map[string]any)
		tgtSections, _ := sections[scope].(map[string]any)
		g, s, err := rebuildScope(srcGroups, srcSections, tgtGroups, tgtSections)
		if err != nil {
			return nil, nil, false, fmt.Errorf("scope %s: %w", scope, err)
		}
		if !jsonEqual(g, groups[scope]) || !jsonEqual(s, sections[scope]) {
			changed = true
		}
		groups[scope], sections[scope] = g, s
	}
	if !changed {
		return groups, sections, false, nil
	}

	puts := map[string]any{lsStore: store}
	for key, val := range map[string]map[string]any{lsGroups: groups, lsSections: sections} {
		obj, err := lsGet(db, key)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, nil, false, err
		}
		if obj == nil {
			obj = map[string]any{}
		}
		obj["value"] = val
		puts[key] = obj
	}
	if err := lsPut(db, puts); err != nil {
		return nil, nil, false, err
	}
	return groups, sections, true, nil
}

// rebuildScope is the source layout with the target's own folder ids,
// matched by folder name. A folder the target does not have keeps the
// source's id.
func rebuildScope(srcGroups, srcSections, tgtGroups, tgtSections map[string]any) (map[string]any, map[string]any, error) {
	tgtIDs := map[string]string{}
	for _, s := range listOf(tgtSections, "sections") {
		if str(s, "kind") == "manual" && str(s, "name") != "" && str(s, "id") != "" {
			tgtIDs[str(s, "name")] = str(s, "id")
		}
	}
	for _, g := range listOf(tgtGroups, "groups") {
		if str(g, "name") != "" && str(g, "id") != "" {
			tgtIDs[str(g, "name")] = str(g, "id")
		}
	}

	g := deepCopy(srcGroups).(map[string]any)
	idmap := map[string]string{}
	for _, gr := range listOf(g, "groups") {
		id, name := str(gr, "id"), str(gr, "name")
		if id == "" {
			return nil, nil, errors.New("a folder has no id")
		}
		newID := id
		if t, ok := tgtIDs[name]; ok {
			newID = t
		}
		idmap[id] = newID
		gr["id"] = newID
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

	s := deepCopy(srcSections).(map[string]any)
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

// writePrefsMirror updates the copy of the folders in the app's preferences.
func writePrefsMirror(path string, groups, sections map[string]any) error {
	cfg, err := readObject(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var prefs map[string]any
	if raw, ok := cfg.get("preferences"); ok {
		if err := decodeJSON(raw, &prefs); err != nil {
			return err
		}
	}
	if prefs == nil {
		prefs = map[string]any{}
	}
	ep, _ := prefs["epitaxyPrefs"].(map[string]any)
	if ep == nil {
		ep = map[string]any{}
	}
	ep["dframe-group-scopes"] = groups
	ep["dframe-code-sections"] = sections
	prefs["epitaxyPrefs"] = ep
	raw, err := encodeJSON(prefs)
	if err != nil {
		return err
	}
	cfg.set("preferences", raw)
	return cfg.write(path)
}

// pinEdit is an entry file whose pin is to be set to starred.
type pinEdit struct {
	path    string
	starred bool
}

// planPins lists the entry files of the other accounts whose pin differs
// from the same chat in source. Chats an account does not have are left
// to the chat import.
func planPins(root, source string, others []string) ([]pinEdit, error) {
	src := filepath.Join(root, filepath.FromSlash(source))
	files, err := filepath.Glob(filepath.Join(src, "local_*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var edits []pinEdit
	for _, f := range files {
		want, err := starred(f)
		if err != nil {
			continue // An entry the app is rewriting or cannot read is not ours to judge.
		}
		for _, scope := range others {
			dst := filepath.Join(root, filepath.FromSlash(scope), filepath.Base(f))
			have, err := starred(dst)
			if err != nil || have == want {
				continue
			}
			edits = append(edits, pinEdit{dst, want})
		}
	}
	return edits, nil
}

func starred(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var e struct {
		IsStarred bool `json:"isStarred"`
	}
	err = json.Unmarshal(data, &e)
	return e.IsStarred, err
}

// setStarred changes only the isStarred entry of an entry file.
func setStarred(path string, on bool) error {
	o, err := readObject(path)
	if err != nil {
		return err
	}
	o.set("isStarred", json.RawMessage(fmt.Sprint(on)))
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
	return writeFileAtomic(path, buf.Bytes())
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
	return os.RemoveAll(old)
}

// scopeKey is how the app names an account's sidebar.
func scopeKey(account, org string) string { return account + "/" + org }
