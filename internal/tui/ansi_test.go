package tui

import "testing"

func TestAnsiEndCSI(t *testing.T) {
	s := "\x1b[31m"
	if got := ansiEnd(s, 0); got != len(s) {
		t.Fatalf("ansiEnd(CSI) = %d, want %d", got, len(s))
	}
}

func TestAnsiEndPlainEsc(t *testing.T) {
	s := "\x1bXrest"
	if got := ansiEnd(s, 0); got != 2 {
		t.Fatalf("ansiEnd(plain) = %d, want 2", got)
	}
}

func TestAnsiEndOSCBEL(t *testing.T) {
	s := "\x1b]8;;url\x07rest"
	if got := ansiEnd(s, 0); got != 9 {
		t.Fatalf("ansiEnd(OSC BEL) = %d, want 9", got)
	}
}

func TestAnsiEndOSCST(t *testing.T) {
	s := "\x1b]8;;url\x1b\\rest"
	if got := ansiEnd(s, 0); got != 10 {
		t.Fatalf("ansiEnd(OSC ST) = %d, want 10", got)
	}
}

func TestAnsiEndOSCUnterminated(t *testing.T) {
	s := "\x1b]8;;url"
	if got := ansiEnd(s, 0); got != len(s) {
		t.Fatalf("ansiEnd(unterminated) = %d, want %d", got, len(s))
	}
}
