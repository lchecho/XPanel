//go:build linux || darwin

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func validateLocalFilesystem(dir string) error {
	local, name, err := filesystemIsLocal(dir)
	if err != nil {
		return fmt.Errorf("inspect storage filesystem: %w", err)
	}
	if !local {
		return fmt.Errorf("storage database requires a local filesystem; found %s", name)
	}
	probe := filepath.Join(dir, ".xpanel-lock-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return errors.New("storage directory does not permit an advisory-lock probe")
	}
	defer os.Remove(probe)
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("storage filesystem does not support the required advisory lock")
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return nil
}
