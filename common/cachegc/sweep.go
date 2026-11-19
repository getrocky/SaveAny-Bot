package cachegc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ResolveDir turns the configured cache path into an absolute directory and
// refuses the working directory and its ancestors.
func ResolveDir(base string) (string, error) {
	if base == "" {
		return "", errors.New("empty cache path")
	}
	currentDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	p := base
	if !filepath.IsAbs(p) {
		p = filepath.Join(currentDir, p)
	}
	if p, err = filepath.Abs(p); err != nil {
		return "", err
	}
	p = filepath.Clean(p)
	if rel, err := filepath.Rel(p, currentDir); err != nil || rel == "." || !strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("refusing unsafe cache directory %s", p)
	}
	if p == string(filepath.Separator) || p == filepath.VolumeName(p)+string(filepath.Separator) {
		return "", fmt.Errorf("refusing unsafe cache directory %s", p)
	}
	return p, nil
}

// Result summarises one sweep.
type Result struct {
	Removed int   // cache entries removed (a .part and its bitmap count once)
	Bytes   int64 // bytes freed
	Kept    int   // entries kept because they are active or recently used
	Errors  int
}

// Options controls one sweep.
type Options struct {
	Dir     string
	IdleFor time.Duration
	Now     time.Time
	// IsActive reports extra in-process activity (for example queued tasks)
	// by entry name. Hold keys are always honoured.
	IsActive func(name string) bool
}

// resumeSuffixes are sidecar files that belong to the same download as the
// entry they extend. They are removed or kept together so a resumable
// download never ends up with a bitmap but no data, or the reverse.
var resumeSuffixes = []string{".bitmap", ".relay", ".part"}

func groupKey(name string) string {
	for {
		trimmed := name
		for _, s := range resumeSuffixes {
			if strings.HasSuffix(trimmed, s) && len(trimmed) > len(s) {
				trimmed = strings.TrimSuffix(trimmed, s)
				break
			}
		}
		if trimmed == name {
			return name
		}
		name = trimmed
	}
}

type entry struct {
	path    string
	name    string
	newest  time.Time
	size    int64
	removed bool
}

// newestAndSize walks p (file or directory) and returns the most recent
// modification time and the total size. Symlinks are not followed.
func newestAndSize(p string, info fs.FileInfo) (time.Time, int64) {
	newest := info.ModTime()
	var size int64
	if !info.IsDir() {
		return newest, info.Size()
	}
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		if fi.Mode().IsRegular() {
			size += fi.Size()
		}
		return nil
	})
	return newest, size
}

// Sweep removes top-level entries of opts.Dir that are not held, not reported
// active, and were not modified within opts.IdleFor. Only regular files and
// directories directly inside Dir are considered; symlinks are skipped.
func Sweep(opts Options) (Result, error) {
	var res Result
	if opts.Dir == "" || !filepath.IsAbs(opts.Dir) {
		return res, errors.New("cache directory must be absolute")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	dirEntries, err := os.ReadDir(opts.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, err
	}
	groups := map[string][]*entry{}
	var order []string
	for _, de := range dirEntries {
		name := de.Name()
		p := filepath.Join(opts.Dir, name)
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !(info.Mode().IsRegular() || info.IsDir()) {
			continue
		}
		newest, size := newestAndSize(p, info)
		key := groupKey(name)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], &entry{path: p, name: name, newest: newest, size: size})
	}
	for _, key := range order {
		members := groups[key]
		keep := false
		for _, m := range members {
			if IsHeld(m.name) || (opts.IsActive != nil && opts.IsActive(m.name)) || now.Sub(m.newest) < opts.IdleFor {
				keep = true
				break
			}
		}
		if keep {
			res.Kept++
			continue
		}
		var freed int64
		ok := true
		for _, m := range members {
			// A task may have started using the entry after the scan.
			if IsHeld(m.name) {
				ok = false
				break
			}
		}
		if !ok {
			res.Kept++
			continue
		}
		for _, m := range members {
			if err := os.RemoveAll(m.path); err != nil {
				res.Errors++
				continue
			}
			freed += m.size
		}
		res.Removed++
		res.Bytes += freed
	}
	return res, nil
}
