package remotesync

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BlueYeoul/agent-bridge/internal/sshx"
)

type Entry struct {
	Type    string `json:"type"`
	Size    int64  `json:"size,omitempty"`
	Mode    int64  `json:"mode,omitempty"`
	ModTime int64  `json:"mod_time,omitempty"`
	Hash    string `json:"hash,omitempty"`
	Link    string `json:"link,omitempty"`
}

type State struct {
	Entries map[string]Entry `json:"entries"`
}

const (
	stateLockPoll     = 25 * time.Millisecond
	stateLockStale    = 30 * time.Minute
	stateLockTimeout  = 10 * time.Minute
	maxMirrorFileSize = 5 * 1024 * 1024
)

var inProcessStateLocks sync.Map

var ExcludedDirs = []string{
	".git",
	".cache",
	".venv",
	"venv",
	".gocache",
	"node_modules",
	".agtbge",
	"data",
	"dataset",
	"datasets",
	"outputs",
	"checkpoints",
	"runs",
	"wandb",
	"mlruns",
}

func isExcluded(name string) bool {
	for _, dir := range ExcludedDirs {
		if name == dir {
			return true
		}
	}
	return false
}

func Download(cfg sshx.Config, workspace, remoteRoot, statePath string) error {
	if err := sshx.RequireFields(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	if err := clearDirectory(workspace); err != nil {
		return err
	}

	remote, err := RemoteSnapshot(cfg, remoteRoot)
	if err != nil {
		return err
	}
	projected := ProjectMirrorState(remote)
	rels := sortedStatePaths(projected)
	if err := downloadTar(cfg, workspace, remoteRoot, rels); err != nil {
		return err
	}

	state, err := Snapshot(workspace)
	if err != nil {
		return err
	}
	return withStateLock(statePath, func() error {
		return saveStateUnlocked(statePath, state)
	})
}

func DownloadChanges(cfg sshx.Config, workspace, remoteRoot, statePath string) error {
	if err := sshx.RequireFields(cfg); err != nil {
		return err
	}
	return withStateLock(statePath, func() error {
		previous, err := LoadState(statePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		remote, err := RemoteSnapshot(cfg, remoteRoot)
		if err != nil {
			return err
		}
		remote = ProjectMirrorState(remote)
		changed, deleted := DiffRemote(previous, remote)
		if len(deleted) > 0 {
			if err := deleteLocal(workspace, deleted); err != nil {
				return err
			}
		}
		if len(changed) > 0 {
			if err := downloadTar(cfg, workspace, remoteRoot, changed); err != nil {
				return err
			}
		}
		current, err := Snapshot(workspace)
		if err != nil {
			return err
		}
		return saveStateUnlocked(statePath, current)
	})
}

func UploadChanges(cfg sshx.Config, workspace, remoteRoot, statePath string) error {
	if err := sshx.RequireFields(cfg); err != nil {
		return err
	}
	return withStateLock(statePath, func() error {
		return uploadChangesUnlocked(cfg, workspace, remoteRoot, statePath)
	})
}

func uploadChangesUnlocked(cfg sshx.Config, workspace, remoteRoot, statePath string) error {
	previous, err := LoadState(statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	current, err := Snapshot(workspace)
	if err != nil {
		return err
	}

	changed, deleted := Diff(previous, current)
	if len(deleted) > 0 {
		if err := deleteRemote(cfg, remoteRoot, deleted); err != nil {
			return err
		}
	}
	if len(changed) > 0 {
		if err := uploadTar(cfg, workspace, remoteRoot, changed); err != nil {
			return err
		}
	}
	return saveStateUnlocked(statePath, current)
}

func ProjectMirrorState(state State) State {
	projected := State{Entries: map[string]Entry{}}
	for rel, entry := range state.Entries {
		if entry.Type == "file" && entry.Size > maxMirrorFileSize {
			continue
		}
		projected.Entries[rel] = entry
	}
	return projected
}

func sortedStatePaths(state State) []string {
	rels := make([]string, 0, len(state.Entries))
	for rel := range state.Entries {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	return rels
}

func Diff(previous, current State) (changed []string, deleted []string) {
	if previous.Entries == nil {
		previous.Entries = map[string]Entry{}
	}
	for rel, cur := range current.Entries {
		prev, ok := previous.Entries[rel]
		if !ok || prev != cur {
			changed = append(changed, rel)
			if ok && prev.Type != cur.Type {
				deleted = append(deleted, rel)
			}
		}
	}
	for rel := range previous.Entries {
		if _, ok := current.Entries[rel]; !ok {
			deleted = append(deleted, rel)
		}
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	return changed, deleted
}

func DiffRemote(previous, remote State) (changed []string, deleted []string) {
	if previous.Entries == nil {
		previous.Entries = map[string]Entry{}
	}
	if remote.Entries == nil {
		remote.Entries = map[string]Entry{}
	}
	for rel, cur := range remote.Entries {
		prev, ok := previous.Entries[rel]
		if !ok || !sameRemoteEntry(prev, cur) {
			changed = append(changed, rel)
			if ok && prev.Type != cur.Type {
				deleted = append(deleted, rel)
			}
		}
	}
	for rel := range previous.Entries {
		if _, ok := remote.Entries[rel]; !ok {
			deleted = append(deleted, rel)
		}
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	return changed, deleted
}

func sameRemoteEntry(previous, remote Entry) bool {
	if previous.Type != remote.Type || previous.Size != remote.Size || previous.Mode != remote.Mode || previous.Link != remote.Link {
		return false
	}
	if remote.ModTime == 0 || previous.ModTime == 0 {
		return true
	}
	return previous.ModTime/int64(time.Second) == remote.ModTime/int64(time.Second)
}

func Snapshot(root string) (State, error) {
	entries := map[string]Entry{}
	err := filepath.WalkDir(root, func(filePath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		if d.IsDir() && isExcluded(d.Name()) {
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !safeRelativePath(rel) {
			return fmt.Errorf("unsafe local path %q", rel)
		}

		info, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		entry := Entry{
			Mode:    int64(info.Mode().Perm()),
			ModTime: info.ModTime().UnixNano(),
		}

		switch {
		case info.IsDir():
			entry.Type = "dir"
		case info.Mode().Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(filePath)
			if err != nil {
				return err
			}
			entry.Type = "symlink"
			entry.Link = link
		case info.Mode().IsRegular():
			hash, err := fileHash(filePath)
			if err != nil {
				return err
			}
			entry.Type = "file"
			entry.Size = info.Size()
			entry.Hash = hash
		default:
			return nil
		}

		entries[rel] = entry
		return nil
	})
	if err != nil {
		return State{}, fmt.Errorf("scan workspace: %w", err)
	}
	return State{Entries: entries}, nil
}

func RemoteSnapshot(cfg sshx.Config, remoteRoot string) (State, error) {
	if err := sshx.RequireFields(cfg); err != nil {
		return State{}, err
	}
	cmd := cfg.Command(remoteManifestCommand(remoteRoot))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return State{}, fmt.Errorf("scan remote workspace: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseRemoteManifest(output)
}

func remoteManifestCommand(remoteRoot string) string {
	var prunes []string
	for _, dir := range ExcludedDirs {
		prunes = append(prunes, "-name "+sshx.ShellQuote(dir))
	}
	pruneExpr := strings.Join(prunes, " -o ")
	return "cd " + sshx.ShellQuote(remoteRoot) + " && find . \\( " + pruneExpr + " \\) -prune -o \\( -type f -o -type d -o -type l \\) -printf '%P\\0%y\\0%s\\0%m\\0%T@\\0%l\\0'"
}

func parseRemoteManifest(data []byte) (State, error) {
	parts := bytes.Split(data, []byte{0})
	entries := map[string]Entry{}
	for i := 0; i+5 < len(parts); i += 6 {
		rel := string(parts[i])
		if rel == "" {
			continue
		}
		if !safeRelativePath(rel) {
			return State{}, fmt.Errorf("unsafe remote path %q", rel)
		}
		typ := string(parts[i+1])
		size, _ := strconv.ParseInt(string(parts[i+2]), 10, 64)
		mode, _ := strconv.ParseInt(string(parts[i+3]), 10, 64)
		modTime, err := parseRemoteModTime(string(parts[i+4]))
		if err != nil {
			return State{}, err
		}
		entry := Entry{Size: size, Mode: mode, ModTime: modTime, Link: string(parts[i+5])}
		switch typ {
		case "d":
			entry.Type = "dir"
			entry.Size = 0
		case "f":
			entry.Type = "file"
		case "l":
			entry.Type = "symlink"
			entry.Size = 0
		default:
			continue
		}
		entries[rel] = entry
	}
	return State{Entries: entries}, nil
}

func parseRemoteModTime(raw string) (int64, error) {
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse remote mtime %q: %w", raw, err)
	}
	return int64(seconds * float64(time.Second)), nil
}

func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("read sync state: %w", err)
	}
	if state.Entries == nil {
		state.Entries = map[string]Entry{}
	}
	return state, nil
}

func SaveState(path string, state State) error {
	return withStateLock(path, func() error {
		return saveStateUnlocked(path, state)
	})
}

func saveStateUnlocked(path string, state State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func withStateLock(statePath string, fn func() error) error {
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	lockPath := statePath + ".lock"
	localLockAny, _ := inProcessStateLocks.LoadOrStore(lockPath, &sync.Mutex{})
	localLock := localLockAny.(*sync.Mutex)
	localLock.Lock()
	defer localLock.Unlock()

	deadline := time.Now().Add(stateLockTimeout)
	for {
		release, err := tryAcquireStateLock(lockPath)
		if err == nil {
			defer release()
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if lockIsStale(lockPath) {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sync state lock timeout: %s", lockPath)
		}
		time.Sleep(stateLockPoll)
	}
}

func tryAcquireStateLock(lockPath string) (func(), error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(file, "pid=%d time=%s\n", os.Getpid(), time.Now().Format(time.RFC3339Nano))
	if err := file.Close(); err != nil {
		_ = os.Remove(lockPath)
		return nil, err
	}
	return func() {
		_ = os.Remove(lockPath)
	}, nil
}

func lockIsStale(lockPath string) bool {
	info, err := os.Stat(lockPath)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > stateLockStale
}

func extractTar(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar stream: %w", err)
		}

		rel, ok := cleanTarName(header.Name)
		if !ok {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		if err := ensureInside(root, target); err != nil {
			return err
		}

		mode := fs.FileMode(header.Mode)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, tr); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			_ = os.Chtimes(target, header.ModTime, header.ModTime)
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("create symlink %s -> %s: %w", target, header.Linkname, err)
			}
		}
	}
}

func downloadTar(cfg sshx.Config, workspace, remoteRoot string, rels []string) error {
	if len(rels) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(rels))
	for _, rel := range rels {
		if !safeRelativePath(rel) {
			return fmt.Errorf("unsafe download path %q", rel)
		}
		quoted = append(quoted, sshx.ShellQuote(rel))
	}
	remoteCommand := "cd " + sshx.ShellQuote(remoteRoot) + " && tar -cf - -- " + strings.Join(quoted, " ")
	cmd := cfg.Command(remoteCommand)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open remote tar delta stream: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start remote delta download: %w", err)
	}

	extractErr := extractTar(stdout, workspace)
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return fmt.Errorf("download changed files: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func uploadTar(cfg sshx.Config, workspace, remoteRoot string, rels []string) error {
	remoteCommand := "cd " + sshx.ShellQuote(remoteRoot) + " && tar -xf -"
	cmd := cfg.Command(remoteCommand)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open remote tar upload: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start remote tar upload: %w", err)
	}

	tw := tar.NewWriter(stdin)
	writeErr := writeTarEntries(tw, workspace, rels)
	closeErr := tw.Close()
	pipeErr := stdin.Close()
	waitErr := cmd.Wait()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if pipeErr != nil {
		return pipeErr
	}
	if waitErr != nil {
		return fmt.Errorf("upload changed files: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func writeTarEntries(tw *tar.Writer, workspace string, rels []string) error {
	for _, rel := range rels {
		if !safeRelativePath(rel) {
			return fmt.Errorf("unsafe upload path %q", rel)
		}
		localPath := filepath.Join(workspace, filepath.FromSlash(rel))
		info, err := os.Lstat(localPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}

		var link string
		if info.Mode().Type()&os.ModeSymlink != 0 {
			link, err = os.Readlink(localPath)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = rel
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(localPath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, file); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

func deleteLocal(workspace string, rels []string) error {
	for _, rel := range rels {
		if !safeRelativePath(rel) {
			return fmt.Errorf("unsafe local delete path %q", rel)
		}
		localPath := filepath.Join(workspace, filepath.FromSlash(rel))
		if err := ensureInside(workspace, localPath); err != nil {
			return err
		}
		if err := os.RemoveAll(localPath); err != nil {
			return fmt.Errorf("delete local path %s: %w", rel, err)
		}
	}
	return nil
}

func deleteRemote(cfg sshx.Config, remoteRoot string, rels []string) error {
	quoted := make([]string, 0, len(rels))
	for _, rel := range rels {
		if !safeRelativePath(rel) {
			return fmt.Errorf("unsafe delete path %q", rel)
		}
		quoted = append(quoted, sshx.ShellQuote(rel))
	}
	if len(quoted) == 0 {
		return nil
	}

	remoteCommand := "cd " + sshx.ShellQuote(remoteRoot) + " && rm -rf -- " + strings.Join(quoted, " ")
	cmd := cfg.Command(remoteCommand)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("delete remote paths: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func fileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func cleanTarName(name string) (string, bool) {
	cleaned := path.Clean(strings.TrimPrefix(name, "./"))
	if cleaned == "." {
		return "", false
	}
	if !safeRelativePath(cleaned) {
		return "", false
	}
	return cleaned, true
}

func safeRelativePath(rel string) bool {
	if rel == "" || rel == "." {
		return false
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "../") || rel == ".." || strings.Contains(rel, "/../") {
		return false
	}
	return true
}

func ensureInside(root, target string) error {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	cleanTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(cleanRoot, cleanTarget)
	if err != nil {
		return err
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("tar entry escapes workspace: %s", target)
	}
	return nil
}

func clearDirectory(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read workspace: %w", err)
	}
	for _, entry := range entries {
		if isExcluded(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("clear workspace: %w", err)
		}
	}
	return nil
}

func TouchState(path string) error {
	return SaveState(path, State{Entries: map[string]Entry{}})
}

func RemoteTarAvailable(cfg sshx.Config, remoteRoot string) error {
	remoteCommand := "cd " + sshx.ShellQuote(remoteRoot) + " && command -v tar >/dev/null"
	cmd := cfg.Command(remoteCommand)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("remote host needs tar in PATH: %w", err)
	}
	return nil
}

func SetModTime(path string, modTime time.Time) {
	_ = os.Chtimes(path, modTime, modTime)
}
