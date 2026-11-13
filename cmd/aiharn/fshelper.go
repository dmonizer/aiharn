package main

import (
	"flag"
	"fmt"
	"os"

	"aiharn/internal/execution/fscore"
	"aiharn/internal/execution/fshelper"
	"aiharn/internal/execution/fsproto"
)

// runFSHelper runs the remote filesystem helper. The ssh transport starts this
// subcommand on the execution host and speaks fsproto over the channel's
// stdin/stdout, so stdout carries protocol frames only and every diagnostic
// goes to stderr. It exits non-zero on a protocol or transport failure so the
// client reports an unsupported or I/O failure instead of retrying.
func runFSHelper(args []string) int {
	flags := flag.NewFlagSet("fs-helper", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	showVersion := flags.Bool("version", false, "print the helper version and exit")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: aiharn fs-helper [--version]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "aiharn: fs-helper: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *showVersion {
		fmt.Fprintf(os.Stdout, "fshelper %s\n", fsproto.HelperVersion)
		return 0
	}
	if err := fshelper.Serve(os.Stdin, os.Stdout, openFSHelperBackend); err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: fs-helper: %v\n", err)
		return 1
	}
	return 0
}

// openFSHelperBackend opens the confined filesystem for the root named in the
// init frame.
func openFSHelperBackend(root string) (fshelper.Backend, error) {
	service, err := fscore.Open(root)
	if err != nil {
		return nil, err
	}
	return service, nil
}
