// Package backuppair publishes a validated, closed snapshot and its checksum.
package backuppair

import (
	"errors"
	"os"
)

// Publish moves stage to a fresh destination, then writes its checksum sidecar.
// The caller exclusively owns the distinct stage/destination namespace, supplies
// the snapshot's SHA-256, and must not select an existing backup or checksum.
// These filesystem calls do not prevent replacement of an existing destination.
// On failure, snapshot cleanup is best effort and its error is returned with
// the publication error. A checksum write/close failure may leave a partial
// sidecar; an existing conflicting sidecar is not removed.
func Publish(stage, destination, sha256 string) error {
	if err := os.Rename(stage, destination); err != nil {
		return errors.Join(err, os.Remove(stage))
	}
	if err := os.WriteFile(destination+".sha256", []byte(sha256+"\n"), 0600); err != nil {
		return errors.Join(err, os.Remove(destination))
	}
	return nil
}
