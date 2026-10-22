// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// File permissions: the files may hold key material in the future
// (HmIP keys), so they are private to the hmbridge user from the start.
const (
	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600
)

// MaxCorrupt is how many quarantined copies of a file are kept.
const MaxCorrupt = 3

// maxFileSize bounds what is read into memory (a sane file is a few
// kB; anything this large is not ours).
const maxFileSize = 16 << 20

// MigrateFunc converts the raw JSON of a file written with format
// version from (< the current version) to the current version.
type MigrateFunc func(from int, data []byte) ([]byte, error)

// FileSpec describes one file in the state directory.
type FileSpec struct {
	// Name is the file name inside the state directory (e.g.
	// "devices.json").
	Name string
	// Path overrides the location (the deprecated state_file and
	// devices_file options). "" means <state dir>/<Name>.
	Path string
	// Version is the current format version. Every file is a JSON
	// object with a "version" field (the envelope); the data lives in
	// the other fields.
	Version int
	// Migrate upgrades older versions; nil means older versions are
	// not readable (the file is quarantined).
	Migrate MigrateFunc
	// Indent writes the file indented (for files people read).
	Indent bool
	// Legacy is a former location of the file: if the file does not
	// exist yet but Legacy does, Legacy is copied into place once
	// (and left where it is).
	Legacy string
}

// File is one JSON file of the store. Its methods are safe for
// concurrent use.
type File struct {
	spec FileSpec
	path string
	logf func(string, ...any)
	now  func() time.Time
	mu   sync.Mutex
	// blocked is why the file must not be written (a newer format
	// version, a link, a damaged file that could not be moved aside);
	// nil if writes are fine.
	blocked error
}

// Path returns the file's location.
func (f *File) Path() string { return f.path }

// ErrSymlink is returned for a file that is a symbolic link: hmbridge
// never follows links for its own files (a link planted in the state
// directory must not make it read or overwrite something else).
var ErrSymlink = errors.New("is a symbolic link (refusing to follow it)")

// ErrNewerVersion is returned for a file written by a newer hmbridge
// (after a downgrade). Such a file is left alone and never overwritten:
// hmbridge continues without its data, and Save fails until the file is
// removed or hmbridge is upgraded again.
var ErrNewerVersion = errors.New("was written by a newer hmbridge")

// Blocked returns why the file is not written (nil if it is): see
// ErrNewerVersion and ErrSymlink, or a damaged file that could not be
// moved aside. Writing would destroy data hmbridge could not read.
func (f *File) Blocked() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blocked
}

// CorruptError reports a file that could not be used and has been moved
// aside (Quarantined is "" if even that failed).
type CorruptError struct {
	Path        string
	Quarantined string
	Err         error
}

func (e *CorruptError) Error() string {
	if e.Quarantined == "" {
		return fmt.Sprintf("%s: %v; continuing without it", e.Path, e.Err)
	}
	return fmt.Sprintf("%s: %v; moved it to %s, continuing without it", e.Path, e.Err, e.Quarantined)
}

func (e *CorruptError) Unwrap() error { return e.Err }

// Load reads the file into v. A missing file is no error (found is
// false). A file that cannot be used — unreadable, not JSON, a missing
// or unsupported old version, a failed migration — is moved aside to
// <name>.corrupt-<time> (the newest MaxCorrupt are kept) and reported
// as a *CorruptError, so that the caller logs it and continues without
// the data: a damaged file never blocks startup, and is never
// overwritten in place. A symbolic link (ErrSymlink) and a file of a
// newer format version (ErrNewerVersion) are left alone and block
// writes (see Blocked). On error v may be partially filled; callers
// should discard it.
func (f *File) Load(v any) (found bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = nil
	if _, err := os.Lstat(f.path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		// Can't even look at it (e.g. a path component is no
		// directory): nothing to move aside; writes will fail too.
		return false, err
	}
	data, err := readNoFollow(f.path)
	if errors.Is(err, ErrSymlink) {
		f.blocked = fmt.Errorf("%s %w", f.path, err)
		return false, f.blocked
	}
	if err == nil {
		err = f.decode(data, v)
	}
	if errors.Is(err, ErrNewerVersion) {
		f.blocked = fmt.Errorf("%s %w (%v); it is left alone and not written until you remove it or upgrade hmbridge", f.path, ErrNewerVersion, err)
		return false, f.blocked
	}
	if err != nil {
		return false, f.quarantine(err)
	}
	return true, nil
}

