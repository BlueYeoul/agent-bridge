package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlueYeoul/agent-bridge/internal/cli"
)

func TestControlMasterUnsupported(t *testing.T) {
	cases := []string{
		"ControlMaster is unsupported on this platform",
		"mux client is not supported",
	}
	for _, tc := range cases {
		if !controlMasterUnsupported(tc) {
			t.Fatalf("controlMasterUnsupported(%q) = false, want true", tc)
		}
	}

	if controlMasterUnsupported("Permission denied, please try again.") {
		t.Fatal("auth failure should not be treated as unsupported ControlMaster")
	}
}

func TestLocalMirrorPathIncludesTargetAndRemotePath(t *testing.T) {
	r := &Runner{
		cfg:           cli.Config{Target: "user@example.com"},
		workspaceRoot: filepath.Join("/tmp", "agtbge", "mirrors", "session_abcd"),
	}
	got := filepath.ToSlash(r.localMirrorPath("/home/perseverance/2026/test"))
	if !strings.Contains(got, "user_example.com/home/perseverance/2026/test") {
		t.Fatalf("localMirrorPath() = %q", got)
	}
}

func TestAgentBridgePromptSeparatesFileToolAndRemotePaths(t *testing.T) {
	r := &Runner{
		cfg:        cli.Config{Target: "univ"},
		remoteRoot: "/home/perseverance/2026/test",
		workspace:  filepath.Join("/tmp", "agtbge", "mirrors", "session_abcd", "univ", "home", "perseverance", "2026", "test"),
	}
	prompt := r.agentBridgePrompt()
	for _, want := range []string{
		"File tools",
		"Never pass the canonical remote absolute path",
		"read ./pyproject.toml",
		"Shell commands execute on the SSH target",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("agentBridgePrompt() missing %q:\n%s", want, prompt)
		}
	}
}
