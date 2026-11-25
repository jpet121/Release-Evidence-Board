package board

import "testing"

func TestC01RejectsForbiddenDynamicDescriptorValues(t *testing.T) {
	valid, err := describe([]any{"reb-fixture", []any{"valid", 1, true}})
	if err != nil || valid.Text != `["reb-fixture",["valid",1,true]]` || len(valid.Digest) != 64 {
		t.Fatalf("valid canonical control: descriptor=%#v error=%v", valid, err)
	}

	cases := []struct {
		id, kind, diagnostic string
		value                any
	}{
		{"AC-003-11.P001", "lone surrogate", "descriptor contains invalid UTF-8", string([]byte{0xed, 0xa0, 0x80})},
		{"AC-003-11.P002", "float", "descriptor value has an unsupported type", float64(1.25)},
		{"AC-003-11.P003", "object", "descriptor value has an unsupported type", map[string]any{"value": true}},
		{"AC-003-11.P004", "null", "descriptor value has an unsupported type", nil},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got, err := describe([]any{"reb-fixture", []any{"valid", tc.value, true}})
			if err == nil || err.Error() != tc.diagnostic || got != (Descriptor{}) {
				t.Fatalf("encoding %s produced descriptor=%#v error=%v", tc.kind, got, err)
			}
			t.Logf("actual C01 encoding rejected %s without descriptor bytes or digest", tc.kind)
			t.Logf(`REB_ACCEPTANCE_INSTANCE {"id":%q,"case_id":"AC-003-11","parameters":{"invalidType":%q},"actual":"Rejected"}`, tc.id, tc.kind)
		})
	}
}
