package ingest

import (
	"fmt"
	"os"
)

// WriteProbe reports whether dir is writable, by attempting to create and
// immediately remove a scratch file in it. Used to fail fast (and with a
// clear error) when a cleanup job is asked to delete SSD originals but the
// SSD is mounted read-only — the default in compose.yaml/README until an
// operator deliberately switches it to rw for cleanup (see the M10 brief).
func WriteProbe(dir string) error {
	f, err := os.CreateTemp(dir, ".dashcam-write-probe-*")
	if err != nil {
		return fmt.Errorf("ingest: %s is not writable (mounted read-only?): %w", dir, err)
	}
	path := f.Name()
	f.Close()
	if err := os.Remove(path); err != nil {
		// Created but couldn't remove — still writable enough that this
		// isn't the read-only case we're guarding against; don't fail the
		// probe over a stray leftover file.
		return nil
	}
	return nil
}
