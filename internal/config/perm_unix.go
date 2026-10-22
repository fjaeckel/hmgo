// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package config

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// permissionWarnings checks that the configuration file at path, which
// holds the AES system key and the MQTT password, is readable only by
// root and hmbridge: not world-readable or writable, not writable by
// its group, readable by its group only if hmbridge belongs to it, and
// owned by root or by hmbridge's user.
func permissionWarnings(path string, fi fs.FileInfo) []string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return checkPermissions(path, fi.Mode().Perm(), int(st.Uid), int(st.Gid), os.Geteuid(), ownGroups())
}

// ownGroups returns the process's effective and supplementary groups.
func ownGroups() map[int]bool {
	g := map[int]bool{os.Getegid(): true}
	if gs, err := os.Getgroups(); err == nil {
		for _, id := range gs {
			g[id] = true
		}
	}
	return g
}

func checkPermissions(path string, perm fs.FileMode, uid, gid, euid int, groups map[int]bool) []string {
	const fix = "it holds the AES system key and the MQTT password; fix with: install -m 0640 -o root -g hmbridge, or chmod 0640 and chown root:hmbridge"
	var w []string
	switch {
	case perm&0o002 != 0:
		w = append(w, fmt.Sprintf("%s is writable by every user (mode %04o); %s", path, perm, fix))
	case perm&0o004 != 0:
		w = append(w, fmt.Sprintf("%s is readable by every user (mode %04o); %s", path, perm, fix))
	}
	switch {
	case perm&0o020 != 0:
		w = append(w, fmt.Sprintf("%s is writable by its group (gid %d, mode %04o); %s", path, gid, perm, fix))
	case perm&0o040 != 0 && !groups[gid]:
		w = append(w, fmt.Sprintf("%s is readable by group %d, which hmbridge does not belong to (mode %04o); %s", path, gid, perm, fix))
	}
	if uid != 0 && uid != euid {
		w = append(w, fmt.Sprintf("%s is owned by another user (uid %d), who can read and change it; %s", path, uid, fix))
	}
	return w
}

// keyFilePermissionWarnings checks that a key file (security.key_file)
// is private to its owner: no permissions for its group or other users
// (hmbridge init creates it with mode 0600, owned by the hmbridge
// user), and owned by root or by hmbridge's user.
func keyFilePermissionWarnings(path string, fi fs.FileInfo) []string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return checkKeyFilePermissions(path, fi.Mode().Perm(), int(st.Uid), os.Geteuid(), serviceUID())
}

// ServiceUser is the system user hmbridge runs as (hmbridge init's
// default -user).
const ServiceUser = "hmbridge"

// serviceUID returns the uid of ServiceUser, or -1 if there is none.
func serviceUID() int {
	u, err := user.Lookup(ServiceUser)
	if err != nil {
		return -1
	}
	id, err := strconv.Atoi(u.Uid)
	if err != nil {
		return -1
	}
	return id
}

// checkKeyFilePermissions: the key file must be private to its owner,
// who must be root or the user hmbridge runs as (euid). Run as root
// (e.g. -check_config before the service starts), the owner must be
// root or ServiceUser (svcUID, -1 if there is none).
func checkKeyFilePermissions(path string, perm fs.FileMode, uid, euid, svcUID int) []string {
	const fix = "it holds the AES system key; fix with: chmod 0600 and chown hmbridge (the user hmbridge runs as)"
	var w []string
	if perm&0o077 != 0 {
		w = append(w, fmt.Sprintf("%s is accessible by its group or other users (mode %04o); %s", path, perm, fix))
	}
	switch {
	case uid == 0:
	case euid != 0 && uid != euid:
		w = append(w, fmt.Sprintf("%s is owned by another user (uid %d), who can read and change it; %s", path, uid, fix))
	case euid == 0 && uid != svcUID:
		w = append(w, fmt.Sprintf("%s is owned by uid %d, neither root nor the user %s, who can read and change it; %s", path, uid, ServiceUser, fix))
	}
	return w
}

// openKeyFile opens a key file for reading. Only a regular file is
// opened: a symbolic link, FIFO, device or directory is refused before
// it is opened (Lstat), and the open neither follows a link put there
// meanwhile (O_NOFOLLOW) nor blocks on a FIFO (O_NONBLOCK); the opened
// file must be the one checked.
func openKeyFile(path string) (*os.File, fs.FileInfo, error) {
	lfi, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if lfi.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%s is a symbolic link (not followed: name the key file itself)", path)
	}
	if !lfi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err == nil && (!fi.Mode().IsRegular() || !os.SameFile(lfi, fi)) {
		err = fmt.Errorf("%s was replaced while opening it", path)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}
