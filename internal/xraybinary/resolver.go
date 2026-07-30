package xraybinary

import (
	"errors"
	"os"
	"path/filepath"
)

const (
	runtimeDirectory = "xray-runtime"
	currentName      = "current"
	appliedName      = "applied.json"
)

func ManagedPath(stateDirectory string) string {
	return filepath.Join(stateDirectory, runtimeDirectory, currentName, "xray")
}

// ActivePath keeps an existing managed Xray executable as the bootstrap
// fallback until the first signed runtime bundle is selected. Once an applied
// marker exists, a missing current link is corruption and must fail visibly.
func ActivePath(stateDirectory, bootstrapPath string) string {
	directory := filepath.Join(stateDirectory, runtimeDirectory)
	if existsOrUnknown(filepath.Join(directory, currentName)) || existsOrUnknown(filepath.Join(directory, appliedName)) {
		return ManagedPath(stateDirectory)
	}
	return bootstrapPath
}

func existsOrUnknown(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}
