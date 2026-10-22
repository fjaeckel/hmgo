// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
)

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

var fixedNow = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }

type doc struct {
	Version int    `json:"version"`
	Value   string `json:"value"`
}

func testFile(t *testing.T, dir string, spec FileSpec) (*File, *logBuf) {
	t.Helper()
	var l logBuf
	s := Open(Options{Dir: dir, Logf: l.logf, Now: fixedNow, StatePath: ptr(""), DevicesPath: ptr("")})
	if spec.Name == "" {
		spec.Name = "test.json"
	}
	if spec.Version == 0 {
		spec.Version = 1
	}
	return s.File(spec), &l
}

func ptr[T any](v T) *T { return &v }

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestWriteFileAtomicAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "state")
	path := filepath.Join(dir, "x.json")
	if err := WriteFileAtomic(path, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != DirMode {
		t.Errorf("directory mode %v, want %v", m, DirMode)
	}
	if m := mode(t, path); m != FileMode {
		t.Errorf("file mode %v, want %v", m, FileMode)
	}
	// An existing world-readable file is replaced by a private one.
	os.Chmod(path, 0o644)
	if err := WriteFileAtomic(path, []byte(`{"version":1,"x":2}`)); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, path); m != FileMode {
		t.Errorf("file mode after rewrite %v", m)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"version":1,"x":2}` {
		t.Errorf("content %s", b)
	}
	// No temporary files are left behind.
	if got := entries(t, dir); !reflect.DeepEqual(got, []string{"x.json"}) {
		t.Errorf("directory has %q", got)
	}
	// A failed write leaves the old file and no temporary file.
	if err := WriteFileAtomic(filepath.Join(path, "sub"), []byte("{}")); err == nil {
		t.Error("write below a file succeeded")
	}
}

func TestSaveLoadAndUnchangedSkip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	f, _ := testFile(t, dir, FileSpec{})
	var d doc
	if found, err := f.Load(&d); found || err != nil {
		t.Fatalf("missing file: %v %v", found, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("directory created before the first write")
	}
	if w, err := f.Save(doc{Version: 1, Value: "a"}); !w || err != nil {
		t.Fatalf("first save: %v %v", w, err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(f.Path(), past, past)
	if w, err := f.Save(doc{Version: 1, Value: "a"}); w || err != nil {
		t.Errorf("unchanged save: written %v, %v", w, err)
	}
	if !f.ModTime().Equal(past) {
		t.Errorf("unchanged content was rewritten (mtime %v)", f.ModTime())
	}
	if w, err := f.Save(doc{Version: 1, Value: "b"}); !w || err != nil {
		t.Errorf("changed save: %v %v", w, err)
	}
	if found, err := f.Load(&d); !found || err != nil || d.Value != "b" {
		t.Errorf("load: %v %v %+v", found, err, d)
	}
	// The envelope version is enforced on writes.
	if _, err := f.Save(doc{Version: 2}); err == nil {
		t.Error("wrong version written")
	}
	if _, err := f.Save(map[string]int{}); err == nil {
		t.Error("missing version written")
	}
}

func TestSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	os.WriteFile(victim, []byte(`{"version":1,"value":"victim"}`), 0o644)
	f, _ := testFile(t, dir, FileSpec{})
	if err := os.Symlink(victim, f.Path()); err != nil {
		t.Fatal(err)
	}
	var d doc
	if found, err := f.Load(&d); found || !errors.Is(err, ErrSymlink) {
		t.Errorf("load through a link: %v %v", found, err)
	}
	if d.Value != "" {
		t.Errorf("read through a link: %+v", d)
	}
	if _, err := f.Save(doc{Version: 1, Value: "x"}); err == nil {
		t.Error("save through a link succeeded")
	}
	if f.Blocked() == nil {
		t.Error("not blocked")
	}
	if b, _ := os.ReadFile(victim); string(b) != `{"version":1,"value":"victim"}` {
		t.Errorf("link target changed: %s", b)
	}
	if fi, _ := os.Lstat(f.Path()); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link replaced")
	}
	// Also without a prior Load, and for the plain write primitive.
	f2, _ := testFile(t, dir, FileSpec{})
	if _, err := f2.Save(doc{Version: 1}); !errors.Is(err, ErrSymlink) {
		t.Errorf("save without load: %v", err)
	}
	if err := WriteFileAtomic(f.Path(), []byte("{}")); !errors.Is(err, ErrSymlink) {
		t.Errorf("WriteFileAtomic: %v", err)
	}
}

func TestCorruptFileQuarantined(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"truncated", `{"version":1,"val`},
		{"no version", `{"value":"x"}`},
		{"old version without migration", `{"version":0,"value":"x"}`},
		{"wrong type", `{"version":1,"value":7}`},
		{"not an object", `[1,2]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			f, _ := testFile(t, dir, FileSpec{})
			os.WriteFile(f.Path(), []byte(tc.content), 0o600)
			var d doc
			found, err := f.Load(&d)
			var ce *CorruptError
			if found || !errors.As(err, &ce) || ce.Quarantined == "" {
				t.Fatalf("load: %v %v", found, err)
			}
			if b, _ := os.ReadFile(ce.Quarantined); string(b) != tc.content {
				t.Errorf("quarantined copy %q", b)
			}
			if !strings.HasPrefix(filepath.Base(ce.Quarantined), "test.json.corrupt-20261022T120000.000Z") {
				t.Errorf("quarantine name %s", ce.Quarantined)
			}
			// Startup continues with empty data; writes work again.
			if found, err := f.Load(&doc{}); found || err != nil {
				t.Errorf("second load: %v %v", found, err)
			}
			if f.Blocked() != nil {
				t.Errorf("blocked: %v", f.Blocked())
			}
			if w, err := f.Save(doc{Version: 1, Value: "new"}); !w || err != nil {
				t.Errorf("save: %v %v", w, err)
			}
		})
	}
}

func TestQuarantineKeepsThree(t *testing.T) {
	dir := t.TempDir()
	f, _ := testFile(t, dir, FileSpec{})
	for i := range 5 {
		os.WriteFile(f.Path(), []byte(fmt.Sprintf("broken %d", i)), 0o600)
		if _, err := f.Load(&doc{}); err == nil {
			t.Fatal("no error")
		}
	}
	got := entries(t, dir)
	if len(got) != MaxCorrupt {
		t.Fatalf("entries %q", got)
	}
	// The newest are kept.
	for i, name := range got {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if want := fmt.Sprintf("broken %d", i+2); string(b) != want {
			t.Errorf("%s: %q, want %q", name, b, want)
		}
	}
}

func TestDamagedSinceLoadNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	f, l := testFile(t, dir, FileSpec{})
	f.Save(doc{Version: 1, Value: "a"})
	os.WriteFile(f.Path(), []byte("garbage"), 0o600)
	if w, err := f.Save(doc{Version: 1, Value: "b"}); !w || err != nil {
		t.Fatalf("save: %v %v", w, err)
	}
	if len(entries(t, dir)) != 2 || !strings.Contains(l.String(), "moved it to") {
		t.Errorf("entries %q, log %q", entries(t, dir), l)
	}
}

func TestNewerVersionLeftAlone(t *testing.T) {
	dir := t.TempDir()
	f, _ := testFile(t, dir, FileSpec{})
	newer := `{"version":2,"value":"from the future"}`
	os.WriteFile(f.Path(), []byte(newer), 0o600)
	var d doc
	if found, err := f.Load(&d); found || !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("load: %v %v", found, err)
	}
	if f.Blocked() == nil {
		t.Fatal("not blocked")
	}
	if _, err := f.Save(doc{Version: 1, Value: "old"}); !errors.Is(err, ErrNewerVersion) {
		t.Errorf("save: %v", err)
	}
	if b, _ := os.ReadFile(f.Path()); string(b) != newer {
		t.Errorf("file changed: %s", b)
	}
	if got := entries(t, dir); len(got) != 1 {
		t.Errorf("entries %q", got)
	}
	// Without a prior Load, Save refuses too.
	f2, _ := testFile(t, dir, FileSpec{})
	if _, err := f2.Save(doc{Version: 1}); !errors.Is(err, ErrNewerVersion) {
		t.Errorf("save without load: %v", err)
	}
	// Removing the file unblocks it (after the next Load).
	os.Remove(f.Path())
	if found, err := f.Load(&d); found || err != nil || f.Blocked() != nil {
		t.Errorf("after removal: %v %v %v", found, err, f.Blocked())
	}
}

func TestMigrationHook(t *testing.T) {
	type v2 struct {
		Version int    `json:"version"`
		Name    string `json:"name"`
	}
	migrate := func(from int, data []byte) ([]byte, error) {
		if from != 1 {
			return nil, fmt.Errorf("from %d", from)
		}
		var old doc
		if err := json.Unmarshal(data, &old); err != nil {
			return nil, err
		}
		return json.Marshal(v2{Version: 2, Name: old.Value})
	}
	dir := t.TempDir()
	f, _ := testFile(t, dir, FileSpec{Version: 2, Migrate: migrate})
	os.WriteFile(f.Path(), []byte(`{"version":1,"value":"x"}`), 0o600)
	var d v2
	if found, err := f.Load(&d); !found || err != nil || d.Name != "x" {
		t.Errorf("migrated: %v %v %+v", found, err, d)
	}
	// A failing migration quarantines the file.
	f, _ = testFile(t, dir, FileSpec{Version: 3, Migrate: migrate})
	os.WriteFile(f.Path(), []byte(`{"version":2,"name":"x"}`), 0o600)
	var ce *CorruptError
	if _, err := f.Load(&d); !errors.As(err, &ce) {
		t.Errorf("failed migration: %v", err)
	}
}

func TestLegacyMigration(t *testing.T) {
	root := t.TempDir()
	legacyDir := filepath.Join(root, "var-lib-hmbridge")
	os.MkdirAll(legacyDir, 0o755)
	oldState := filepath.Join(legacyDir, "state.json")
	oldDevices := filepath.Join(legacyDir, "devices.json")
	os.WriteFile(oldState, []byte(`{"version":1,"devices":{}}`), 0o644)
	os.WriteFile(oldDevices, []byte(`{"version":1,"devices":[{"name":"Lampe","model":"HM-LC-Sw1-Pl-DN-R1","address":"112233","key_index":0,"paired":"2026-10-01T00:00:00Z"}]}`), 0o644)
	defer func(s, d string) { LegacyStateFile, LegacyDevicesFile = s, d }(LegacyStateFile, LegacyDevicesFile)
	LegacyStateFile, LegacyDevicesFile = oldState, oldDevices

	dir := filepath.Join(root, "usr-local-hmbridge", "state")
	cfg, err := config.Parse([]byte(`{"central_address":"1A2B3C","state_dir":"` + dir + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	l := &logBuf{}
	s := OpenConfig(cfg, l.logf)
	devs, err := s.LoadDevices()
	if err != nil || len(devs) != 1 || devs[0].Name != "Lampe" {
		t.Fatalf("migrated devices: %+v %v", devs, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "state.json")); string(b) != `{"version":1,"devices":{}}` {
		t.Errorf("migrated state %q", b)
	}
	if m := mode(t, filepath.Join(dir, "devices.json")); m != FileMode {
		t.Errorf("migrated file mode %v", m)
	}
	if m := mode(t, dir); m != DirMode {
		t.Errorf("state dir mode %v", m)
	}
	// The old files stay in place; the copy is logged.
	if _, err := os.Stat(oldDevices); err != nil {
		t.Errorf("old file removed: %v", err)
	}
	if got := strings.Count(l.String(), "copied"); got != 2 {
		t.Errorf("log %q", l)
	}

	// The next start does not copy again (the new file exists).
	s.SaveDevices(nil)
	l = &logBuf{}
	s = OpenConfig(cfg, l.logf)
	if devs, _ := s.LoadDevices(); len(devs) != 0 || strings.Contains(l.String(), "copied") {
		t.Errorf("copied again: %+v %q", devs, l)
	}

	// A link at the old location is not copied.
	dir2 := filepath.Join(root, "other")
	os.Remove(oldState)
	os.Symlink(oldDevices, oldState)
	l = &logBuf{}
	Open(Options{Dir: dir2, LegacyStatePath: oldState, Logf: l.logf})
	if _, err := os.Lstat(filepath.Join(dir2, "state.json")); !os.IsNotExist(err) || !strings.Contains(l.String(), "symbolic link") {
		t.Errorf("link copied: %v %q", err, l)
	}

	// The default state_dir is the legacy location: nothing to copy.
	l = &logBuf{}
	Open(Options{Dir: legacyDir, LegacyStatePath: oldState, LegacyDevicesPath: oldDevices, Logf: l.logf})
	if l.String() != "" {
		t.Errorf("log %q", l)
	}
}

func TestDeprecatedPaths(t *testing.T) {
	root := t.TempDir()
	defer func(s, d string) { LegacyStateFile, LegacyDevicesFile = s, d }(LegacyStateFile, LegacyDevicesFile)
	LegacyStateFile, LegacyDevicesFile = filepath.Join(root, "old-state.json"), filepath.Join(root, "old-devices.json")
	os.WriteFile(LegacyStateFile, []byte(`{"version":1}`), 0o600)
	os.WriteFile(LegacyDevicesFile, []byte(`{"version":1}`), 0o600)

	explicit := filepath.Join(root, "custom", "devs.json")
	dir := filepath.Join(root, "state")
	cfg, err := config.Parse([]byte(`{"central_address":"1A2B3C","state_dir":"` + dir + `","devices_file":"` + explicit + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	var l logBuf
	s := OpenConfig(cfg, l.logf)
	// devices_file: that exact path, never migrated; the state file
	// lives in state_dir (migrated from the legacy location).
	if s.DevicesPath() != explicit || s.State().Path() != filepath.Join(dir, "state.json") {
		t.Errorf("paths %q %q", s.DevicesPath(), s.State().Path())
	}
	if _, err := os.Stat(explicit); !os.IsNotExist(err) {
		t.Errorf("legacy devices copied to the explicit path")
	}
	if err := s.SaveDevices([]Device{{Name: "a", Address: "112233"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(explicit); err != nil {
		t.Error(err)
	}

	// state_file "" disables only the state file; state_dir "" all.
	cfg, _ = config.Parse([]byte(`{"central_address":"1A2B3C","state_dir":"` + dir + `","state_file":""}`))
	s = OpenConfig(cfg, l.logf)
	if s.State() != nil || !s.PairingEnabled() {
		t.Errorf("state_file \"\": state %v pairing %v", s.State(), s.PairingEnabled())
	}
	cfg, _ = config.Parse([]byte(`{"central_address":"1A2B3C","state_dir":"","devices":[{"name":"a","model":"HM-CC-RT-DN","address":"38F59C"}]}`))
	s = OpenConfig(cfg, l.logf)
	if s.State() != nil || s.PairingEnabled() || s.Dir() != "" {
		t.Errorf("state_dir \"\": %+v", s)
	}
	if err := s.SaveDevices(nil); !errors.Is(err, ErrPairingDisabled) {
		t.Errorf("save without devices file: %v", err)
	}
}

// Both files survive a restart (a new Store on the same directory).
func TestRestartReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	paired := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	devs := []Device{
		{Name: "HM-Sec-SCo NEQ0000002", Model: "HM-Sec-SCo", Address: "4C5D6F", Serial: "NEQ0000002", AESChannels: []int{1}, KeyIndex: 2, Paired: paired},
		{Name: "Plug", Model: "HM-LC-Sw1-Pl-DN-R1", Address: "112233", KeyIndex: 0, Paired: paired, Firmware: "2.8", ModelID: "00D8"},
	}
	type state struct {
		Version int               `json:"version"`
		Devices map[string]string `json:"devices"`
	}
	s := Open(Options{Dir: dir})
	if err := s.SaveDevices(devs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.State().Save(state{Version: StateVersion, Devices: map[string]string{"38f59c": "x"}}); err != nil {
		t.Fatal(err)
	}

	s = Open(Options{Dir: dir})
	got, err := s.LoadDevices()
	if want := []Device{devs[1], devs[0]}; err != nil || !reflect.DeepEqual(got, want) { // sorted by address
		t.Errorf("devices %+v %v", got, err)
	}
	var st state
	if found, err := s.State().Load(&st); !found || err != nil || st.Devices["38f59c"] != "x" {
		t.Errorf("state %+v %v %v", st, found, err)
	}
	if got := entries(t, dir); !reflect.DeepEqual(got, []string{"devices.json", "state.json"}) {
		t.Errorf("directory %q", got)
	}
	// devices.json stays readable for people.
	if b, _ := os.ReadFile(filepath.Join(dir, "devices.json")); !strings.Contains(string(b), "\n \"devices\": [") {
		t.Errorf("devices.json not indented:\n%s", b)
	}
}

func TestDevicesConfigRoundTrip(t *testing.T) {
	cfg, err := config.Parse([]byte(`{"central_address":"1A2B3C","security":{"passphrase":"x","index":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	in := Device{Name: "Taster", Model: "HM-PB-2-WM55", Address: "73788E", Serial: "NEQ1234567", AESChannels: []int{1, 2}, KeyIndex: 2}
	c, err := cfg.ResolveStoreDevice(in.Config())
	if err != nil {
		t.Fatal(err)
	}
	if !c.AESEnabled() || c.KeyIndex() != 2 || !c.FromStore() {
		t.Errorf("resolved %+v", c)
	}
	out := DeviceFromConfig(c, "1.4", "006B", time.Time{})
	if out.Address != "73788E" || !reflect.DeepEqual(out.AESChannels, []int{1, 2}) || out.KeyIndex != 2 || out.Firmware != "1.4" {
		t.Errorf("DeviceFromConfig = %+v", out)
	}
	// Without AES channels: no AES.
	in.AESChannels, in.KeyIndex = nil, 0
	c, err = cfg.ResolveStoreDevice(in.Config())
	if err != nil || c.AESEnabled() || c.KeyIndex() != 0 {
		t.Errorf("no AES: %+v %v", c, err)
	}
}

// An unreadable (but existing) file is quarantined, not overwritten.
func TestUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}
	dir := t.TempDir()
	f, _ := testFile(t, dir, FileSpec{})
	os.WriteFile(f.Path(), []byte(`{"version":1}`), 0o000)
	var ce *CorruptError
	if _, err := f.Load(&doc{}); !errors.As(err, &ce) || ce.Quarantined == "" {
		t.Errorf("load: %v", err)
	}
}
