package browser

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
)

func blockNativeSessionDeletion(t *testing.T, path string) func() {
	t.Helper()
	file, err := os.Open(path) // Native Go sharing permits read/write, not delete.
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(release)
	if err := os.Remove(path); !isNativeSessionDeleteRefusal(err) {
		t.Fatal("owned handle did not selectively deny native deletion")
	}
	return release
}

func isNativeSessionDeleteRefusal(err error) bool { return errors.Is(err, syscall.Errno(32)) }

func runSessionPermissionChild(*testing.T) bool { return false }
