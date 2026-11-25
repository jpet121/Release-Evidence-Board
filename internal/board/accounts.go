package board

import (
	"errors"

	"release-evidence-board/internal/access"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/crypto/bcrypt"
)

type Role = access.Role

const (
	Viewer       = access.Viewer
	Contributor  = access.Contributor
	Reviewer     = access.Reviewer
	Operator     = access.Operator
	ReleaseOwner = access.ReleaseOwner
)

type permission = access.Permission

const (
	read       = access.Read
	contribute = access.Contribute
	review     = access.Review
	operate    = access.Operate
)

type Account = access.Account

type accountRecord struct {
	Account
	PasswordHash []byte
}

func makeAccount(id, password string, roles []Role) (accountRecord, error) {
	if !idPattern.MatchString(id) || len(roles) == 0 {
		return accountRecord{}, invalid("account requires a valid ID and assigned roles")
	}
	seen := make(map[Role]bool)
	for _, role := range roles {
		switch role {
		case Viewer, Contributor, Reviewer, Operator, ReleaseOwner:
		default:
			return accountRecord{}, invalid("unknown account role")
		}
		if seen[role] {
			return accountRecord{}, invalid("duplicate account role")
		}
		seen[role] = true
	}
	if len(password) < 12 || len(password) > 72 {
		return accountRecord{}, invalid("account password requires 12–72 UTF-8 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return accountRecord{}, err
	}
	return accountRecord{Account: Account{ID: id, Roles: roles}, PasswordHash: hash}, nil
}

func (b *Board) Bootstrap(id, password string) error {
	record, err := makeAccount(id, password, []Role{Reviewer, Operator})
	if err != nil {
		return err
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		return b.bootstrapAccount(tx, record)
	})
}

// bootstrapAccount publishes the first account and its audit in the caller's
// transaction. Native write refusal must leave both records uncommitted.
func (b *Board) bootstrapAccount(tx *bolt.Tx, record accountRecord) error {
	first, _ := tx.Bucket([]byte(accountBucket)).Cursor().First()
	if first != nil {
		return ErrConflict
	}
	if err := putJSON(tx, accountBucket, record.ID, record); err != nil {
		return err
	}
	return b.audit(tx, record.ID, "access.bootstrap", "success", record.ID)
}

func (b *Board) SetAccount(actor, id, password string, roles []Role) error {
	record, err := makeAccount(id, password, roles)
	if err != nil {
		return b.failedChange(actor, "access.set", err)
	}
	return b.change(actor, "access.set", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, b.project, operate); err != nil {
			return err
		}
		if actor == id && !record.CanOperate() {
			return invalid("operator cannot remove their own operator access")
		}
		if err := putJSON(tx, accountBucket, id, record); err != nil {
			return err
		}
		return b.audit(tx, actor, "access.set", "success", id)
	})
}

func (b *Board) RemoveAccount(actor, id string) error {
	return b.change(actor, "access.remove", func(tx *bolt.Tx) error {
		return b.removeAccount(tx, actor, id)
	})
}

func (b *Board) removeAccount(tx *bolt.Tx, actor, id string) error {
	if err := b.authorize(tx, actor, b.project, operate); err != nil {
		return err
	}
	if id == actor {
		return invalid("operator cannot remove their own account")
	}
	if _, err := getJSON[accountRecord](tx, accountBucket, id); err != nil {
		return err
	}
	if err := tx.Bucket([]byte(accountBucket)).Delete([]byte(id)); err != nil {
		return err
	}
	return b.audit(tx, actor, "access.remove", "success", id)
}

func (b *Board) Account(id string) (Account, error) {
	var record accountRecord
	err := b.db.View(func(tx *bolt.Tx) error {
		var err error
		record, err = getJSON[accountRecord](tx, accountBucket, id)
		return err
	})
	return record.Account, err
}

func (b *Board) Authenticate(id, password string) (Account, error) {
	var record accountRecord
	err := b.db.View(func(tx *bolt.Tx) error {
		var err error
		record, err = getJSON[accountRecord](tx, accountBucket, id)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		// A fixed test hash evens out rejection work for unknown accounts.
		record.PasswordHash = []byte("$2a$10$RRrfvg6hEXA2FFHy0VF/1ujCviNDL4zWw3MbY3NmhKbIv47hA6nV6")
	case err != nil:
		return Account{}, err
	}
	if bcrypt.CompareHashAndPassword(record.PasswordHash, []byte(password)) != nil || err != nil {
		return Account{}, ErrForbidden
	}
	return record.Account, nil
}

func (b *Board) authorize(tx *bolt.Tx, actor, project string, action permission) error {
	if project != b.project {
		return ErrNotFound
	}
	record, err := getJSON[accountRecord](tx, accountBucket, actor)
	if errors.Is(err, ErrNotFound) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if !record.Allows(action) {
		return ErrForbidden
	}
	return nil
}
