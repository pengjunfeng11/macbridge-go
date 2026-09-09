package host

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"macbridge/internal/core"
)

func posixMode(n int) os.FileMode {
	mode := os.FileMode(n & 0777)
	if n&04000 != 0 {
		mode |= os.ModeSetuid
	}
	if n&02000 != 0 {
		mode |= os.ModeSetgid
	}
	if n&01000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
}
func statMode(i os.FileInfo) int {
	if s, ok := i.Sys().(*syscall.Stat_t); ok {
		return int(s.Mode & 07777)
	}
	return int(i.Mode().Perm())
}

func hashFile(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func fileKind(i os.FileInfo) string {
	switch {
	case i.IsDir():
		return "directory"
	case i.Mode().IsRegular():
		return "file"
	case i.Mode()&os.ModeSymlink != 0:
		return "symlink"
	case i.Mode()&os.ModeSocket != 0:
		return "socket"
	default:
		return "other"
	}
}
func fileInfo(p string, i os.FileInfo) map[string]any {
	r := map[string]any{"path": p, "type": fileKind(i), "size": i.Size(), "mode": uint32(i.Mode().Perm()), "modifiedAt": i.ModTime().UTC().Format(time.RFC3339Nano), "symlinkTarget": nil}
	if i.Mode()&os.ModeSymlink != 0 {
		r["symlinkTarget"], _ = os.Readlink(p)
	}
	if s, ok := i.Sys().(*syscall.Stat_t); ok {
		r["uid"] = s.Uid
		r["gid"] = s.Gid
		r["inode"] = s.Ino
		r["device"] = s.Dev
		r["links"] = s.Nlink
		r["mode"] = s.Mode & 07777
		statTimes(r, s)
	}
	return r
}
func (m *Manager) fsCall(ctx context.Context, name string, a map[string]any) (any, error) {
	if name == "apply_patch" {
		patch, e := core.Required(a, "patch")
		if e != nil {
			return nil, e
		}
		flags := []string{"apply", "--recount", "--whitespace=nowarn"}
		for k, f := range map[string]string{"check_only": "--check", "reverse": "--reverse", "three_way": "--3way"} {
			if core.Bool(a, k, false) {
				flags = append(flags, f)
			}
		}
		flags = append(flags, "-")
		cmd := exec.Command("git", flags...)
		cmd.Dir = m.cfg.Path(core.String(a, "cwd", m.cfg.Home))
		cmd.Stdin = strings.NewReader(patch)
		r, e := runCommand(ctx, cmd, 120*time.Second, m.defaultOutput)
		if r != nil {
			r["command"] = "git " + strings.Join(flags, " ")
			r["cwd"] = cmd.Dir
			r["shell"] = m.cfg.Shell
		}
		return r, e
	}
	input, e := core.Required(a, "path")
	if e != nil {
		return nil, e
	}
	p := m.cfg.Path(input)
	switch name {
	case "fs_stat":
		i, e := os.Lstat(p)
		if e != nil {
			return nil, e
		}
		return fileInfo(p, i), nil
	case "fs_read":
		offset, e := integer(a, "offset", 0, 0, int(^uint(0)>>1))
		if e != nil {
			return nil, e
		}
		limit, e := integer(a, "max_bytes", m.defaultOutput, 1, m.maxOutput)
		if e != nil {
			return nil, e
		}
		enc := core.String(a, "encoding", "utf8")
		if enc != "utf8" && enc != "base64" {
			return nil, core.Error("INVALID_ARGUMENT", "encoding must be utf8 or base64")
		}
		f, e := os.Open(p)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		i, e := f.Stat()
		if e != nil {
			return nil, e
		}
		if !i.Mode().IsRegular() {
			return nil, fmt.Errorf("Not a regular file: %s", p)
		}
		data := make([]byte, min(limit, max(0, int(i.Size())-offset)))
		n, e := f.ReadAt(data, int64(offset))
		if e != nil && e != io.EOF {
			return nil, e
		}
		data = data[:n]
		content := string(data)
		if enc == "base64" {
			content = base64.StdEncoding.EncodeToString(data)
		}
		var next any
		if int64(offset+n) < i.Size() {
			next = offset + n
		}
		result := map[string]any{"path": p, "size": i.Size(), "offset": offset, "bytesRead": n, "nextOffset": next, "truncated": next != nil, "encoding": enc, "content": content}
		if core.Bool(a, "include_sha256", false) {
			h := sha256.New()
			if _, e = f.Seek(0, 0); e == nil {
				_, e = io.Copy(h, f)
			}
			if e != nil {
				return nil, e
			}
			after, e := f.Stat()
			if e != nil {
				return nil, e
			}
			if after.Size() != i.Size() || !after.ModTime().Equal(i.ModTime()) {
				return nil, core.Error("FS_CHANGED", "File changed during read; retry")
			}
			result["sha256"] = hex.EncodeToString(h.Sum(nil))
		}
		return result, nil

	case "fs_write":
		return m.writeFile(p, a)
	case "fs_list":
		limit, e := integer(a, "max_entries", 5000, 1, 100000)
		if e != nil {
			return nil, e
		}
		depth, e := integer(a, "max_depth", 10, 0, 100)
		if e != nil {
			return nil, e
		}
		entries := []any{}
		truncated := false
		var walk func(string, int) error
		walk = func(dir string, d int) error {
			items, e := os.ReadDir(dir)
			if e != nil {
				return e
			}
			for _, item := range items {
				if !core.Bool(a, "include_hidden", true) && strings.HasPrefix(item.Name(), ".") {
					continue
				}
				if len(entries) >= limit {
					truncated = true
					return nil
				}
				full := filepath.Join(dir, item.Name())
				i, e := os.Lstat(full)
				if e != nil {
					return e
				}
				r := fileInfo(full, i)
				r["name"] = item.Name()
				r["relativePath"], _ = filepath.Rel(p, full)
				entries = append(entries, r)
				if core.Bool(a, "recursive", false) && i.IsDir() && d < depth {
					if e = walk(full, d+1); e != nil {
						return e
					}
				}
			}
			return nil
		}
		if e = walk(p, 0); e != nil {
			return nil, e
		}
		return map[string]any{"root": p, "entries": entries, "count": len(entries), "truncated": truncated}, nil
	case "fs_manage":
		return m.manageFile(p, a)
	}
	return nil, core.Error("UNKNOWN_TOOL", name)
}
func (m *Manager) writeFile(p string, a map[string]any) (any, error) {
	// ponytail: writes serialize within this instance; use per-path locks if bulk write throughput matters.
	m.files.Lock()
	defer m.files.Unlock()
	content, ok := a["content"].(string)
	if !ok {
		return nil, core.Error("INVALID_ARGUMENT", "content must be a string")
	}
	data := []byte(content)
	var e error
	enc := core.String(a, "encoding", "utf8")
	if enc == "base64" {
		data, e = base64.StdEncoding.DecodeString(content)
		if e != nil {
			return nil, e
		}
	} else if enc != "utf8" {
		return nil, core.Error("INVALID_ARGUMENT", "encoding must be utf8 or base64")
	}
	mode, e := integer(a, "mode", 0644, 0, 07777)
	if e != nil {
		return nil, e
	}
	if _, ok := a["mode"]; !ok {
		if i, err := os.Stat(p); err == nil {
			mode = statMode(i)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if expected, ok := a["expected_sha256"]; ok {
		current, e := hashFile(p)
		if expected == nil {
			_, entryErr := os.Lstat(p)
			if entryErr == nil {
				return nil, core.Error("FS_CONFLICT", "File already exists")
			}
			if !os.IsNotExist(entryErr) {
				return nil, entryErr
			}
		} else {
			want, ok := expected.(string)
			_, hexErr := hex.DecodeString(want)
			if !ok || len(want) != 64 || hexErr != nil {
				return nil, core.Error("INVALID_ARGUMENT", "expected_sha256 must be a SHA256 hex digest or null")
			}
			if e != nil || !strings.EqualFold(want, current) {
				return nil, core.Error("FS_CONFLICT", "File changed since it was read")
			}
		}
	}
	if core.Bool(a, "create_parents", true) {
		if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return nil, e
		}
	}
	appendMode := core.Bool(a, "append", false)
	atomic := core.Bool(a, "atomic", true) && !appendMode
	if atomic {
		f, e := os.CreateTemp(filepath.Dir(p), ".mdb-write-*")
		if e != nil {
			return nil, e
		}
		tmp := f.Name()
		defer os.Remove(tmp)
		if e = f.Chmod(posixMode(mode)); e == nil {
			_, e = f.Write(data)
		}
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e == nil {
			e = os.Rename(tmp, p)
		}
		if e != nil {
			return nil, e
		}
	} else {
		flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if appendMode {
			flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, e := os.OpenFile(p, flags, posixMode(mode))
		if e != nil {
			return nil, e
		}
		_, e = f.Write(data)
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return nil, e
		}
	}
	i, e := os.Stat(p)
	if e != nil {
		return nil, e
	}
	digest, e := hashFile(p)
	return map[string]any{"path": p, "bytesWritten": len(data), "size": i.Size(), "append": appendMode, "atomic": atomic, "sha256": digest}, e
}
func (m *Manager) manageFile(p string, a map[string]any) (any, error) {
	op, e := core.Required(a, "operation")
	if e != nil {
		return nil, e
	}
	raw := core.String(a, "destination", "")
	dest := ""
	if raw != "" {
		dest = m.cfg.Path(raw)
		if op == "symlink" && !filepath.IsAbs(raw) && !strings.HasPrefix(raw, "~/") {
			dest = raw
		}
	}
	recursive := core.Bool(a, "recursive", false)
	force := core.Bool(a, "force", false)
	mode, e := integer(a, "mode", 0755, 0, 07777)
	if e != nil {
		return nil, e
	}
	switch op {
	case "mkdir":
		if recursive {
			e = os.MkdirAll(p, posixMode(mode))
		} else {
			e = os.Mkdir(p, posixMode(mode))
		}
	case "remove":
		if !force {
			if _, err := os.Lstat(p); err != nil {
				return nil, err
			}
		}
		if recursive {
			e = os.RemoveAll(p)
		} else {
			e = os.Remove(p)
		}
		if force && os.IsNotExist(e) {
			e = nil
		}
	case "chmod":
		if _, ok := a["mode"]; !ok {
			return nil, core.Error("INVALID_ARGUMENT", "mode is required")
		}
		e = os.Chmod(p, posixMode(mode))
	case "copy", "move":
		if dest == "" {
			return nil, core.Error("INVALID_ARGUMENT", "destination is required")
		}
		if dest == p {
			return nil, core.Error("INVALID_ARGUMENT", "Source and destination are identical")
		}
		if _, err := os.Lstat(p); err != nil {
			return nil, err
		}
		if rel, err := filepath.Rel(dest, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, core.Error("INVALID_ARGUMENT", "Destination cannot contain source")
		}
		if rel, er := filepath.Rel(p, dest); er == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, core.Error("INVALID_ARGUMENT", "Destination cannot be inside source")
		}
		if force {
			if e = os.RemoveAll(dest); e != nil {
				return nil, e
			}
		}
		if e = os.MkdirAll(filepath.Dir(dest), 0755); e != nil {
			return nil, e
		}
		if op == "move" {
			e = os.Rename(p, dest)
			if errors.Is(e, syscall.EXDEV) {
				e = copyPath(p, dest, true, force)
				if e == nil {
					e = os.RemoveAll(p)
				}
			}
		} else {
			e = copyPath(p, dest, recursive, force)
		}
	case "symlink":
		if dest == "" {
			return nil, core.Error("INVALID_ARGUMENT", "destination is required")
		}
		if force {
			if e = os.RemoveAll(p); e != nil {
				return nil, e
			}
		}
		if e = os.MkdirAll(filepath.Dir(p), 0755); e == nil {
			e = os.Symlink(dest, p)
		}
	default:
		return nil, core.Error("INVALID_ARGUMENT", "Unsupported operation: "+op)
	}
	if e != nil {
		return nil, e
	}
	var d any
	if dest != "" {
		d = dest
	}
	var modeResult any
	if _, ok := a["mode"]; ok {
		modeResult = mode
	}
	return map[string]any{"operation": op, "path": p, "destination": d, "recursive": recursive, "force": force, "mode": modeResult}, nil
}
func copyPath(src, dest string, recursive, force bool) error {
	i, e := os.Lstat(src)
	if e != nil {
		return e
	}
	if _, e = os.Lstat(dest); e == nil && !force {
		return os.ErrExist
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	if i.Mode()&os.ModeSymlink != 0 {
		target, e := os.Readlink(src)
		if e != nil {
			return e
		}
		return os.Symlink(target, dest)
	}
	if i.IsDir() {
		if !recursive {
			return fmt.Errorf("Recursive copy required for directory %s", src)
		}
		// Populate with owner write access, then restore the source mode. A
		// read-only source directory must still be possible to copy recursively.
		if e = os.MkdirAll(dest, i.Mode().Perm()|0700); e != nil {
			return e
		}
		defer os.Chmod(dest, i.Mode())
		items, e := os.ReadDir(src)
		if e != nil {
			return e
		}
		for _, entry := range items {
			if e = copyPath(filepath.Join(src, entry.Name()), filepath.Join(dest, entry.Name()), true, force); e != nil {
				return e
			}
		}
		if e = os.Chtimes(dest, accessTime(i.Sys().(*syscall.Stat_t)), i.ModTime()); e != nil {
			return e
		}
		return os.Chmod(dest, i.Mode())
	}
	if !i.Mode().IsRegular() {
		return fmt.Errorf("Cannot copy special file %s", src)
	}
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if !force {
		flags |= os.O_EXCL
	}
	out, e := os.OpenFile(dest, flags, i.Mode().Perm())
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	ce := out.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Chtimes(dest, accessTime(i.Sys().(*syscall.Stat_t)), i.ModTime()); e != nil {
		return e
	}
	return os.Chmod(dest, i.Mode())
}
