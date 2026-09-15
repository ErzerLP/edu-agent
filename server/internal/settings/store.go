package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const maxSettingsBytes = 64 << 10

// 文件只属于一个服务进程；公开接口不暴露路径、底层错误或文件内容。
func privateDirectory(path string) (*os.Root, string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." || filepath.Dir(path) == path {
		return nil, "", ErrStorage
	}
	directory := filepath.Dir(path)
	// 不允许通过任何符号链接选择秘密目录。
	for p := directory; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", ErrStorage
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return nil, "", ErrStorage
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, "", ErrStorage
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode().Perm() != 0700 {
		return nil, "", ErrStorage
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, "", ErrStorage
	}
	return root, filepath.Base(path), nil
}

func checkFile(root *os.Root, name string) (os.FileInfo, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, ErrStorage
	}
	return info, nil
}

func loadFile(path string) (stored, bool, error) {
	if path == "" {
		return stored{}, false, nil
	}
	root, name, err := privateDirectory(path)
	if err != nil {
		return stored{}, false, err
	}
	defer root.Close()
	info, err := checkFile(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return stored{}, false, nil
	}
	if err != nil {
		return stored{}, false, ErrStorage
	}
	f, err := root.Open(name)
	if err != nil {
		return stored{}, false, ErrStorage
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0600 {
		return stored{}, false, ErrStorage
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSettingsBytes+1))
	if err != nil || len(b) > maxSettingsBytes {
		return stored{}, false, ErrStorage
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var value stored
	if d.Decode(&value) != nil || d.Decode(new(any)) != io.EOF || value.Version != 1 || value.Revision < 0 {
		return stored{}, false, ErrStorage
	}
	return value, true, nil
}

func saveFile(path string, value stored) error {
	root, name, err := privateDirectory(path)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := checkFile(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrStorage
	}
	// 环境配置由原环境继续拥有；保存其他槽位不会迁移环境 Key。
	if value.Teaching.Source == "environment" {
		value.Teaching = secretSlot{Source: "environment"}
	}
	b, err := json.Marshal(value)
	if err != nil || len(b) > maxSettingsBytes {
		return ErrStorage
	}
	f, err := os.CreateTemp(root.Name(), ".settings-*")
	if err != nil {
		return ErrStorage
	}
	tmp := filepath.Base(f.Name())
	defer root.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return ErrStorage
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return ErrStorage
	}
	if err = f.Close(); err != nil {
		return ErrStorage
	}
	if err = root.Rename(tmp, name); err != nil {
		return ErrStorage
	}
	dir, err := root.Open(".")
	if err != nil {
		return ErrStorage
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return ErrStorage
	}
	return nil
}
