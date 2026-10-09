package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var diskConfigMu sync.Mutex
var syncConfigDirectoryFunc = syncConfigDirectory

// SaveSelectedDiskMount changes only the selected_disk_mount key in the
// Agent's existing config.yaml. It never serializes Config over user YAML.
func SaveSelectedDiskMount(path, mountpoint string) error {
	if !validSelectedDiskMount(mountpoint) {
		return errors.New("invalid selected disk mountpoint")
	}
	diskConfigMu.Lock()
	defer diskConfigMu.Unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	original, info, err := readConfigForUpdate(path)
	if err != nil {
		return err
	}
	document, mapping, err := configDocument(original)
	if err != nil {
		return err
	}
	if err := setSelectedDiskNode(mapping, mountpoint); err != nil {
		return err
	}
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".config.yaml.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	mode := os.FileMode(0o600)
	if info != nil {
		mode = info.Mode().Perm()
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if info != nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && (int(stat.Uid) != os.Geteuid() || int(stat.Gid) != os.Getegid()) {
			if err := tmp.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
				tmp.Close()
				return err
			}
		}
	}
	if _, err := tmp.Write(encoded.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := verifyConfigUnchanged(path, original, info != nil); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := syncConfigDirectoryFunc(dir); err != nil {
		// Rename already committed the new config path. Reporting failure here
		// would leave the caller's active selection out of sync with the file.
		log.Print("[config] selected disk config committed; directory sync failed, crash durability is uncertain")
	}
	return nil
}

func validSelectedDiskMount(path string) bool {
	if path == "" || len(path) > 1024 || !utf8.ValidString(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func readConfigForUpdate(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("Agent config is not a regular file")
	}
	data, err := os.ReadFile(path)
	return data, info, err
}

func configDocument(data []byte) (*yaml.Node, *yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mapping}}, mapping, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, nil, errors.New("Agent config must contain exactly one YAML document")
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("Agent config must be a YAML mapping")
	}
	return &document, document.Content[0], nil
}

func setSelectedDiskNode(mapping *yaml.Node, mountpoint string) error {
	var value *yaml.Node
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "selected_disk_mount" {
			continue
		}
		if value != nil {
			return errors.New("duplicate selected_disk_mount key")
		}
		value = mapping.Content[i+1]
	}
	if value == nil {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "selected_disk_mount"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: mountpoint},
		)
		return nil
	}
	value.Kind = yaml.ScalarNode
	value.Tag = "!!str"
	value.Value = mountpoint
	value.Style = 0
	return nil
}

func verifyConfigUnchanged(path string, original []byte, existed bool) error {
	current, err := os.ReadFile(path)
	if !existed && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Agent config changed during update: %w", err)
	}
	if !existed || !bytes.Equal(current, original) {
		return errors.New("Agent config changed during update")
	}
	return nil
}

func syncConfigDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
