package board

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupOpenRefusesPartialSchemaWithoutReinitializing(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	path := f.app.db.Path()
	if err := f.app.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(metaBucket)) }); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }}
	app, err := Open(path, settings)
	if app != nil {
		if closeErr := app.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if app != nil || !errors.Is(err, bolt.ErrBucketExists) {
		t.Errorf("partial existing schema was treated as a fresh store: opened=%t, %v", app != nil, err)
	}
	saved, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = saved.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(metaBucket)) != nil {
			t.Error("refused open committed a new identity in the damaged store")
		}
		if raw := tx.Bucket([]byte(csvBucket)).Get([]byte(f.receipt.Hash)); !bytes.Equal(raw, f.source) {
			t.Error("refused open changed the retained original CSV")
		}
		if raw := tx.Bucket([]byte(htmlBucket)).Get([]byte(f.report.ID)); !bytes.Equal(raw, f.html) {
			t.Error("refused open changed the retained frozen artifact")
		}
		return nil
	})
	if err := errors.Join(err, saved.Close()); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"), settings)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Check().DatabaseOK {
		t.Error("genuinely empty store could not initialize")
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}
