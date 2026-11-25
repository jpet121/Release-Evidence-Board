package recordset_test

import (
	"encoding/json"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/recordset"
)

func TestWriteFrozenHTMLCommitsExactCompanions(t *testing.T) {
	db := openRecords(t)
	stores := frozenStores(t, db)
	var got string
	if err := db.Update(func(tx *bolt.Tx) error {
		var err error
		got, err = recordset.WriteFrozenHTML(tx, stores, "report", []byte("123"), func(sha string) any {
			if sha != hash123 || string(tx.Bucket([]byte(stores.HTML)).Get([]byte("report"))) != "123" {
				t.Error("metadata built before exact HTML was written")
			}
			return 456
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got != hash123 {
		t.Fatalf("HTML digest = %q", got)
	}
	checkFrozenValue(t, db, stores.HTML, "123")
	checkFrozenValue(t, db, stores.Metadata, "456")
	checkFrozenValue(t, db, stores.Binding, `{"MetadataSHA256":"`+hash456+`","HTMLSHA256":"`+hash123+`"}`)
	checkValue(t, db, "sentinel", "untouched")
}

func TestWriteFrozenHTMLStopsAndCallerRollsBackEachRefusal(t *testing.T) {
	for _, stage := range []string{"html", "metadata-encode", "metadata-put", "binding"} {
		t.Run(stage, func(t *testing.T) {
			db := openRecords(t)
			stores := frozenStores(t, db)
			faultBucket := map[string]string{"html": stores.HTML, "metadata-put": stores.Metadata, "binding": stores.Binding}[stage]
			if faultBucket != "" {
				if err := db.Update(func(tx *bolt.Tx) error {
					_, err := tx.Bucket([]byte(faultBucket)).CreateBucket([]byte("report"))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err := db.Update(func(tx *bolt.Tx) error {
				sha, err := recordset.WriteFrozenHTML(tx, stores, "report", []byte("123"), func(sha string) any {
					called = true
					if string(tx.Bucket([]byte(stores.HTML)).Get([]byte("report"))) != "123" {
						t.Error("HTML not written first")
					}
					if stage == "metadata-encode" {
						return make(chan int)
					}
					return 456
				})
				if sha != hash123 {
					t.Error("error lost exact HTML digest")
				}
				if stage == "binding" && string(tx.Bucket([]byte(stores.Metadata)).Get([]byte("report"))) != "456" {
					t.Error("binding attempted before metadata")
				}
				return err
			})
			if err == nil || (stage != "metadata-encode" && !errors.Is(err, bolt.ErrIncompatibleValue)) || called != (stage != "html") {
				t.Fatalf("wrong refusal/order: %v called=%v", err, called)
			}
			if stage == "metadata-encode" {
				var unsupported *json.UnsupportedTypeError
				if !errors.As(err, &unsupported) {
					t.Fatalf("encoding cause changed: %v", err)
				}
			}
			for _, bucket := range []string{stores.HTML, stores.Metadata, stores.Binding} {
				checkFrozenValue(t, db, bucket, "")
			}
			checkValue(t, db, "sentinel", "untouched")
		})
	}
}

func TestWriteFrozenHTMLPreservesRealNativeSizeAndReadOnlyRefusals(t *testing.T) {
	db := openRecords(t)
	stores := frozenStores(t, db)
	called := false
	if err := db.View(func(tx *bolt.Tx) error {
		_, err := recordset.WriteFrozenHTML(tx, stores, "report", []byte("123"), func(string) any { called = true; return 456 })
		return err
	}); !errors.Is(err, bolt.ErrTxNotWritable) || called {
		t.Fatalf("readonly refusal/order = %v called=%v", err, called)
	}
	const size = 2147483647 // bbolt v1.5.0 MaxValueSize + 1, not a product limit.
	// Independent Python hashlib streamed 2047 1MiB zero chunks and a final
	// 1048575-byte chunk; it neither uses this mapper nor the product digest.
	const zeroSHA = "25ba9187e4e7b89d2a7f1a49f0155c233ea8fe0b19c881bc53d23fd7b93deda0"
	raw := mappedZeroValue(t, size)
	err := db.Update(func(tx *bolt.Tx) error {
		sha, err := recordset.WriteFrozenHTML(tx, stores, "report", raw, func(string) any { called = true; return 456 })
		if sha != zeroSHA {
			t.Errorf("oversized actual mapped bytes hash = %s", sha)
		}
		return err
	})
	if !errors.Is(err, bolt.ErrValueTooLarge) || called {
		t.Fatalf("native oversize refusal/order = %v called=%v", err, called)
	}
	for _, bucket := range []string{stores.HTML, stores.Metadata, stores.Binding} {
		checkFrozenValue(t, db, bucket, "")
	}
	checkValue(t, db, "sentinel", "untouched")
	t.Logf("actual mapped zero bytes=%d native error=%v; no accepted large value committed", len(raw), err)
}

func frozenStores(t *testing.T, db *bolt.DB) recordset.FrozenHTMLStores {
	t.Helper()
	stores := recordset.FrozenHTMLStores{HTML: "html", Metadata: "metadata", Binding: "report-bindings"}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{stores.HTML, stores.Metadata, stores.Binding} {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return stores
}

func checkFrozenValue(t *testing.T, db *bolt.DB, bucket, want string) {
	t.Helper()
	if err := db.View(func(tx *bolt.Tx) error {
		if got := string(tx.Bucket([]byte(bucket)).Get([]byte("report"))); got != want {
			t.Errorf("%s=%q want %q", bucket, got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
