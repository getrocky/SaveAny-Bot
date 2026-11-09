package fsutil

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/gabriel-vasile/mimetype"
)

// 删除文件夹内的所有文件和子目录, 但不删除文件夹本身
func RemoveAllInDir(dirPath string) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		entryPath := filepath.Join(dirPath, entry.Name())
		if err := os.RemoveAll(entryPath); err != nil {
			return err
		}
	}
	return nil
}

func DetectFileExt(fp string) string {
	mt, err := mimetype.DetectFile(fp)
	if err != nil {
		return ""
	}
	return mt.Extension()
}

type File struct {
	*os.File
}

func (f *File) Remove() error {
	return os.Remove(f.Name())
}

func (f *File) CloseAndRemove() error {
	closeErr := f.Close()
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	return errors.Join(closeErr, f.Remove())
}

func CreateFile(fp string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(fp), os.ModePerm); err != nil {
		return nil, err
	}
	file, err := os.Create(fp)
	if err != nil {
		return nil, err
	}
	return &File{File: file}, nil
}

// SafeBaseName reduces a remote-provided name to one safe filename component.
func SafeBaseName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = NormalizePathname(name)
	switch name {
	case "", ".", "..":
		return "file"
	}
	const maxBytes = 180
	b := []byte(name)
	if len(b) <= maxBytes {
		return name
	}
	ext := path.Ext(name)
	stemBudget := maxBytes - len([]byte(ext))
	if stemBudget < 16 {
		stemBudget = maxBytes
		ext = ""
	}
	stem := []byte(strings.TrimSuffix(name, ext))
	if len(stem) > stemBudget {
		stem = stem[:stemBudget]
		for len(stem) > 0 && stem[len(stem)-1]&0xc0 == 0x80 {
			stem = stem[:len(stem)-1]
		}
	}
	name = string(stem) + ext
	if name == "" || name == ext {
		return "file" + ext
	}
	return name
}

func NormalizePathname(s string) string {
	specials := `\/:*?"<>|` + "\n\r\t"
	var builder strings.Builder
	for _, ch := range s {
		if strings.ContainsRune(specials, ch) || unicode.IsControl(ch) {
			builder.WriteRune('_')
		} else {
			builder.WriteRune(ch)
		}
	}

	result := strings.TrimRightFunc(builder.String(), func(r rune) bool {
		return r == '.' || r == '_' || unicode.IsSpace(r)
	})

	return result
}
