package ssh

import "testing"

func TestInterpreterCommand(t *testing.T) {
	// RemoteCommand wins verbatim over the default shell.
	if got := interpreterCommand(Options{DefaultShell: "/bin/bash", RemoteCommand: "bash"}); got != "bash" {
		t.Fatalf("interpreterCommand with RemoteCommand = %q, want %q", got, "bash")
	}
	// A multi-word RemoteCommand is passed through untouched.
	if got := interpreterCommand(Options{RemoteCommand: "bash --noprofile"}); got != "bash --noprofile" {
		t.Fatalf("interpreterCommand multi-word = %q", got)
	}
	// With no RemoteCommand, the default shell is started in stdin mode.
	if got := interpreterCommand(Options{DefaultShell: "/bin/bash"}); got != "'/bin/bash' --noprofile --norc -s" {
		t.Fatalf("interpreterCommand default = %q", got)
	}
	// A non-bash default shell gets plain -s, no bash-specific flags.
	if got := interpreterCommand(Options{DefaultShell: "/bin/sh"}); got != "'/bin/sh' -s" {
		t.Fatalf("interpreterCommand sh = %q", got)
	}
}
