package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"aiharn/internal/authfile"
	"golang.org/x/term"
)

func runAddUser(args []string) int {
	fs := flag.NewFlagSet("add-user", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("file", ".aiharn-users", "password file outside the web directory")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: aiharn add-user [--file PATH] USERNAME")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: aiharn add-user [--file PATH] USERNAME")
		return 2
	}
	if !authfile.ValidUsername(fs.Arg(0)) {
		fmt.Fprintln(os.Stderr, "aiharn: username must be 1-64 letters, digits, dots, dashes, or underscores")
		return 2
	}
	abs, err := filepath.Abs(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: resolve user file: %v\n", err)
		return 1
	}
	if pathInsideWeb(abs) {
		fmt.Fprintln(os.Stderr, "aiharn: user file must be outside the web directory")
		return 2
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "aiharn: add-user requires an interactive terminal")
		return 2
	}
	fmt.Fprint(os.Stderr, "Password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: read password: %v\n", err)
		return 1
	}
	defer clearBytes(password)
	fmt.Fprint(os.Stderr, "Confirm password: ")
	confirmation, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: read confirmation: %v\n", err)
		return 1
	}
	defer clearBytes(confirmation)
	if !bytes.Equal(password, confirmation) {
		fmt.Fprintln(os.Stderr, "aiharn: passwords do not match")
		return 2
	}
	if err := authfile.Set(abs, fs.Arg(0), password); err != nil {
		fmt.Fprintf(os.Stderr, "aiharn: add user: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "Saved user %s in %s\n", fs.Arg(0), abs)
	return 0
}

func clearBytes(data []byte) {
	for i := range data {
		data[i] = 0
	}
}

func pathInsideWeb(path string) bool {
	web, err := filepath.Abs("web")
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(web, path)
	return err == nil && rel != ".." && rel != "." && (len(rel) < 3 || rel[:3] != "../")
}
