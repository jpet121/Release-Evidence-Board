package board_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
)

func TestFollowupNativeRootScalarRefusesStoreInitialization(t *testing.T) {
	if runtime.GOARCH != "amd64" || binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Fatal("this pinned bbolt format fixture requires a qualified little-endian amd64 target")
	}
	settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO"}
	for _, fixture := range []struct {
		name, target string
		migration    bool
	}{
		{"fresh_meta", "meta", false},
		{"migration_companion", "audit_metadata_sha256", true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "owned-root-kind.db")
			if fixture.migration {
				app, err := board.Open(path, settings)
				if err != nil {
					t.Fatal(err)
				}
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
			}
			writeRootKindFixture(t, path, func(tx *bolt.Tx) error {
				if fixture.migration {
					// This earlier new migration bucket should be created in
					// the attempted transaction and then rolled back.
					return tx.DeleteBucket([]byte("backup_expiry"))
				}
				if _, err := tx.CreateBucket([]byte("meta")); err != nil {
					return err
				}
				control, err := tx.CreateBucket([]byte("fixture-control"))
				if err != nil {
					return err
				}
				return control.Put([]byte("sentinel"), []byte("genuine untouched fixture sibling"))
			})

			original := readSmallRootKindFile(t, path)
			before := readNativeRootKind(t, path, fixture.target, 0, fixture.migration)
			if !before.targetBucket || before.targetValue != nil {
				t.Fatal("unmodified native cursor did not observe the target as a bucket")
			}
			root, offset, value := locatePinnedRootKind(t, original, before, fixture.target)
			qualified := readNativeRootKind(t, path, fixture.target, root, fixture.migration)
			if !qualified.targetBucket || qualified.txID != before.txID || !slices.Equal(qualified.keys, before.keys) {
				t.Fatal("parsed leaf page did not agree with the same native root transaction")
			}

			file, err := os.OpenFile(path, os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			n, writeErr := file.WriteAt([]byte{0}, int64(offset))
			if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil || n != 1 {
				t.Fatalf("could not write the exact single native kind byte: %d, %v", n, err)
			}
			changed := bytes.Clone(original)
			changed[offset] = 0
			if !bytes.Equal(readSmallRootKindFile(t, path), changed) {
				t.Fatal("offline fixture changed bytes outside the verified kind flag")
			}
			fault := readNativeRootKind(t, path, fixture.target, root, fixture.migration)
			if fault.targetBucket || !bytes.Equal(fault.targetValue, value) || fault.txID != before.txID || !slices.Equal(fault.keys, before.keys) {
				t.Fatal("native root cursor did not observe exactly the intended scalar change")
			}
			writeRootKindFixtureExpect(t, path, bolt.ErrIncompatibleValue, func(tx *bolt.Tx) error {
				_, err := tx.CreateBucketIfNotExists([]byte(fixture.target))
				return err
			})
			if !bytes.Equal(readSmallRootKindFile(t, path), changed) {
				t.Fatal("the SDK qualification unexpectedly wrote the store")
			}
			t.Logf("native root scalar verified: name=%s page=%d offset=%d tx=%d original=%x changed=%x", fixture.target, root, offset, fault.txID, sha256.Sum256(original), sha256.Sum256(changed))

			app, err := board.Open(path, settings)
			if app != nil {
				if closeErr := app.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if app != nil || !errors.Is(err, bolt.ErrIncompatibleValue) {
				t.Fatalf("Board.Open did not propagate the native incompatible-bucket error: %v", err)
			}
			after := readNativeRootKind(t, path, fixture.target, root, fixture.migration)
			if after.targetBucket || !bytes.Equal(after.targetValue, value) || after.txID != fault.txID || !slices.Equal(after.keys, fault.keys) || !bytes.Equal(readSmallRootKindFile(t, path), changed) {
				t.Fatal("Board.Open partially changed the real database despite refusing the root scalar")
			}
			t.Log("Board.Open propagated ErrIncompatibleValue and preserved every byte and the original transaction")
		})
	}
}

func writeRootKindFixture(t *testing.T, path string, action func(*bolt.Tx) error) {
	t.Helper()
	writeRootKindFixtureExpect(t, path, nil, action)
}

func writeRootKindFixtureExpect(t *testing.T, path string, want error, action func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	updated := db.Update(action)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(updated, want) {
		t.Fatalf("native bbolt write yielded %v, want %v", updated, want)
	}
}

func readSmallRootKindFile(t *testing.T, path string) []byte {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
		t.Fatalf("expected a small owned bbolt file: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || int64(len(raw)) != info.Size() {
		t.Fatalf("could not read exact small owned database bytes: %v", err)
	}
	return raw
}

type nativeRootKind struct {
	pageSize, txID int
	keys           []string
	targetBucket   bool
	targetValue    []byte
}

