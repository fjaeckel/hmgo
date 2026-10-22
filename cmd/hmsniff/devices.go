package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DeviceInfo is what OpenCCU's rfd knows about a paired device.
type DeviceInfo struct {
	Serial   string
	Type     string
	Addr     [3]byte
	KeyIndex string
}

func (d DeviceInfo) String() string { return d.Type + " " + d.Serial }

var attrRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// loadDevices reads the first line (the <device …> element) of every
// *.dev file in dir, e.g. /etc/config/rfd on OpenCCU. Missing or
// unreadable files are skipped.
func loadDevices(dir string) map[[3]byte]DeviceInfo {
	res := make(map[[3]byte]DeviceInfo)
	files, _ := filepath.Glob(filepath.Join(dir, "*.dev"))
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		if sc.Scan() {
			if di, ok := parseDeviceLine(sc.Text()); ok {
				res[di.Addr] = di
			}
		}
		f.Close()
	}
	return res
}

func parseDeviceLine(line string) (DeviceInfo, bool) {
	var di DeviceInfo
	attrs := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(line, -1) {
		attrs[m[1]] = m[2]
	}
	a, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(attrs["address"]), "0x"), 16, 32)
	if err != nil || a == 0 || a > 0xFFFFFF {
		return di, false
	}
	di.Addr = [3]byte{byte(a >> 16), byte(a >> 8), byte(a)}
	di.Serial = attrs["serial"]
	di.Type = attrs["type"]
	di.KeyIndex = attrs["aes_key_index"]
	return di, true
}
