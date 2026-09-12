// Package localconfig updates only exact, authenticated Toolmux token references
// in detected local configuration files. It preserves file formatting/comments.
package localconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/shero4/toolmux/internal/store"
)

type Reference struct {
	Path, Digest string
	Count        int
	content      []byte
	token        string
	mode         os.FileMode
}

var tokenPattern = regexp.MustCompile(`tmx_[A-Za-z0-9_-]{32,100}`)

func Files(a store.Agent) []string {
	if a.ConfigPath == "" || strings.HasPrefix(a.Environment, "WSL") {
		return nil
	}
	dir := filepath.Dir(a.ConfigPath)
	files := []string{a.ConfigPath}
	switch a.Runtime {
	case "hermes":
		files = append(files, filepath.Join(dir, ".env"))
	case "openclaw":
		parts := strings.SplitN(a.Profile, ":", 2)
		if len(parts) == 2 && parts[1] != "" && parts[1] != "." && parts[1] != ".." && !strings.ContainsAny(parts[1], `/\`) {
			files = append(files, filepath.Join(dir, "agents", parts[1], "agent", "models.json"), filepath.Join(dir, "agents", parts[1], "agent", "auth-profiles.json"))
		}
	case "codex":
		files = append(files, filepath.Join(dir, ".env"))
	case "claude":
		files = append(files, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".mcp.json"))
	default:
		return nil
	}
	return files
}

// Match must authenticate the full candidate against the selected token ID.
func Inspect(ctx context.Context, files []string, match func(context.Context, string) (bool, error)) ([]Reference, error) {
	result := []Reference{}
	seen := map[string]bool{}
	for _, path := range files {
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, errors.New("a detected configuration file cannot be read")
		}
		if !info.Mode().IsRegular() || info.Size() > 4<<20 {
			return nil, errors.New("configuration must be a regular file under 4 MiB")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("a detected configuration file cannot be read")
		}
		found := ""
		count := 0
		for _, candidate := range tokenPattern.FindAll(data, -1) {
			ok, err := match(ctx, string(candidate))
			if err != nil {
				return nil, err
			}
			if ok {
				found = string(candidate)
				count++
			}
		}
		if count > 0 {
			digest := sha256.Sum256(data)
			result = append(result, Reference{Path: path, Digest: hex.EncodeToString(digest[:]), Count: count, content: data, token: found, mode: info.Mode()})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}
func Fingerprint(refs []Reference) string {
	h := sha256.New()
	for _, r := range refs {
		h.Write([]byte(r.Path + "\x00" + r.Digest + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".toolmux-rotation-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(mode.Perm()); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func Apply(refs []Reference, newToken string) error {
	// Verify all snapshots before writing the first file. Recheck each immediately
	// before replacement to catch an editor changing it while the dialog was open.
	verify := func(r Reference) error {
		info, e := os.Lstat(r.Path)
		if e != nil || !info.Mode().IsRegular() {
			return errors.New("configuration changed; review again")
		}
		data, e := os.ReadFile(r.Path)
		if e != nil || !bytes.Equal(data, r.content) {
			return errors.New("configuration changed; review again")
		}
		return nil
	}
	for _, r := range refs {
		if err := verify(r); err != nil {
			return err
		}
	}
	for _, r := range refs {
		if err := verify(r); err != nil {
			return err
		}
		data := tokenPattern.ReplaceAllFunc(r.content, func(value []byte) []byte {
			if string(value) == r.token {
				return []byte(newToken)
			}
			return value
		})
		if err := atomicWrite(r.Path, data, r.mode); err != nil {
			return errors.New("configuration update failed; old token remains active")
		}
	}
	return nil
}
