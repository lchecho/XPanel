//go:build !linux && !darwin

package config

func validateLocalFilesystem(string) error { return nil }