func readNativeRootKind(t *testing.T, path, target string, root int, migration bool) nativeRootKind {
	t.Helper()
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, PreLoadFreelist: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	probe := nativeRootKind{pageSize: db.Info().PageSize}
	viewed := db.View(func(tx *bolt.Tx) error {
		probe.txID = tx.ID()
		probe.targetBucket = tx.Bucket([]byte(target)) != nil
		cursor := tx.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			probe.keys = append(probe.keys, string(key))
			if string(key) == target {
				probe.targetValue = bytes.Clone(value)
			}
		}
		if root != 0 {
			page, err := tx.Page(root)
			if err != nil || page == nil || page.ID != root || page.Type != "leaf" || page.OverflowCount != 0 || page.Count != len(probe.keys) {
				return fmt.Errorf("native root-page read disagrees with the format probe: %+v, %v", page, err)
			}
		}
		if migration {
			meta := tx.Bucket([]byte("meta"))
			if meta == nil || string(meta.Get([]byte("project"))) != "pilot" || string(meta.Get([]byte("product"))) != "DEMO" ||
				string(meta.Get([]byte("schema"))) != "reb-store/1" || slices.Contains(probe.keys, "backup_expiry") {
				return errors.New("migration identity or earlier missing-bucket control changed")
			}
		} else {
			control := tx.Bucket([]byte("fixture-control"))
			if control == nil || string(control.Get([]byte("sentinel"))) != "genuine untouched fixture sibling" || len(probe.keys) != 2 {
				return errors.New("fresh root did not preserve the unrelated exact control bucket")
			}
		}
		return nil
	})
	if err := errors.Join(viewed, db.Close()); err != nil {
		t.Fatal(err)
	}
	return probe
}

// bbolt v1.5.0 format 2 on little-endian amd64. The native view validates the
// page size, transaction, leaf type and roster as independent preconditions.
// No SDK struct pointers are used for the offline format inspection.
func locatePinnedRootKind(t *testing.T, raw []byte, native nativeRootKind, target string) (int, int, []byte) {
	t.Helper()
	const pageHeader, leafElement, metaSize = 16, 16, 64
	u := binary.LittleEndian
	pageSize := native.pageSize
	if pageSize < 2*(pageHeader+metaSize) || pageSize > len(raw)/3 || len(raw)%pageSize != 0 || native.txID < 1 {
		t.Fatal("unqualified bbolt page-size or transaction format")
	}
	var selectedRoot, selectedTx uint64
	for slot := 0; slot < 2; slot++ {
		page := raw[slot*pageSize : (slot+1)*pageSize]
		meta := page[pageHeader : pageHeader+metaSize]
		checksum := fnv.New64a()
		_, _ = checksum.Write(meta[:56])
		if u.Uint64(page) != uint64(slot) || u.Uint16(page[8:]) != 0x04 || u.Uint16(page[10:]) != 0 || u.Uint32(page[12:]) != 0 ||
			u.Uint32(meta) != 0xED0CDAED || u.Uint32(meta[4:]) != 2 || u.Uint32(meta[8:]) != uint32(pageSize) || u.Uint32(meta[12:]) != 0 ||
			u.Uint64(meta[56:]) != checksum.Sum64() {
			t.Fatalf("unqualified bbolt metadata page %d", slot)
		}
		root, highWater, tx := u.Uint64(meta[16:]), u.Uint64(meta[40:]), u.Uint64(meta[48:])
		if tx%2 != uint64(slot) || root < 2 || root >= highWater || highWater > uint64(len(raw)/pageSize) {
			t.Fatalf("metadata page %d points beyond its owned file or has conflicting transaction parity", slot)
		}
		if tx > selectedTx {
			selectedRoot, selectedTx = root, tx
		}
	}
	if selectedTx != uint64(native.txID) {
		t.Fatal("checked bbolt metadata did not identify the actual native transaction")
	}
	start := int(selectedRoot) * pageSize
	page := raw[start : start+pageSize]
	count := int(u.Uint16(page[10:]))
	if u.Uint64(page) != selectedRoot || u.Uint16(page[8:]) != 0x02 || u.Uint32(page[12:]) != 0 ||
		count != len(native.keys) || count > (pageSize-pageHeader)/leafElement {
		t.Fatal("pinned fixture root must be a single native leaf with the exact expected key count")
	}
	flagOffset := -1
	var targetValue []byte
	previousEnd := pageHeader + count*leafElement
	for i, expected := range native.keys {
		at := pageHeader + i*leafElement
		entry := page[at : at+leafElement]
		pos, keyLen, valueLen := u.Uint32(entry[4:]), u.Uint32(entry[8:]), u.Uint32(entry[12:])
		if u.Uint32(entry) != 1 || pos > uint32(pageSize-at) || keyLen == 0 || uint64(pos)+uint64(keyLen)+uint64(valueLen) > uint64(pageSize-at) || valueLen < 16 ||
			at+int(pos) < previousEnd {
			t.Fatalf("unqualified native root element metadata for %q", expected)
		}
		from := at + int(pos)
		keyEnd := from + int(keyLen)
		previousEnd = keyEnd + int(valueLen)
		if string(page[from:keyEnd]) != expected {
			t.Fatal("qualifying bytes do not name the same keys as the actual native root cursor")
		}
		if expected == target {
			var emptyInline [32]byte
			emptyInline[24] = 0x02 // empty inline bucket leaf page flag
			if flagOffset != -1 || !bytes.Equal(page[keyEnd:previousEnd], emptyInline[:]) {
				t.Fatal("target must be the unique native empty inline bucket")
			}
			flagOffset, targetValue = start+at, bytes.Clone(page[keyEnd:previousEnd])
		}
	}
	if flagOffset == -1 || raw[flagOffset] != 1 {
		t.Fatal("could not completely qualify the target leaf's bucket-kind byte")
	}
	return int(selectedRoot), flagOffset, targetValue
}
