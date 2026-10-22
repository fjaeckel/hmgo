// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Package store keeps everything hmbridge persists, as small JSON files
// in one state directory (config state_dir, default /var/lib/hmbridge):
//
//   - state.json: the last known device state, a fallback for when the
//     MQTT broker's retained messages are not available at startup
//     (written by the bridge's saver goroutine, throttled);
//   - devices.json: the devices paired with hmbridge (written on pair,
//     unpair and rename);
//   - settings.json: the device settings (configuration registers) last
//     read from the devices (written when they change);
//   - control.json: the heating control state of groups and thermostats
//     controlled by Home Assistant (whether the fail-safe fallback is
//     active, Home Assistant's last setpoints; written on fallback
//     transitions and on shutdown).
//
// Further data (e.g. HmIP keys) becomes another file (Store.File).
//
// All files share the same rules, chosen for a Raspberry Pi's SD card
// and for robustness:
//
//   - every write is atomic (temporary file in the same directory,
//     fsync, rename, fsync of the directory), so a power cut leaves the
//     old or the new version;
//   - nothing is written if the content did not change;
//   - files are created with mode 0600, the directory with 0700;
//   - symbolic links at the files' paths are never followed;
//   - every file is a JSON object with a "version" field; older
//     versions go through a migration hook;
//   - a file that cannot be used is moved aside to
//     <name>.corrupt-<time> (the newest three are kept), and hmbridge
//     continues without it: a damaged file never blocks startup.
package store

import (
	"log"
	"path/filepath"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
)

// File names inside the state directory.
const (
	StateFileName   = config.StateFileName
	DevicesFileName = config.DevicesFileName
)

// StateVersion is the format version of state.json (its content is
// defined by the bridge).
const StateVersion = 1

// Options configures Open.
type Options struct {
	// Dir is the state directory ("" = none: files without an explicit
	// path are disabled).
	Dir string
	// StatePath and DevicesPath override the files' locations (the
	// deprecated state_file and devices_file options); nil means
	// <Dir>/<name>, a pointer to "" disables the file.
	StatePath, DevicesPath *string
	// LegacyStatePath and LegacyDevicesPath are former default
	// locations, copied into place once if the file does not exist yet.
	LegacyStatePath, LegacyDevicesPath string
	Logf                               func(string, ...any)
	Now                                func() time.Time
}

// Store is hmbridge's state directory.
type Store struct {
	dir      string
	logf     func(string, ...any)
	now      func() time.Time
	state    *File
	devices  *File
	settings *File // nil without a state directory
	control  *File // nil without a state directory
}

// ControlFileName is the file of the heating control state.
const ControlFileName = "control.json"

// ControlVersion is the format version of control.json (its content is
// defined by the bridge).
const ControlVersion = 1

// Open sets up the store and copies files from their legacy locations
// (see FileSpec.Legacy). It never fails: problems are logged, and the
// affected file is reported again when it is read or written. The
// state directory is created on the first write.
func Open(o Options) *Store {
	s := &Store{dir: o.Dir, logf: o.Logf, now: o.Now}
	if s.logf == nil {
		s.logf = log.Printf
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.state = s.file(FileSpec{Name: StateFileName, Version: StateVersion, Legacy: o.LegacyStatePath}, o.StatePath)
	s.devices = s.file(FileSpec{Name: DevicesFileName, Version: DevicesVersion, Indent: true, Legacy: o.LegacyDevicesPath}, o.DevicesPath)
	s.settings = s.File(FileSpec{Name: SettingsFileName, Version: SettingsVersion})
	s.control = s.File(FileSpec{Name: ControlFileName, Version: ControlVersion})
	return s
}

func (s *Store) file(spec FileSpec, override *string) *File {
	if override != nil {
		if *override == "" {
			return nil
		}
		spec.Path = *override
		spec.Legacy = "" // an explicit path is used as is
	}
	return s.File(spec)
}

// Legacy default locations (before state_dir), copied into state_dir
// on the first start. Variables for tests.
var (
	LegacyStateFile   = filepath.Join(config.DefaultStateDir, StateFileName)
	LegacyDevicesFile = filepath.Join(config.DefaultStateDir, DevicesFileName)
)

// OpenConfig opens the store described by cfg (state_dir and the
// deprecated state_file and devices_file).
func OpenConfig(cfg *config.Config, logf func(string, ...any)) *Store {
	return Open(Options{
		Dir:               cfg.StateDirPath(),
		StatePath:         cfg.StateFile,
		DevicesPath:       cfg.DevicesFile,
		LegacyStatePath:   LegacyStateFile,
		LegacyDevicesPath: LegacyDevicesFile,
		Logf:              logf,
	})
}

// Dir returns the state directory ("" = none).
func (s *Store) Dir() string { return s.dir }

// File returns the file described by spec (nil if it has neither a
// path nor a state directory to live in), after copying spec.Legacy
// into place if needed.
func (s *Store) File(spec FileSpec) *File {
	path := spec.Path
	if path == "" {
		if s.dir == "" {
			return nil
		}
		path = filepath.Join(s.dir, spec.Name)
	}
	f := &File{spec: spec, path: path, logf: s.logf, now: s.now}
	f.migrateLegacy()
	return f
}

// State returns the device state file (nil = disabled).
func (s *Store) State() *File { return s.state }

// Control returns the heating control state file (nil = no state
// directory).
func (s *Store) Control() *File { return s.control }

// DevicesFile returns the paired devices file (nil = pairing disabled).
func (s *Store) DevicesFile() *File { return s.devices }
