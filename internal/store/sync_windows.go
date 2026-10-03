//go:build windows

package store

// Go's portable file API does not expose a supported Windows directory flush.
// Individual files are flushed before same-directory rename; see package docs.
func syncDirectory(string) error { return nil }
