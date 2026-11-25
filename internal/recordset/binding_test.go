package recordset_test

import (
	"bytes"
	"strings"
	"testing"

	"release-evidence-board/internal/recordset"
)

func TestMatchesBindingUsesIndependentGoldensAndDistinguishesAbsentBytes(t *testing.T) {
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, fixture := range []struct {
		name    string
		raw     []byte
		binding string
		want    bool
	}{
		{"exact 123", []byte("123"), hash123, true},
		{"exact 456", []byte("456"), hash456, true},
		{"different present bytes", []byte("456"), hash123, false},
		{"missing primary with empty value digest", nil, emptySHA, false},
		{"present zero length value", []byte{}, emptySHA, true},
		{"missing companion", []byte("123"), "", false},
		{"both missing", nil, "", false},
		{"uppercase digest", []byte("123"), strings.ToUpper(hash123), false},
		{"digest whitespace", []byte("123"), hash123 + " ", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			binding := []byte(fixture.binding)
			beforeRaw, beforeBinding := bytes.Clone(fixture.raw), bytes.Clone(binding)
			if got := recordset.MatchesBinding(fixture.raw, binding); got != fixture.want {
				t.Fatalf("binding presence/exact-byte result = %t, want %t", got, fixture.want)
			}
			if !bytes.Equal(fixture.raw, beforeRaw) || !bytes.Equal(binding, beforeBinding) {
				t.Fatal("verification changed caller-owned bytes")
			}
		})
	}
	if recordset.MatchesBinding([]byte("123"), nil) {
		t.Fatal("nil companion matched a present value")
	}
}
