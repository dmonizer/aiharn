package shell

import "strings"

// ShellQuote single-quotes s so it is treated as one literal argument when the
// shell evals the command.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellArgs returns the arguments that start shell in stdin-reading,
// profile-less mode. Bash gets --noprofile --norc -s; other shells get plain -s.
func ShellArgs(shell string) []string {
	if strings.HasSuffix(shell, "bash") {
		return []string{"--noprofile", "--norc", "-s"}
	}
	return []string{"-s"}
}

// ShellCommand returns the single-string remote command that starts the
// persistent shell. It is the SSH form of ShellArgs: the shell path is quoted
// for evaluation by the remote login shell.
func ShellCommand(shell string) string {
	quoted := ShellQuote(shell)
	if strings.HasSuffix(shell, "bash") {
		return quoted + " --noprofile --norc -s"
	}
	return quoted + " -s"
}
