package cli

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseMixedFlags(t *testing.T) {
	cfg, err := Parse([]string{
		"./workspace/my-project",
		"--target", "devbox",
		"--launch", "claude",
		"--port=2222",
		"--", "--debug",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := Config{
		RemotePath: "./workspace/my-project",
		Target:     "devbox",
		Launch:     "claude",
		Port:       2222,
		PortSet:    true,
		LaunchArgs: []string{"--debug"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Parse() = %#v, want %#v", cfg, want)
	}
}

func TestParseRequiresFields(t *testing.T) {
	_, err := Parse([]string{"./workspace"})
	if err == nil {
		t.Fatal("Parse() expected error")
	}
}

func TestParseHelp(t *testing.T) {
	_, err := Parse([]string{"--help"})
	if !errors.Is(err, ErrHelp) {
		t.Fatalf("Parse() error = %v, want ErrHelp", err)
	}
}
