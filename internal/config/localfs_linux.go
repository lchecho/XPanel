//go:build linux

package config

import "golang.org/x/sys/unix"

func filesystemIsLocal(path string) (bool, string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false, "unknown", err
	}
	switch uint64(stat.Type) {
	case unix.NFS_SUPER_MAGIC:
		return false, "nfs", nil
	case unix.CIFS_MAGIC_NUMBER:
		return false, "cifs", nil
	case unix.FUSE_SUPER_MAGIC:
		return false, "fuse", nil
	default:
		return true, "local", nil
	}
}
