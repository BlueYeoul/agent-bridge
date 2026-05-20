package sshx

import "testing"

func TestShellQuote(t *testing.T) {
	got := ShellQuote("a b'c")
	want := `'a b'"'"'c'`
	if got != want {
		t.Fatalf("ShellQuote() = %q, want %q", got, want)
	}
}
