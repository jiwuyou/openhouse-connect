//go:build windows

package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func ensureOutboxDirNoFollow(path string) (*os.File, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return nil, errors.New("directory is required")
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return nil, err
	}
	return openOutboxDirNoFollow(clean)
}

func ensureOutboxChildDirNoFollow(parent *os.File, name string) (*os.File, error) {
	if parent == nil {
		return nil, errors.New("missing parent directory")
	}
	if err := validateOutboxChildName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(parent.Name(), name)
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return openOutboxDirNoFollow(path)
}

func outboxFileInfoAtNoFollow(dir *os.File, name string) (fs.FileInfo, error) {
	if dir == nil {
		return nil, errors.New("missing outbox directory")
	}
	if err := validateOutboxChildName(name); err != nil {
		return nil, err
	}
	info, err := os.Lstat(filepath.Join(dir.Name(), name))
	if err != nil {
		return nil, fmt.Errorf("lstat: %w", err)
	}
	return info, nil
}

func openOutboxFileAtNoFollow(dir *os.File, name string) (*os.File, fs.FileInfo, error) {
	if dir == nil {
		return nil, nil, errors.New("missing outbox directory")
	}
	if err := validateOutboxChildName(name); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir.Name(), name)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("lstat: %w", err)
	}
	if err := validateOutboxRegularFile(before); err != nil {
		return nil, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("fstat: %w", err)
	}
	if err := validateOutboxRegularFile(after); err != nil {
		file.Close()
		return nil, nil, err
	}
	if !outboxSameFileInfo(before, after) {
		file.Close()
		return nil, nil, errors.New("file changed before open")
	}
	return file, after, nil
}

func openOutboxFileNoFollow(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("lstat: %w", err)
	}
	if err := validateOutboxRegularFile(before); err != nil {
		return nil, err
	}
	return os.Open(path)
}

func outboxSameFileInfo(a, b fs.FileInfo) bool {
	return os.SameFile(a, b)
}

func outboxHardlinkCount(fs.FileInfo) (uint64, bool) {
	return 0, false
}

func moveOutboxFileAt(srcDir *os.File, archiveDirName, name string) error {
	if srcDir == nil {
		return errors.New("missing source directory")
	}
	if err := validateOutboxChildName(archiveDirName); err != nil {
		return err
	}
	if err := validateOutboxChildName(name); err != nil {
		return err
	}
	dstDir := filepath.Join(srcDir.Name(), archiveDirName)
	if err := os.Mkdir(dstDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	dst, err := uniqueOutboxArchivePath(dstDir, name)
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(srcDir.Name(), name), dst)
}

func openOutboxDirNoFollow(path string) (*os.File, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return nil, errors.New("directory is required")
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, fmt.Errorf("lstat directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("refusing to open symlinked directory")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("refusing to open non-directory: %s", info.Mode())
	}
	return os.Open(clean)
}
