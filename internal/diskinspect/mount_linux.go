//go:build linux

package diskinspect

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func inspectOpenDirectory(ctx context.Context, dir *os.File, _ string) (Usage, Code) {
	var before unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &before); err != nil || before.Bsize <= 0 {
		return Usage{}, UnsupportedFilesystem
	}
	id, err := descriptorMountID(int(dir.Fd()))
	if err != nil {
		return Usage{}, UnsupportedFilesystem
	}
	mounts, err := currentLinuxMounts()
	if err != nil {
		return Usage{}, UnsupportedFilesystem
	}
	mount, code := selectLinuxMount(mounts, id)
	if code != "" {
		return Usage{}, code
	}
	if ctx.Err() != nil {
		return Usage{}, Timeout
	}
	var after unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &after); err != nil || after.Fsid != before.Fsid || after.Type != before.Type || after.Bsize <= 0 {
		return Usage{}, MountChanged
	}
	currentID, err := descriptorMountID(int(dir.Fd()))
	if err != nil || currentID != id {
		return Usage{}, MountChanged
	}
	current, err := currentLinuxMounts()
	if err != nil {
		return Usage{}, MountChanged
	}
	currentMount, code := selectLinuxMount(current, id)
	if code != "" || currentMount != mount {
		return Usage{}, MountChanged
	}
	return usageFromBlocks(mount.mountpoint, mount.filesystem, after.Blocks, after.Bfree, after.Bavail, uint64(after.Bsize))
}

func descriptorMountID(fd int) (int, error) {
	data, err := readBoundedKernelFile("/proc/self/fdinfo/"+strconv.Itoa(fd), 4096)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "mnt_id:" {
			return strconv.Atoi(fields[1])
		}
	}
	return 0, errors.New("mount ID unavailable")
}

func currentLinuxMounts() ([]mountRecord, error) {
	data, err := readBoundedKernelFile("/proc/self/mountinfo", 2<<20)
	if err != nil {
		return nil, err
	}
	return parseMountInfo(data)
}

func readBoundedKernelFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("kernel mount metadata too large")
	}
	return data, nil
}
