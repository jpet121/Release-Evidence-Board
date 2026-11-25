package upload_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"release-evidence-board/internal/upload"
)

func ownedFile(t *testing.T) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upload.csv")
	if err := os.WriteFile(path, []byte("abcdefgh"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
	})
	return file
}

func TestReadAndCloseBoundsThePrefixAndAlwaysCloses(t *testing.T) {
	for _, test := range []struct {
		limit int64
		want  string
	}{{-1, ""}, {0, ""}, {3, "abc"}, {20, "abcdefgh"}} {
		t.Run(strconv.FormatInt(test.limit, 10), func(t *testing.T) {
			file := ownedFile(t)
			raw, err := upload.ReadAndClose(file, test.limit)
			if err != nil || string(raw) != test.want {
				t.Fatalf("bounded result=%q error=%v", raw, err)
			}
			if _, err := file.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
				t.Fatal("successful read left file open")
			}
		})
	}
}

func TestReadAndClosePreservesReadAndCloseCausesIndependently(t *testing.T) {
	for _, limit := range []int64{0, 1} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			file := ownedFile(t)
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := upload.ReadAndClose(file, limit)
			joined, ok := err.(interface{ Unwrap() []error })
			want := []string{"close"}
			if limit > 0 {
				want = []string{"read", "close"}
			}
			if len(raw) != 0 || !ok || len(joined.Unwrap()) != len(want) {
				t.Fatal("a real read/close error was omitted")
			}
			for i, cause := range joined.Unwrap() {
				var native *os.PathError
				if !errors.As(cause, &native) || native.Op != want[i] || native.Path != file.Name() || !errors.Is(cause, os.ErrClosed) {
					t.Fatalf("real cause %d was not preserved as %s", i, want[i])
				}
			}
		})
	}
}

func TestReadAndCloseClosesAfterNativeDirectoryReadFailure(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
	})
	_, err = upload.ReadAndClose(file, 1)
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 1 {
		t.Fatal("native read failure was lost or a close error was manufactured")
	}
	var native *os.PathError
	if !errors.As(joined.Unwrap()[0], &native) || native.Op != "read" || native.Path != file.Name() {
		t.Fatal("wrong native read failure")
	}
	if err := file.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("read failure left file open")
	}
}
