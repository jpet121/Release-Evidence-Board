package backuppair_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"release-evidence-board/internal/backuppair"
)

const snapshotHash = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

func TestPublicationUsesRealSnapshotAndChecksum(t *testing.T) {
	dir := t.TempDir()
	stage, destination := filepath.Join(dir, "stage.db"), filepath.Join(dir, "backup.db")
	put(t, stage, "abc")
	if err := backuppair.Publish(stage, destination, snapshotHash); err != nil {
		t.Fatal(err)
	}
	absent(t, stage)
	contents(t, destination, "abc")
	contents(t, destination+".sha256", snapshotHash+"\n")
}

func TestNativeChecksumRefusalRollsBackOnlyNewSnapshot(t *testing.T) {
	dir := t.TempDir()
	stage, destination := filepath.Join(dir, "stage.db"), filepath.Join(dir, "backup.db")
	prior := filepath.Join(dir, "prior.db")
	put(t, stage, "abc")
	put(t, prior, "retained snapshot")
	put(t, prior+".sha256", "retained checksum\n")
	if err := os.Mkdir(destination+".sha256", 0700); err != nil {
		t.Fatal(err)
	}
	err := backuppair.Publish(stage, destination, snapshotHash)
	var native *os.PathError
	if !errors.As(err, &native) || native.Op != "open" || native.Path != destination+".sha256" {
		t.Fatalf("expected native checksum open refusal after rename, got %v", err)
	}
	absent(t, stage)
	absent(t, destination)
	contents(t, prior, "retained snapshot")
	contents(t, prior+".sha256", "retained checksum\n")
	if entry, err := os.Stat(destination + ".sha256"); err != nil || !entry.IsDir() {
		t.Fatalf("publication removed the conflicting directory: %v", err)
	}
	t.Logf("native checksum refusal preserved: %v", native.Err)
}

func TestNativeRenameRefusalRemovesOnlyStage(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage.db")
	destination := filepath.Join(dir, "missing", "backup.db")
	prior := filepath.Join(dir, "prior.db")
	put(t, stage, "abc")
	put(t, prior, "retained snapshot")
	put(t, prior+".sha256", "retained checksum\n")
	err := backuppair.Publish(stage, destination, snapshotHash)
	var native *os.LinkError
	if !errors.As(err, &native) || native.Op != "rename" || native.Old != stage || native.New != destination {
		t.Fatalf("expected native rename refusal, got %v", err)
	}
	absent(t, stage)
	absent(t, destination)
	absent(t, destination+".sha256")
	contents(t, prior, "retained snapshot")
	contents(t, prior+".sha256", "retained checksum\n")
}

func put(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected retained path %s: %v", path, err)
	}
}

func contents(t *testing.T, path, expected string) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != expected {
		t.Fatalf("contents of %s: %q, %v; want %q", path, actual, err, expected)
	}
}
