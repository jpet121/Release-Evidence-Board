package recordset_test

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"unsafe"
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
	// Mark the owned NTFS file sparse before extending it. The view still
	// covers every byte of an actual file; no forged slice or committed value.
	var returned uint32
	if err := syscall.DeviceIoControl(syscall.Handle(file.Fd()), 0x900c4, nil, 0, nil, 0, &returned, nil); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	mapping, err := syscall.CreateFileMapping(syscall.Handle(file.Fd()), nil, syscall.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.CloseHandle(mapping); err != nil {
			t.Error(err)
		}
	})
	address, err := syscall.MapViewOfFile(mapping, syscall.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.UnmapViewOfFile(address); err != nil {
			t.Error(err)
		}
	})
	// MapViewOfFile above granted precisely this readable length. The view
	// stays mapped until after the caller finishes all hashing and writes.
	var raw []byte
	// This header belongs to an actual slice (unsafe documentation pattern 6).
	// Its backing storage is OS-owned, not a Go heap object. The registered
	// UnmapViewOfFile cleanup keeps it readable through the caller's use.
	header := (*reflect.SliceHeader)(unsafe.Pointer(&raw))
	header.Data, header.Len, header.Cap = address, size, size
	return raw
}
