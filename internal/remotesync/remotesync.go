package remotesync

import (
	"archive/tar"
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
	"strings"
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

	remoteCommand := "cd " + sshx.ShellQuote(remoteRoot) + " && tar -cf - ."
	cmd := cfg.Command(remoteCommand)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open remote tar stream: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start remote tar download: %w", err)
	}

	extractErr := extractTar(stdout, workspace)
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return fmt.Errorf("download remote workspace: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}

	state, err := Snapshot(workspace)
	if err != nil {
		return err
	}
	return SaveState(statePath, state)
}

func UploadChanges(cfg sshx.Config, workspace, remoteRoot, statePath string) error {
	if err := sshx.RequireFields(cfg); err != nil {
		return err
	}
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
	return SaveState(statePath, current)
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

func Snapshot(root string) (State, error) {
	entries := map[string]Entry{}
	err := filepath.WalkDir(root, func(filePath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
