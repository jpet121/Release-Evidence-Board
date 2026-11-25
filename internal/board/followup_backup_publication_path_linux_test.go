package board

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestFollowupBackupRefusesNativeOverlongPublicationPaths(t *testing.T) {
	const nativePathLimit = 4096
	// The temporary basename is at most 23 bytes. The final database name
	// is 50 bytes and the checksum name adds another seven.
	for _, trial := range []struct {
		name      string
		length    int
		atSidecar bool
	}{
		{"final_rename", 4065, false},
		{"checksum_creation", 4042, true},
	} {
		t.Run(trial.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			dir := t.TempDir()
			if len(dir)+2 > trial.length {
				t.Fatalf("temporary root is too long for the bounded fixture: %d bytes", len(dir))
			}
			for len(dir) < trial.length {
				remaining := trial.length - len(dir) - 1
				width := min(remaining, 200)
				if remaining == 201 {
					width--
				}
				dir = filepath.Join(dir, strings.Repeat("x", width))
			}
			if len(dir) != trial.length {
				t.Fatalf("unexpected temporary directory path length %d", len(dir))
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatalf("owned backup directory failed before publication: %v", err)
			}
			result, err := f.app.Backup(secondActor, secondProject, dir)
			if !errors.Is(err, syscall.ENAMETOOLONG) || result.Path != "" {
				t.Fatalf("backup did not report the targeted native long-path error: %v", err)
			}
			if trial.atSidecar {
				var pathErr *os.PathError
				if !errors.As(err, &pathErr) || pathErr.Op != "open" || filepath.Dir(pathErr.Path) != dir ||
					!strings.HasSuffix(pathErr.Path, ".db.sha256") || len(pathErr.Path) < nativePathLimit ||
					len(strings.TrimSuffix(pathErr.Path, ".sha256")) >= nativePathLimit {
					t.Fatalf("failed call did not reach the native checksum file open: %v", err)
				}
				t.Logf("native %s rejected checksum path of %d bytes", pathErr.Op, len(pathErr.Path))
			} else {
				var renameErr *os.LinkError
				if !errors.As(err, &renameErr) || renameErr.Op != "rename" || filepath.Dir(renameErr.Old) != dir ||
					filepath.Dir(renameErr.New) != dir || !strings.HasPrefix(filepath.Base(renameErr.Old), "reb-stage-") ||
					!strings.HasSuffix(renameErr.New, ".db") || len(renameErr.Old) >= nativePathLimit || len(renameErr.New) < nativePathLimit {
					t.Fatalf("failed call did not reach the native final snapshot rename: %v", err)
				}
				t.Logf("native %s rejected final database path of %d bytes from staged path of %d bytes",
					renameErr.Op, len(renameErr.New), len(renameErr.Old))
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("failed native publication retained a partial backup: entries=%d error=%v", len(entries), err)
			}
			f.checkReopened(t, false)
		})
	}
}
