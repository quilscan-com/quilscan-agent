package diskinspect

import (
	"errors"
	"strconv"
	"strings"
)

type mountRecord struct {
	id         int
	deviceID   string
	root       string
	mountpoint string
	filesystem string
	source     string
}

var mountEscape = strings.NewReplacer(
	`\040`, " ",
	`\011`, "\t",
	`\012`, "\n",
	`\134`, `\`,
)

func parseMountInfo(data []byte) ([]mountRecord, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 16384 {
		return nil, errors.New("too many mounts")
	}
	mounts := make([]mountRecord, 0, len(lines))
	for _, line := range lines {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			return nil, errors.New("invalid mount information")
		}
		before, after := strings.Fields(left), strings.Fields(right)
		if len(before) < 6 || len(after) < 3 {
			return nil, errors.New("invalid mount fields")
		}
		id, err := strconv.Atoi(before[0])
		if err != nil || id < 1 {
			return nil, errors.New("invalid mount ID")
		}
		mounts = append(mounts, mountRecord{
			id:         id,
			deviceID:   before[2],
			root:       mountEscape.Replace(before[3]),
			mountpoint: mountEscape.Replace(before[4]),
			filesystem: strings.ToLower(after[0]),
			source:     mountEscape.Replace(after[1]),
		})
	}
	return mounts, nil
}

func selectLinuxMount(mounts []mountRecord, id int) (mountRecord, Code) {
	var target mountRecord
	for _, mount := range mounts {
		if mount.id == id {
			target = mount
			break
		}
	}
	if target.id == 0 || target.mountpoint == "" {
		return mountRecord{}, MountChanged
	}
	if target.mountpoint == "/" {
		return target, ""
	}
	if !linuxLocalFilesystem(target.filesystem) {
		return mountRecord{}, UnsupportedFilesystem
	}
	if target.root != "/" {
		if target.filesystem != "btrfs" && target.filesystem != "zfs" {
			return mountRecord{}, UnsupportedFilesystem
		}
		for _, mount := range mounts {
			if mount.id != target.id && mount.deviceID == target.deviceID && mount.source == target.source && mount.root == "/" {
				return mountRecord{}, UnsupportedFilesystem
			}
		}
	}
	canonical := target
	for _, mount := range mounts {
		if mount.id == target.id || mount.deviceID != target.deviceID || mount.source != target.source || mount.root != target.root || !linuxLocalFilesystem(mount.filesystem) {
			continue
		}
		if mount.id < canonical.id {
			canonical = mount
		}
	}
	return canonical, ""
}

func linuxLocalFilesystem(filesystem string) bool {
	switch filesystem {
	case "ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "f2fs", "vfat", "exfat", "ntfs", "bcachefs", "ubifs":
		return true
	default:
		return false
	}
}
