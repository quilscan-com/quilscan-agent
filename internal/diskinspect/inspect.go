// Package diskinspect validates browser-supplied directory paths before any
// platform-specific filesystem inspection. It never interprets paths as shell
// input.
package diskinspect

import (
	"context"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Usage contains only the mount identity and capacity needed by the disk view.
type Usage struct {
	Mountpoint     string  `json:"mountpoint"`
	FilesystemType string  `json:"filesystem_type"`
	UsedBytes      uint64  `json:"used_bytes"`
	FreeBytes      uint64  `json:"free_bytes"`
	TotalBytes     uint64  `json:"total_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

// Code is a stable, non-sensitive error category returned to the browser.
type Code string

const (
	InvalidPath           Code = "invalid_path"
	NotDirectory          Code = "not_directory"
	UnsupportedFilesystem Code = "unsupported_filesystem"
	MountChanged          Code = "mount_changed"
	Busy                  Code = "busy"
	Timeout               Code = "timeout"
	PersistenceFailed     Code = "persistence_failed"
)

// ValidatePath accepts one absolute directory path as uninterpreted data.
// The caller must still open and inspect the resulting path before trusting it.
func ValidatePath(path string) (string, Code) {
	if path == "" || len(path) > 1024 || !utf8.ValidString(path) || !filepath.IsAbs(path) {
		return "", InvalidPath
	}
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return "", InvalidPath
		}
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return "", InvalidPath
		}
	}
	return filepath.Clean(path), ""
}

// Inspect checks an existing directory and reports the local mount it belongs
// to. The open descriptor, rather than a lexical path prefix, is authoritative.
func Inspect(ctx context.Context, path string) (Usage, Code) {
	clean, code := ValidatePath(path)
	if code != "" {
		return Usage{}, code
	}
	if ctx.Err() != nil {
		return Usage{}, Timeout
	}
	fd, err := unix.Open(clean, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Usage{}, NotDirectory
	}
	dir := os.NewFile(uintptr(fd), clean)
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		return Usage{}, NotDirectory
	}
	usage, code := inspectOpenDirectory(ctx, dir, clean)
	if ctx.Err() != nil {
		return Usage{}, Timeout
	}
	return usage, code
}

func usageFromBlocks(mountpoint, filesystemType string, blocks, freeBlocks, availableBlocks, blockSize uint64) (Usage, Code) {
	if blocks == 0 || blockSize == 0 || freeBlocks > blocks || availableBlocks > blocks {
		return Usage{}, UnsupportedFilesystem
	}
	totalOverflow, total := bits.Mul64(blocks, blockSize)
	usedOverflow, used := bits.Mul64(blocks-freeBlocks, blockSize)
	freeOverflow, free := bits.Mul64(availableBlocks, blockSize)
	if totalOverflow != 0 || usedOverflow != 0 || freeOverflow != 0 || total == 0 || used > total || free > total {
		return Usage{}, UnsupportedFilesystem
	}
	return Usage{
		Mountpoint:     mountpoint,
		FilesystemType: filesystemType,
		UsedBytes:      used,
		FreeBytes:      free,
		TotalBytes:     total,
		UsedPercent:    float64(used) * 100 / float64(total),
	}, ""
}
