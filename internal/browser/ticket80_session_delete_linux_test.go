package browser

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func blockNativeSessionDeletion(t *testing.T, path string) func() {
	t.Helper()
	dir := filepath.Dir(path)
	release := sync.OnceFunc(func() {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(release)
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); !isNativeSessionDeleteRefusal(err) {
		t.Fatal("owned directory did not selectively deny native deletion")
	}
	return release
}

func isNativeSessionDeleteRefusal(err error) bool { return errors.Is(err, syscall.EACCES) }

// Namespace-root tests must not bypass directory permissions. Drop only the
// disposable child's DAC override capabilities before exec starts any Go
// threads; never alter privileges of the parent suite or another process.
func runSessionPermissionChild(t *testing.T) bool {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("REB_SESSION_PERMISSION_CHILD") == "1" {
		return false
	}
	const helper = "/usr/bin/setpriv"
	raw, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native child setpriv SHA256 %x", sha256.Sum256(raw))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	if parent, ok := t.Deadline(); ok && parent.Add(-2*time.Second).Before(deadline) {
		deadline = parent.Add(-2 * time.Second)
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	const name = "TestTicket80SessionNativeDeleteRefusalCannotMasqueradeAsSuccess"
	child := exec.CommandContext(ctx, helper, "--bounding-set=-dac_override,-dac_read_search", "--no-new-privs",
		executable, "-test.run=^"+name+"$", "-test.timeout=25s", "-test.v")
	child.Env = append(os.Environ(), "REB_SESSION_PERMISSION_CHILD=1")
	child.WaitDelay = time.Second
	output, err := child.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("confined native session child failed and was joined: %v", err)
	}
	for _, action := range []string{"login", "logout", "removed-account"} {
		if !strings.Contains(string(output), "--- PASS: "+name+"/"+action+" ") {
			t.Fatal("child did not execute every required HTTP fault case")
		}
	}
	return true
}
