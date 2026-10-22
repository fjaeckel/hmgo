// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

//go:build unix

package config

import (
	"fmt"
	"io/fs"
	"os"
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
