package recordset_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func mappedZeroValue(t *testing.T, size int) []byte {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "owned-zero-value"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := file.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	raw, err := syscall.Mmap(int(file.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Munmap(raw); err != nil {
			t.Error(err)
		}
	})
	return raw
}
