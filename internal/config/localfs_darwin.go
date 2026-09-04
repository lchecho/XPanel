//go:build darwin

package config

import (
	"strings"

	"golang.org/x/sys/unix"
)

func filesystemIsLocal(path string) (bool, string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false, "unknown", err
	}
	buf := make([]byte, 0, len(stat.Fstypename))
	for _, c := range stat.Fstypename {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	name := strings.ToLower(string(buf))
	switch name {
	case "nfs", "smbfs", "webdav", "osxfuse", "macfuse":
		return false, name, nil
	default:
		return true, name, nil
	}
}
