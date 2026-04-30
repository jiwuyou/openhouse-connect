//go:build !windows

package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func ensureOutboxDirNoFollow(path string) (*os.File, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return nil, errors.New("directory is required")
	}

	start := "."
	rest := clean
	if filepath.IsAbs(clean) {
		start = string(os.PathSeparator)
		rest = strings.TrimPrefix(clean, string(os.PathSeparator))
	}

	current, err := os.Open(start)
	if err != nil {
		return nil, fmt.Errorf("open start directory: %w", err)
	}
	if rest == "" {
		return current, nil
	}

	parts := strings.Split(rest, string(os.PathSeparator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			current.Close()
			return nil, errors.New("directory must not contain parent traversal")
		}
		next, err := ensureOutboxChildDirNoFollow(current, part)
		current.Close()
		if err != nil {
			return nil, fmt.Errorf("open directory component %q nofollow: %w", part, err)
		}
		current = next
	}
	return current, nil
}

func ensureOutboxChildDirNoFollow(parent *os.File, name string) (*os.File, error) {
	if parent == nil {
		return nil, errors.New("missing parent directory")
	}
	if err := validateOutboxChildName(name); err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("mkdirat: %w", err)
	}
	return openOutboxChildDirNoFollowAt(parent, name)
}

func outboxFileInfoAtNoFollow(dir *os.File, name string) (fs.FileInfo, error) {
	file, info, err := openOutboxFileAtNoFollow(dir, name)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close file: %w", err)
	}
	return info, nil
}

func openOutboxFileAtNoFollow(dir *os.File, name string) (*os.File, fs.FileInfo, error) {
	if dir == nil {
		return nil, nil, errors.New("missing outbox directory")
	}
	before, err := fstatatOutboxFileNoFollow(dir, name)
	if err != nil {
		return nil, nil, err
	}
	if err := validateOutboxRegularUnixStat(before); err != nil {
		return nil, nil, err
	}

	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("openat nofollow: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, errors.New("openat nofollow: could not wrap file descriptor")
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
	if !outboxSameUnixStatFileInfo(before, after) {
		file.Close()
		return nil, nil, errors.New("file changed before open")
	}
	return file, after, nil
}

func fstatatOutboxFileNoFollow(dir *os.File, name string) (*unix.Stat_t, error) {
	if err := validateOutboxChildName(name); err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("fstatat nofollow: %w", err)
	}
	return &stat, nil
}

func openOutboxFileNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open nofollow: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open nofollow: could not wrap file descriptor")
	}
	return file, nil
}

func validateOutboxRegularUnixStat(stat *unix.Stat_t) error {
	mode := stat.Mode & unix.S_IFMT
	if mode == unix.S_IFLNK {
		return errors.New("refusing to read symlink")
	}
	if mode != unix.S_IFREG {
		return fmt.Errorf("refusing to read non-regular file: mode %s", outboxUnixFileMode(stat.Mode))
	}
	if stat.Nlink > 1 {
		return fmt.Errorf("refusing to read hardlinked file: links=%d", stat.Nlink)
	}
	return nil
}

type outboxFileID struct {
	dev uint64
	ino uint64
}

func outboxSameUnixStatFileInfo(stat *unix.Stat_t, info fs.FileInfo) bool {
	infoID, ok := outboxFileInfoID(info)
	if !ok {
		return false
	}
	return outboxFileID{dev: uint64(stat.Dev), ino: uint64(stat.Ino)} == infoID
}

func outboxSameFileInfo(a, b fs.FileInfo) bool {
	aID, ok := outboxFileInfoID(a)
	if !ok {
		return false
	}
	bID, ok := outboxFileInfoID(b)
	if !ok {
		return false
	}
	return aID == bID
}

func outboxFileInfoID(info fs.FileInfo) (outboxFileID, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return outboxFileID{}, false
	}
	return outboxFileID{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}, true
}

func outboxHardlinkCount(info fs.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Nlink), true
}

func outboxUnixFileMode(mode uint32) fs.FileMode {
	fileMode := fs.FileMode(mode & 0o777)
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		fileMode |= fs.ModeDir
	case unix.S_IFLNK:
		fileMode |= fs.ModeSymlink
	case unix.S_IFBLK:
		fileMode |= fs.ModeDevice
	case unix.S_IFCHR:
		fileMode |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFIFO:
		fileMode |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		fileMode |= fs.ModeSocket
	}
	if mode&unix.S_ISUID != 0 {
		fileMode |= fs.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		fileMode |= fs.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		fileMode |= fs.ModeSticky
	}
	return fileMode
}

func moveOutboxFileAt(srcDir *os.File, archiveDirName, name string) error {
	if srcDir == nil {
		return errors.New("missing source directory")
	}
	dstDir, err := openOutboxChildDirNoFollowAt(srcDir, archiveDirName)
	if err != nil {
		return err
	}
	defer dstDir.Close()

	dstName, err := uniqueOutboxArchiveNameAt(dstDir, name)
	if err != nil {
		return err
	}
	if err := unix.Renameat(int(srcDir.Fd()), name, int(dstDir.Fd()), dstName); err != nil {
		return fmt.Errorf("renameat: %w", err)
	}
	return nil
}

func openOutboxChildDirNoFollowAt(dir *os.File, name string) (*os.File, error) {
	if dir == nil {
		return nil, errors.New("missing parent directory")
	}
	if err := validateOutboxChildName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("openat directory nofollow: %w", err)
	}
	opened := os.NewFile(uintptr(fd), name)
	if opened == nil {
		_ = unix.Close(fd)
		return nil, errors.New("openat directory nofollow: could not wrap file descriptor")
	}
	return opened, nil
}

func openOutboxDirNoFollow(path string) (*os.File, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return nil, errors.New("directory is required")
	}

	start := "."
	rest := clean
	if filepath.IsAbs(clean) {
		start = string(os.PathSeparator)
		rest = strings.TrimPrefix(clean, string(os.PathSeparator))
	}

	current, err := os.Open(start)
	if err != nil {
		return nil, fmt.Errorf("open start directory: %w", err)
	}
	if rest == "" {
		return current, nil
	}

	parts := strings.Split(rest, string(os.PathSeparator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			current.Close()
			return nil, errors.New("directory must not contain parent traversal")
		}
		fd, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		current.Close()
		if err != nil {
			return nil, fmt.Errorf("open directory component %q nofollow: %w", part, err)
		}
		current = os.NewFile(uintptr(fd), part)
		if current == nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("open directory component %q: could not wrap descriptor", part)
		}
	}
	return current, nil
}

func uniqueOutboxArchiveNameAt(dir *os.File, name string) (string, error) {
	if dir == nil {
		return "", errors.New("missing archive directory")
	}
	if strings.TrimSpace(name) == "" {
		name = "file"
	}
	name = filepath.Base(name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem = "file"
	}
	for i := 0; i < 10000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i+1, ext)
		}
		var stat unix.Stat_t
		err := unix.Fstatat(int(dir.Fd()), candidate, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("fstatat archive candidate: %w", err)
		}
	}
	return "", fmt.Errorf("could not allocate archive name for %q", name)
}