func (f *File) decode(data []byte, v any) error {
	var env struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	switch {
	case env.Version == nil:
		return errors.New("no format version")
	case *env.Version == f.spec.Version:
	case *env.Version > f.spec.Version:
		return fmt.Errorf("%w: format version %d, this hmbridge supports %d", ErrNewerVersion, *env.Version, f.spec.Version)
	case f.spec.Migrate == nil || *env.Version < 1:
		return fmt.Errorf("unsupported format version %d (want %d)", *env.Version, f.spec.Version)
	default:
		from := *env.Version
		var err error
		if data, err = f.spec.Migrate(from, data); err != nil {
			return fmt.Errorf("migrating from format version %d: %v", from, err)
		}
		if got := versionOf(data); got != f.spec.Version {
			return fmt.Errorf("migrating from format version %d yielded version %d", from, got)
		}
	}
	return json.Unmarshal(data, v)
}

// versionOf returns the envelope version of data (-1 if none).
func versionOf(data []byte) int {
	var env struct {
		Version *int `json:"version"`
	}
	if json.Unmarshal(data, &env) != nil || env.Version == nil {
		return -1
	}
	return *env.Version
}

// quarantine moves the unusable file aside. Caller holds f.mu.
func (f *File) quarantine(cause error) error {
	ce := &CorruptError{Path: f.path, Err: cause}
	// Millisecond timestamps sort chronologically; quarantines within
	// the same millisecond get a counter after the highest one in use.
	stamp := f.now().UTC().Format("20060102T150405.000Z")
	q := f.path + ".corrupt-" + stamp
	if matches, _ := filepath.Glob(globEscape(q) + "*"); len(matches) > 0 {
		n := 0
		for _, m := range matches {
			var i int
			if _, err := fmt.Sscanf(strings.TrimPrefix(m, q), "-%d", &i); err == nil && i > n {
				n = i
			}
		}
		q = fmt.Sprintf("%s-%03d", q, n+1)
	}
	if err := os.Rename(f.path, q); err != nil {
		ce.Err = fmt.Errorf("%v (moving it aside failed: %v)", cause, err)
		f.blocked = ce // never overwrite it in place
		return ce
	}
	ce.Quarantined = q
	syncDir(filepath.Dir(f.path))
	pruneCorrupt(f.path)
	return ce
}

// pruneCorrupt removes all but the newest MaxCorrupt quarantined copies
// of path. The names sort chronologically.
func pruneCorrupt(path string) {
	matches, err := filepath.Glob(globEscape(path) + ".corrupt-*")
	if err != nil || len(matches) <= MaxCorrupt {
		return
	}
	sort.Strings(matches)
	for _, m := range matches[:len(matches)-MaxCorrupt] {
		os.Remove(m)
	}
}

func globEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '*', '?', '[', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Save writes v (a struct or map marshalling to a JSON object whose
// "version" is the spec's version) atomically. If the file already has
// exactly these bytes, nothing is written (written is false): the SD
// card is spared. A symbolic link at the path is refused, and so is a
// blocked file (see Blocked). A file that has become unreadable since
// it was loaded is moved aside first, never overwritten in place.
func (f *File) Save(v any) (written bool, err error) {
	var data []byte
	if f.spec.Indent {
		data, err = json.MarshalIndent(v, "", " ")
		data = append(data, '\n')
	} else {
		data, err = json.Marshal(v) // compact: fewer bytes written
	}
	if err != nil {
		return false, err
	}
	if got := versionOf(data); got != f.spec.Version {
		return false, fmt.Errorf("%s: refusing to write format version %d (want %d)", f.path, got, f.spec.Version)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked != nil {
		return false, fmt.Errorf("not writing: %w", f.blocked)
	}
	old, err := readNoFollow(f.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case errors.Is(err, ErrSymlink):
		return false, fmt.Errorf("%s %w", f.path, err)
	case err == nil && bytes.Equal(old, data):
		return false, nil
	case err == nil && versionOf(old) > f.spec.Version:
		f.blocked = fmt.Errorf("%s %w (format version %d); it is left alone and not written until you remove it or upgrade hmbridge", f.path, ErrNewerVersion, versionOf(old))
		return false, f.blocked
	case err != nil:
		if _, lerr := os.Lstat(f.path); lerr != nil {
			return false, lerr // can't look at it: the write would fail too
		}
		// Exists but unreadable: keep it rather than overwrite it.
		qerr := f.quarantine(err)
		if f.blocked != nil {
			return false, qerr
		}
		f.logf("%v", qerr)
	case versionOf(old) < 0:
		// Damaged since it was loaded: keep a copy.
		qerr := f.quarantine(errors.New("no format version"))
		if f.blocked != nil {
			return false, qerr
		}
		f.logf("%v", qerr)
	}
	if err := WriteFileAtomic(f.path, data); err != nil {
		return false, err
	}
	return true, nil
}

// ModTime returns when the file was last written (zero if unknown).
func (f *File) ModTime() time.Time {
	fi, err := os.Lstat(f.path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// migrateLegacy copies spec.Legacy to the file's path if the file does
// not exist yet. The old file is left in place.
func (f *File) migrateLegacy() {
	src := f.spec.Legacy
	if src == "" || filepath.Clean(src) == filepath.Clean(f.path) {
		return
	}
	if _, err := os.Lstat(f.path); !errors.Is(err, os.ErrNotExist) {
		return // exists (or can't tell): never overwrite
	}
	data, err := readNoFollow(src)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		f.logf("not copying %s to %s: %v", src, f.path, err)
		return
	}
	if err := WriteFileAtomic(f.path, data); err != nil {
		f.logf("copying %s to %s: %v", src, f.path, err)
		return
	}
	f.logf("copied %s to %s (the new location in state_dir); the old file is left in place and no longer used, delete it once hmbridge runs fine", src, f.path)
}

// readNoFollow reads a regular file without following a symbolic link
// at path itself.
func readNoFollow(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, ErrSymlink
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, ErrSymlink // replaced by a link since the Lstat
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if fi, err := file.Stat(); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("larger than %d bytes", maxFileSize)
	}
	return data, nil
}

// WriteFileAtomic replaces path with data so that a power cut leaves
// either the old or the new content: a temporary file in the same
// directory (mode 0600) is written and fsynced, renamed over path, and
// the directory is fsynced. The directory is created (mode 0700) if
// missing. A symbolic link at path is refused.
func WriteFileAtomic(path string, data []byte) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s %w", path, ErrSymlink)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	err = tmp.Chmod(FileMode)   // CreateTemp uses 0600 already; be explicit
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename in dir durable (best effort: some file
// systems do not support fsync on directories).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// NewFileOption is an option of WriteFileNew.
type NewFileOption func(*newFileOptions)

type newFileOptions struct {
	uid, gid int
	mode     os.FileMode
}

// WithOwner makes WriteFileNew give the file the owner uid and group
// gid (-1 keeps one), set on the temporary file before it gets its
// name (fchown: no path is followed).
func WithOwner(uid, gid int) NewFileOption {
	return func(o *newFileOptions) { o.uid, o.gid = uid, gid }
}

// WithMode makes WriteFileNew create the file with mode instead of
// FileMode.
func WithMode(mode os.FileMode) NewFileOption {
	return func(o *newFileOptions) { o.mode = mode }
}

// WriteFileNew creates path with data (mode 0600, see WithMode and
// WithOwner) only if nothing exists at path yet; otherwise it fails with
// an error matching fs.ErrExist and leaves the existing file alone. Like
// WriteFileAtomic, the content is written to a temporary file in the
// same directory and fsynced first, so that path never exists with
// partial content; the temporary file gets its owner and mode before it
// is hard-linked to path, which fails atomically if path exists (also
// as a symbolic link). The directory is created (mode 0700) if missing.
func WriteFileNew(path string, data []byte, opts ...NewFileOption) error {
	o := newFileOptions{uid: -1, gid: -1, mode: FileMode}
	for _, opt := range opts {
		opt(&o)
	}
	if _, err := os.Lstat(path); err == nil {
		return &os.PathError{Op: "create", Path: path, Err: fs.ErrExist}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if o.uid >= 0 || o.gid >= 0 {
		err = tmp.Chown(o.uid, o.gid)
	}
	if err == nil {
		err = tmp.Chmod(o.mode)
	}
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		var le *os.LinkError
		if errors.As(err, &le) && errors.Is(le.Err, fs.ErrExist) {
			return &os.PathError{Op: "create", Path: path, Err: fs.ErrExist}
		}
		return err
	}
	syncDir(dir)
	return nil
}
