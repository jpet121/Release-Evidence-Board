package recordset

import bolt "go.etcd.io/bbolt"

// FrozenHTMLStores names the three companion stores of an immutable report.
type FrozenHTMLStores struct{ HTML, Metadata, Binding string }

// FrozenHTMLBinding binds the report metadata and its exact downloaded bytes.
type FrozenHTMLBinding struct{ MetadataSHA256, HTMLSHA256 string }

// WriteFrozenHTML publishes HTML, derived metadata, and their dual binding in
// that order. Return its error from Update to roll back earlier writes.
// The caller and metadata callback must keep html valid and unchanged through
// the end of the transaction: bbolt retains this borrowed slice after Put.
// The exact HTML digest is returned even when publication fails.
func WriteFrozenHTML(tx *bolt.Tx, stores FrozenHTMLStores, id string, html []byte, metadata func(string) any) (string, error) {
	sha := string(jsonBinding(html))
	if err := put(tx, Key{Bucket: stores.HTML, Key: id}, html); err != nil {
		return sha, err
	}
	raw, binding, err := EncodeJSON(metadata(sha))
	if err != nil {
		return sha, err
	}
	if err := put(tx, Key{Bucket: stores.Metadata, Key: id}, raw); err != nil {
		return sha, err
	}
	err = WriteJSON(tx, JSONRecord{Key: Key{Bucket: stores.Binding, Key: id}, Value: FrozenHTMLBinding{MetadataSHA256: string(binding), HTMLSHA256: sha}})
	return sha, err
}
