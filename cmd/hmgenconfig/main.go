// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Command hmgenconfig generates an hmbridge configuration from a running
// OpenCCU (RaspberryMatic).
//
// Run it on OpenCCU's Pi while OpenCCU is running. It only reads, never
// writes anything on OpenCCU, and never reads key material:
//
//   - /etc/config/ids: the central's BidCoS address
//   - /etc/config/rfd/*.dev: address, type, serial and AES key index of
//     every paired BidCoS device
//   - rfd's XML-RPC API (http://127.0.0.1:2001/, listDevices): which
//     channels have AES enabled
//   - ReGaHss (http://127.0.0.1:8183/rega.exe): the device names
//   - /etc/config/groups.gson: the heating groups
//   - /var/hm_mode: the radio module type
//
// Every source is optional; what is missing is reported on stderr and
// left for the user to fill in. The configuration is printed to stdout
// (or -out). The security passphrase and the MQTT settings are
// placeholders ("CHANGE-ME") that must be filled in: hmbridge (and its
// -check_config) rejects a configuration that still contains them.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/fjaeckel/hmgo/internal/buildinfo"
	"github.com/fjaeckel/hmgo/licenses"
)

var (
	rfdDir      = flag.String("rfd", "/etc/config/rfd", "directory with rfd's *.dev device files (\"\" disables)")
	idsPath     = flag.String("ids", "/etc/config/ids", "OpenCCU's ids file with the central address (\"\" disables)")
	hmModePath  = flag.String("hm_mode", "/var/hm_mode", "OpenCCU's radio module description (\"\" disables)")
	xmlrpcURL   = flag.String("xmlrpc", "http://127.0.0.1:2001/", "rfd's XML-RPC URL, for the AES settings (\"\" disables)")
	regaURL     = flag.String("rega", "http://127.0.0.1:8183/rega.exe", "ReGaHss script URL, for the device names (\"\" disables)")
	groupsPath  = flag.String("groups", "/etc/config/groups.gson", "OpenCCU's groups file, for the heating groups (\"\" disables)")
	outPath     = flag.String("out", "", "write the configuration to this file instead of stdout")
	dump        = flag.Bool("dump", false, "also print the raw collected inputs to stderr (for bug reports; contains no keys)")
	timeout     = flag.Duration("timeout", 30*time.Second, "timeout for the XML-RPC and ReGaHss requests")
	showLicense = flag.Bool("license", false, "print the licenses and exit")
	showVersion = flag.Bool("version", false, "print the version and exit")
)

func version() string { return buildinfo.Version() }

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: hmgenconfig [flags] > config.json

Generates an hmbridge configuration from OpenCCU. Run it on OpenCCU's Pi
while OpenCCU is running (device names come from ReGaHss, AES settings
from rfd). Nothing on OpenCCU is changed, and no keys are read.

Afterwards fill in security.passphrase (OpenCCU's system security key)
and the mqtt settings (the CHANGE-ME placeholders; hmbridge rejects them),
review the devices and groups, and check the result with:
hmbridge -config config.json -check_config

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showLicense {
		fmt.Print(licenses.Text())
		return
	}
	if *showVersion {
		fmt.Println("hmgenconfig", version())
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hmgenconfig:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	src := Sources{
		RfdDir:     *rfdDir,
		IDsPath:    *idsPath,
		HMModePath: *hmModePath,
		XMLRPCURL:  *xmlrpcURL,
		ReGaURL:    *regaURL,
		GroupsPath: *groupsPath,
		Client:     &http.Client{Timeout: *timeout},
		Log:        os.Stderr,
	}
	if *dump {
		src.Dump = os.Stderr
	}
	out, err := Generate(ctx, src)
	if err != nil {
		return err
	}
	if *outPath == "" {
		_, err = os.Stdout.Write(out)
		return err
	}
	// 0600: the file will hold the passphrase once filled in.
	if err := os.WriteFile(*outPath, out, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "hmgenconfig: configuration written to %s\n", *outPath)
	return nil
}
