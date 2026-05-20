package remotesync

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

func TestSaveStateConcurrentWriters(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "session", "state.json")
	const writers = 16

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- SaveState(statePath, State{Entries: map[string]Entry{
				"README.md": {Type: "file", Size: 12},
			}})
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("SaveState concurrent error = %v", err)
		}
	}
	state, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState error = %v", err)
	}
	if _, ok := state.Entries["README.md"]; !ok {
		t.Fatalf("state entries = %v, want README.md", state.Entries)
	}
}

func TestDiffRemoteUsesMetadataWithoutHash(t *testing.T) {
	previous := State{Entries: map[string]Entry{
		"main.py": {Type: "file", Size: 10, Mode: 0o644, ModTime: int64(100 * time.Second), Hash: "local-only"},
		"old.py":  {Type: "file", Size: 5, Mode: 0o644, ModTime: int64(100 * time.Second)},
	}}
	remote := State{Entries: map[string]Entry{
		"main.py": {Type: "file", Size: 10, Mode: 0o644, ModTime: int64(100*time.Second) + 500},
		"new.py":  {Type: "file", Size: 7, Mode: 0o644, ModTime: int64(200 * time.Second)},
	}}

	changed, deleted := DiffRemote(previous, remote)
	if contains(changed, "main.py") {
		t.Fatalf("changed = %v, main.py should match by remote metadata", changed)
	}
	if !contains(changed, "new.py") {
		t.Fatalf("changed = %v, want new.py", changed)
	}
	if !contains(deleted, "old.py") {
		t.Fatalf("deleted = %v, want old.py", deleted)
	}
}

func TestParseRemoteManifest(t *testing.T) {
	manifest := []byte("src\x00d\x000\x00755\x00100.0\x00\x00src/main.go\x00f\x0012\x00644\x00101.5\x00\x00link\x00l\x000\x00777\x00102.0\x00src/main.go\x00")
	state, err := parseRemoteManifest(manifest)
	if err != nil {
		t.Fatalf("parseRemoteManifest error = %v", err)
	}
	if state.Entries["src"].Type != "dir" {
		t.Fatalf("src entry = %+v, want dir", state.Entries["src"])
	}
	if state.Entries["src/main.go"].Size != 12 {
		t.Fatalf("src/main.go entry = %+v, want size 12", state.Entries["src/main.go"])
	}
	if state.Entries["link"].Link != "src/main.go" {
		t.Fatalf("link entry = %+v, want symlink target", state.Entries["link"])
	}
}

func TestProjectMirrorStateSkipsLargeFiles(t *testing.T) {
	state := State{Entries: map[string]Entry{
		"src":         {Type: "dir"},
		"src/main.py": {Type: "file", Size: 1024},
		"data.bin":    {Type: "file", Size: maxMirrorFileSize + 1},
		"link":        {Type: "symlink", Link: "src/main.py"},
	}}

	projected := ProjectMirrorState(state)
	if _, ok := projected.Entries["src/main.py"]; !ok {
		t.Fatalf("small source file should stay in projected state")
	}
	if _, ok := projected.Entries["data.bin"]; ok {
		t.Fatalf("large file should not be mirrored")
	}
	if _, ok := projected.Entries["src"]; !ok {
		t.Fatalf("directories should stay in projected state")
	}
	if _, ok := projected.Entries["link"]; !ok {
		t.Fatalf("symlinks should stay in projected state")
	}
}

func TestExcludedDirs(t *testing.T) {
	root := t.TempDir()

	// Create some standard files/folders
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	// Create excluded folders and files inside them
	for _, dir := range ExcludedDirs {
		dirPath := filepath.Join(root, dir)
		if err := os.Mkdir(dirPath, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dirPath, "dummy.txt"), []byte("dummy"), 0o600); err != nil {
			t.Fatalf("write dummy inside %s: %v", dir, err)
		}
	}

	// Test Snapshot exclusions
	state, err := Snapshot(root)
	if err != nil {
		t.Fatalf("Snapshot error = %v", err)
	}

	// Verify "src/main.go" is in snapshot, but none of the excluded dirs/files are
	if _, ok := state.Entries["src/main.go"]; !ok {
		t.Errorf("expected src/main.go in snapshot")
	}

	for _, dir := range ExcludedDirs {
		if _, ok := state.Entries[dir]; ok {
			t.Errorf("found excluded directory %s in snapshot", dir)
		}
		if _, ok := state.Entries[dir+"/dummy.txt"]; ok {
			t.Errorf("found file inside excluded directory %s in snapshot", dir)
		}
	}

	// Test clearDirectory preserves excluded dirs
	if err := clearDirectory(root); err != nil {
		t.Fatalf("clearDirectory error = %v", err)
	}

	// Verify src/main.go is deleted
	if _, err := os.Stat(filepath.Join(root, "src", "main.go")); !os.IsNotExist(err) {
		t.Errorf("expected src/main.go to be deleted, err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src")); !os.IsNotExist(err) {
		t.Errorf("expected src to be deleted, err = %v", err)
	}

	// Verify excluded dirs still exist
	for _, dir := range ExcludedDirs {
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
			t.Errorf("expected excluded directory %s to be preserved, err = %v", dir, err)
		}
		if _, err := os.Stat(filepath.Join(root, dir, "dummy.txt")); err != nil {
			t.Errorf("expected file dummy.txt inside %s to be preserved, err = %v", dir, err)
		}
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
