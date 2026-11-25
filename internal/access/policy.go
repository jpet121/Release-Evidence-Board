// Package access defines the Board's pure role and permission policy.
package access

import "slices"

type Role string

const (
	Viewer       Role = "viewer"
	Contributor  Role = "contributor"
	Reviewer     Role = "reviewer"
	Operator     Role = "operator"
	ReleaseOwner Role = "release-owner"
)

type Permission int

const (
	Read Permission = iota
	Contribute
	Review
	Operate
)

type Account struct {
	ID    string
	Roles []Role
}

func (a Account) CanView() bool       { return a.has(Viewer) || a.CanContribute() || a.has(ReleaseOwner) }
func (a Account) CanContribute() bool { return a.has(Contributor) || a.CanReview() }
func (a Account) CanReview() bool     { return a.has(Reviewer) }
func (a Account) CanOperate() bool    { return a.has(Operator) }
func (a Account) has(role Role) bool  { return slices.Contains(a.Roles, role) }

// Allows fails closed for every unknown permission, even for an all-role account.
// Project scope, account identity and storage integrity belong to the caller.
func (a Account) Allows(action Permission) bool {
	switch action {
	case Read:
		return a.CanView()
	case Contribute:
		return a.CanContribute()
	case Review:
		return a.CanReview()
	case Operate:
		return a.CanOperate()
	default:
		return false
	}
}
