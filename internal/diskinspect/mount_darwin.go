//go:build darwin

package diskinspect

import (
	"context"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func inspectOpenDirectory(ctx context.Context, dir *os.File, cleanPath string) (Usage, Code) {
	var before unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &before); err != nil {
		return Usage{}, UnsupportedFilesystem
	}
	mountpoint := unix.ByteSliceToString(before.Mntonname[:])
	filesystemType := strings.ToLower(unix.ByteSliceToString(before.Fstypename[:]))
	if !darwinAllowedMount(cleanPath, mountpoint, filesystemType, before.Flags) {
		return Usage{}, UnsupportedFilesystem
	}
	mounts, ok := currentDarwinMounts()
	if !ok {
		return Usage{}, UnsupportedFilesystem
	}
	if !darwinMountPresent(mounts, before) || ctx.Err() != nil {
		return Usage{}, MountChanged
	}
	var after unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &after); err != nil || after.Fsid != before.Fsid || unix.ByteSliceToString(after.Mntonname[:]) != mountpoint {
		return Usage{}, MountChanged
	}
	mounts, ok = currentDarwinMounts()
	if !ok || !darwinMountPresent(mounts, after) {
		return Usage{}, MountChanged
	}
	if mountpoint == "/System/Volumes/Data" {
		root, err := os.Open("/")
		if err != nil {
			return Usage{}, MountChanged
		}
		defer root.Close()
		if err := unix.Fstatfs(int(root.Fd()), &after); err != nil {
			return Usage{}, MountChanged
		}
		mountpoint = "/"
		filesystemType = strings.ToLower(unix.ByteSliceToString(after.Fstypename[:]))
	}
	return usageFromBlocks(mountpoint, filesystemType, after.Blocks, after.Bfree, after.Bavail, uint64(after.Bsize))
}

func currentDarwinMounts() ([]unix.Statfs_t, bool) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || count < 1 || count > 4096 {
		return nil, false
	}
	mounts := make([]unix.Statfs_t, count+8)
	n, err := unix.Getfsstat(mounts, unix.MNT_NOWAIT)
	if err != nil || n < 1 || n > len(mounts) {
		return nil, false
	}
	return mounts[:n], true
}

func darwinMountPresent(mounts []unix.Statfs_t, want unix.Statfs_t) bool {
	mountpoint := unix.ByteSliceToString(want.Mntonname[:])
	for _, current := range mounts {
		if current.Fsid == want.Fsid && unix.ByteSliceToString(current.Mntonname[:]) == mountpoint {
			return true
		}
	}
	return false
}

func darwinAllowedMount(cleanPath, mountpoint, filesystemType string, flags uint32) bool {
	if flags&unix.MNT_LOCAL == 0 || mountpoint == "" || strings.HasPrefix(cleanPath, "/System/Volumes/") {
		return false
	}
	if mountpoint != "/" && mountpoint != "/System/Volumes/Data" && !strings.HasPrefix(mountpoint, "/Volumes/") {
		return false
	}
	switch filesystemType {
	case "apfs", "hfs", "msdos", "exfat", "ntfs":
		return true
	default:
		return false
	}
}
