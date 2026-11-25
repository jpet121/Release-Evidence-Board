package board

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupRestoreDrillRejectsNativeUnaccountedBackupPage(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	backup, err := f.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := os.ReadFile(backup.Path + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	if failures := nativeBackupPageProblems(t, backup.Path); len(failures) != 0 {
		t.Fatalf("pristine native page inventory: %v", failures)
	}
	originalRecords := nativeBackupRecordBytes(t, backup.Path)
	if err := f.app.RecordRestoreDrill(secondActor, secondProject, backup.Path); err != nil {
		t.Fatalf("pristine public drill: %v", err)
	}
	altered, omitted := omitOneOwnedBackupFreePage(t, original)
	detached := filepath.Join(t.TempDir(), "detached.db")
	if err := os.WriteFile(detached, altered, 0600); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("page %d: unreachable unfreed", omitted)
	problems := nativeBackupPageProblems(t, detached)
	if !reflect.DeepEqual(problems, []string{want}) {
		t.Fatalf("detached backup did not isolate its exact native inventory error: got %v want %q", problems, want)
	}
	if !bytes.Equal(nativeBackupRecordBytes(t, detached), originalRecords) {
		t.Fatal("fixture changed semantic records instead of only free-page accounting")
	}
	eventsBefore, err := f.app.Audit(secondActor, secondProject)
	if err != nil {
		t.Fatal(err)
	}
	store := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	later := f.at.Add(time.Hour)
	app, err := Open(store, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return later }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.RecordRestoreDrill(secondActor, secondProject, detached); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("public drill accepted actual native page inconsistency: %v", err)
	}
	health, err := app.Health(secondActor, secondProject)
	if err != nil || !health.LastDrill.Equal(f.at) {
		t.Fatalf("refused drill advanced verified drill time: %s, %v", health.LastDrill, err)
	}
	eventsAfter, err := app.Audit(secondActor, secondProject)
	if err != nil || len(eventsAfter) != len(eventsBefore)+1 || !reflect.DeepEqual(eventsBefore, eventsAfter[:len(eventsBefore)]) {
		t.Fatalf("refused drill changed prior audit or added unexpected events: %v", err)
	}
	event := eventsAfter[len(eventsBefore)]
	if event.Action != "backup.drill" || event.Outcome != "failure" || event.Actor != secondActor || !event.Time.Equal(later) {
		t.Fatalf("missing attributed drill refusal: %#v", event)
	}
	if got, err := os.ReadFile(backup.Path); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original backup changed: %v", err)
	}
	if got, err := os.ReadFile(backup.Path + ".sha256"); err != nil || !bytes.Equal(got, sidecar) {
		t.Fatalf("original checksum changed: %v", err)
	}
	f.app = app
	f.checkReopened(t, false)
	t.Logf("native readonly Tx.Check isolated %q; refused drill preserved verified time, original pair, receipt, CSV and frozen HTML", want)
}

// This fixture edits only a detached self-created backup's already-free inventory.
// Pinned bbolt v1.5.0 amd64 layout: 16-byte page header and 64-byte meta.
func omitOneOwnedBackupFreePage(t *testing.T, raw []byte) ([]byte, uint64) {
	t.Helper()
	if len(raw) < 4096 {
		t.Fatal("backup too small for qualified meta layout")
	}
	pageSize := int(binary.LittleEndian.Uint32(raw[24:28]))
	if pageSize < 1024 || pageSize > 65536 || len(raw)%pageSize != 0 {
		t.Fatal("unqualified backup page size")
	}
	active := -1
	var last uint64
	for page := 0; page < 2; page++ {
		start := page * pageSize
		meta := raw[start+16 : start+80]
		h := fnv.New64a()
		_, _ = h.Write(meta[:56])
		if binary.LittleEndian.Uint64(raw[start:start+8]) != uint64(page) || binary.LittleEndian.Uint16(raw[start+8:start+10]) != 4 || binary.LittleEndian.Uint32(meta[:4]) != 0xED0CDAED || binary.LittleEndian.Uint32(meta[4:8]) != 2 || binary.LittleEndian.Uint32(meta[8:12]) != uint32(pageSize) || binary.LittleEndian.Uint64(meta[56:64]) != h.Sum64() {
			t.Fatal("unqualified native backup metadata")
		}
		txid := binary.LittleEndian.Uint64(meta[48:56])
		if active < 0 || txid > last {
			active = page
			last = txid
		}
	}
	meta := raw[active*pageSize+16 : active*pageSize+80]
	free := binary.LittleEndian.Uint64(meta[32:40])
	high := binary.LittleEndian.Uint64(meta[40:48])
	if free < 2 || free >= high || free >= uint64(len(raw)/pageSize) {
		t.Fatal("unqualified freelist page")
	}
	start := int(free) * pageSize
	count := binary.LittleEndian.Uint16(raw[start+10 : start+12])
	if binary.LittleEndian.Uint64(raw[start:start+8]) != free || binary.LittleEndian.Uint16(raw[start+8:start+10]) != 16 || count == 0 || count == 65535 || start+16+int(count)*8 > len(raw) {
		t.Fatalf("unqualified small native freelist count=%d", count)
	}
	lastOffset := start + 16 + (int(count)-1)*8
	omitted := binary.LittleEndian.Uint64(raw[lastOffset : lastOffset+8])
	if omitted < 2 || omitted >= high || omitted == free {
		t.Fatal("unqualified already-free page")
	}
	changed := bytes.Clone(raw)
	binary.LittleEndian.PutUint16(changed[start+10:start+12], count-1)
	return changed, omitted
}

func nativeBackupPageProblems(t *testing.T, path string) []string {
	t.Helper()
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	err = db.View(func(tx *bolt.Tx) error {
		for problem := range tx.Check() {
			problems = append(problems, problem.Error())
		}
		return nil
	})
	if err := errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	return problems
}

func nativeBackupRecordBytes(t *testing.T, path string) []byte {
	t.Helper()
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var visit func(*bolt.Bucket) error
	visit = func(bucket *bolt.Bucket) error {
		return bucket.ForEach(func(k, v []byte) error {
			fmt.Fprintf(&out, "K%d:", len(k))
			out.Write(k)
			if v == nil {
				out.WriteString("BUCKET{")
				if err := visit(bucket.Bucket(k)); err != nil {
					return err
				}
				out.WriteString("}")
			} else {
				fmt.Fprintf(&out, "V%d:", len(v))
				out.Write(v)
			}
			return nil
		})
	}
	err = db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(k []byte, bucket *bolt.Bucket) error {
			fmt.Fprintf(&out, "ROOT%d:", len(k))
			out.Write(k)
			return visit(bucket)
		})
	})
	if err := errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
