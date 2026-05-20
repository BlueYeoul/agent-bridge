package remotesync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotAndDiff(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "pkg"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	filePath := filepath.Join(root, "pkg", "app.go")
	if err := os.WriteFile(filePath, []byte("package pkg\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	before, err := Snapshot(root)
	if err != nil {
		t.Fatalf("Snapshot(before) error = %v", err)
	}
	if err := os.WriteFile(filePath, []byte("package pkg\n\nconst Name = \"agent\"\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# hello\n"), 0o600); err != nil {
		t.Fatalf("write readme: %v", err)
	}

	after, err := Snapshot(root)
	if err != nil {
		t.Fatalf("Snapshot(after) error = %v", err)
	}
	changed, deleted := Diff(before, after)
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none", deleted)
	}
	if !contains(changed, "pkg/app.go") || !contains(changed, "README.md") {
		t.Fatalf("changed = %v, want modified app.go and added README.md", changed)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
