package access_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"release-evidence-board/internal/access"
)

func TestPolicyUsesTheRequiredRoleMatrix(t *testing.T) {
	for _, test := range []struct {
		name  string
		roles []access.Role
		want  [4]bool
	}{
		{"none", nil, [4]bool{false, false, false, false}},
		{"viewer", []access.Role{access.Viewer}, [4]bool{true, false, false, false}},
		{"contributor", []access.Role{access.Contributor}, [4]bool{true, true, false, false}},
		{"reviewer", []access.Role{access.Reviewer}, [4]bool{true, true, true, false}},
		{"operator", []access.Role{access.Operator}, [4]bool{false, false, false, true}},
		{"release-owner", []access.Role{access.ReleaseOwner}, [4]bool{true, false, false, false}},
		{"viewer-operator", []access.Role{access.Viewer, access.Operator}, [4]bool{true, false, false, true}},
		{"unrecognized-role", []access.Role{"unrecognized"}, [4]bool{false, false, false, false}},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := access.Account{ID: "actor", Roles: test.roles}
			for permission, want := range test.want {
				if got := account.Allows(access.Permission(permission)); got != want {
					t.Fatalf("permission%d=%t want%t", permission, got, want)
				}
			}
		})
	}
}

func TestPolicyDeniesUnknownPermissionEvenWithEveryRole(t *testing.T) {
	account := access.Account{ID: "reviewer", Roles: []access.Role{access.Viewer, access.Contributor, access.Reviewer, access.Operator, access.ReleaseOwner}}
	for _, permission := range []access.Permission{-1, 4, 999} {
		t.Run(strconv.Itoa(int(permission)), func(t *testing.T) {
			if account.Allows(permission) {
				t.Fatal("unknown permission granted")
			}
		})
	}
}

func TestPolicyAccountRetainsStoredJSONRepresentation(t *testing.T) {
	const original = `{"ID":"reviewer","Roles":["reviewer","operator"]}`
	var account access.Account
	if err := json.Unmarshal([]byte(original), &account); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(account)
	if err != nil || string(encoded) != original || !account.CanReview() || !account.CanOperate() {
		t.Fatal("account wire identity or policy changed")
	}
}
