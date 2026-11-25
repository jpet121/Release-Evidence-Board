package board

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"release-evidence-board/internal/recordset"

	bolt "go.etcd.io/bbolt"
)

type CandidateInput struct {
	Manifest                       Manifest
	BaselineDigest, DisplayVersion string
	PredecessorID, Correction      string
}

type Candidate struct {
	ID, ManifestDigest, BaselineDigest string
	DisplayVersion, PredecessorID      string
	Correction, MaterialDigest         string
	Changes                            []string
	CreatedAt                          time.Time
	CreatedBy                          string
}

type StoredManifest struct {
	Manifest   Manifest
	Descriptor Descriptor
}

func (b *Board) CreateCandidate(actor, project string, input CandidateInput) (Candidate, bool, error) {
	var result Candidate
	reused := false
	err := b.change(actor, "candidate.create", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, contribute); err != nil {
			return err
		}
		if err := b.assessmentAvailable(tx); err != nil {
			return err
		}
		if input.Manifest.ProductID != b.product {
			return invalid("manifest product does not match the project")
		}
		if !utf8.ValidString(input.DisplayVersion) || !utf8.ValidString(input.Correction) {
			return invalid("display version and correction must use valid UTF-8")
		}
		manifest, err := input.Manifest.Descriptor()
		if err != nil {
			return invalid(err.Error())
		}
		var predecessor candidatePredecessor
		if err := visitDescriptorCatalog(tx, func(candidate Candidate, manifest StoredManifest, baseline ApprovedBaseline) {
			predecessor.consider(input, candidate, manifest.Manifest, baseline.Baseline)
		}); err != nil {
			return err
		}
		approved, err := getApprovedBaseline(tx, input.BaselineDigest)
		if err != nil {
			return err
		}
		result, reused, err = b.publishCandidate(tx, actor, input, manifest, approved, predecessor)
		return err
	})
	return result, reused, b.recordIntegrityIssue(actor, err)
}

// publishCandidate owns material reuse, lineage and ordered record publication.
// Its caller has already authorized and checked the complete descriptor catalog
// in this same transaction. Errors preserve the existing result/reuse contract.
func (b *Board) publishCandidate(tx *bolt.Tx, actor string, input CandidateInput, manifest Descriptor, approved ApprovedBaseline, predecessor candidatePredecessor) (result Candidate, reused bool, err error) {
	material, err := materialDescriptor(input.Manifest, approved.Baseline)
	if err != nil {
		return result, reused, err
	}
	candidateID := "C-" + digest([]byte(manifest.Digest+approved.Descriptor.Digest))
	if tx.Bucket([]byte(tombstoneBucket)).Get([]byte(candidateID)) != nil {
		return result, reused, fmt.Errorf("%w: this candidate identity has a purged-record marker", ErrConflict)
	}
	existingID, err := getJSON[string](tx, materialBucket, material.Digest)
	if err == nil {
		result, err = getJSON[Candidate](tx, candidateBucket, existingID)
		return result, true, err
	}
	if !errors.Is(err, ErrNotFound) {
		return result, reused, err
	}
	predecessorID, changes, err := predecessor.resolve(input, approved.Baseline)
	if err != nil {
		return result, reused, err
	}
	display := input.DisplayVersion
	if display == "" {
		display = input.Manifest.ApplicationVersion
	}
	result = Candidate{
		ID: candidateID, ManifestDigest: manifest.Digest, BaselineDigest: approved.Descriptor.Digest,
		MaterialDigest: material.Digest, DisplayVersion: display, PredecessorID: predecessorID,
		Correction: input.Correction, Changes: changes, CreatedAt: b.now(), CreatedBy: actor,
	}
	binding := recordset.Key{Bucket: candidateMetaBucket, Key: candidateID}
	if err := recordset.WriteJSON(tx,
		recordset.JSONRecord{Key: recordset.Key{Bucket: manifestBucket, Key: manifest.Digest}, Value: StoredManifest{Manifest: input.Manifest, Descriptor: manifest}},
		recordset.JSONRecord{Key: recordset.Key{Bucket: candidateBucket, Key: candidateID}, Value: result, Binding: &binding},
		recordset.JSONRecord{Key: recordset.Key{Bucket: materialBucket, Key: material.Digest}, Value: candidateID}); err != nil {
		return result, reused, err
	}
	if _, err := increment(tx, "data_revision"); err != nil {
		return result, reused, err
	}
	return result, reused, b.audit(tx, actor, "candidate.create", "success", candidateID, predecessorID)
}

func materialDescriptor(manifest Manifest, baseline Baseline) (Descriptor, error) {
	behaviors := make([]string, 0, len(baseline.Behaviors))
	for _, behavior := range baseline.Behaviors {
		tests := make([]string, 0, len(behavior.Tests))
		for _, test := range behavior.Tests {
			text, err := describe([]any{test.ExpectedOutcome, test.MustPass, test.PlannedAttempts})
			if err != nil {
				return Descriptor{}, err
			}
			tests = append(tests, text.Text)
		}
		slices.Sort(tests)
		testParts := make([]any, 0, len(tests))
		for _, test := range tests {
			testParts = append(testParts, test)
		}
		text, err := describe([]any{behavior.Text, behavior.Required, behavior.Policy.MinPassBasisPoints, behavior.Policy.MaxAgeHours, testParts})
		if err != nil {
			return Descriptor{}, err
		}
		behaviors = append(behaviors, text.Text)
	}
	slices.Sort(behaviors)
	content := make([]any, 0, len(behaviors))
	for _, behavior := range behaviors {
		content = append(content, behavior)
	}
	return describe([]any{
		"reb-material/1", manifest.BuildDigest, manifest.PromptDigest, manifest.ModelConfigDigest,
		manifest.KnowledgeDigest, manifest.EnvironmentID, content,
	})
}

type candidatePredecessor struct {
	id        string
	createdAt time.Time
	manifest  Manifest
	baseline  Baseline
}

func (selected *candidatePredecessor) consider(input CandidateInput, candidate Candidate, manifest Manifest, baseline Baseline) {
	if input.PredecessorID != "" {
		if candidate.ID != input.PredecessorID {
			return
		}
	} else {
		if manifest.ReleaseID != input.Manifest.ReleaseID && !sameExecution(manifest, input.Manifest) {
			return
		}
		if cmp.Or(candidate.CreatedAt.Compare(selected.createdAt), strings.Compare(candidate.ID, selected.id)) <= 0 {
			return
		}
	}
	*selected = candidatePredecessor{id: candidate.ID, createdAt: candidate.CreatedAt, manifest: manifest, baseline: baseline}
}

func (selected candidatePredecessor) resolve(input CandidateInput, baseline Baseline) (string, []string, error) {
	if selected.id == "" {
		if input.PredecessorID != "" {
			return "", nil, ErrNotFound
		}
		if input.Correction != "" {
			return "", nil, invalid("a correction requires a retained predecessor")
		}
		return "", nil, nil
	}
	changes, err := changedInputs(selected.manifest, input.Manifest, selected.baseline, baseline)
	if err != nil {
		return "", nil, err
	}
	return selected.id, changes, nil
}

func sameExecution(a, b Manifest) bool {
	return a.BuildDigest == b.BuildDigest && a.PromptDigest == b.PromptDigest &&
		a.ModelConfigDigest == b.ModelConfigDigest && a.KnowledgeDigest == b.KnowledgeDigest && a.EnvironmentID == b.EnvironmentID
}

func changedInputs(a, b Manifest, old, next Baseline) ([]string, error) {
	var changes []string
	for _, input := range []struct{ Name, Before, After string }{
		{"Build artifact", a.BuildDigest, b.BuildDigest}, {"Prompt revision", a.PromptDigest, b.PromptDigest},
		{"Model configuration", a.ModelConfigDigest, b.ModelConfigDigest}, {"Knowledge revision", a.KnowledgeDigest, b.KnowledgeDigest},
		{"Environment or harness revision", a.EnvironmentID, b.EnvironmentID},
	} {
		if input.Before != input.After {
			changes = append(changes, input.Name)
		}
	}
	priorContract, err := materialDescriptor(b, old)
	if err != nil {
		return nil, err
	}
	nextContract, err := materialDescriptor(b, next)
	if err != nil {
		return nil, err
	}
	if priorContract.Digest != nextContract.Digest {
		changes = append(changes, "Approved behavior, roster or policy")
	}
	return changes, nil
}

func (b *Board) Candidates(actor, project string) ([]Candidate, error) {
	var candidates []Candidate
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		if err := verifyCompanionKeys(tx, candidateMetaBucket, candidateBucket); err != nil {
			return err
		}
		return visitJSON(tx, candidateBucket, func(key string, stored Candidate) error {
			if key != stored.ID {
				return integrity("candidate key is inconsistent")
			}
			if err := checkRecordBinding(tx, candidateBucket, candidateMetaBucket, key); err != nil {
				return err
			}
			candidates = append(candidates, stored)
			return nil
		})
	})
	return candidates, b.recordIntegrityIssue(actor, err)
}

func (b *Board) Candidate(actor, project, id string) (Candidate, StoredManifest, ApprovedBaseline, error) {
	var candidate Candidate
	var manifest StoredManifest
	var baseline ApprovedBaseline
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		var err error
		candidate, manifest, baseline, err = canonicalCandidateRecords(tx, id)
		return err
	})
	return candidate, manifest, baseline, b.recordIntegrityIssue(actor, err)
}

func candidateRecords(tx *bolt.Tx, id string) (Candidate, StoredManifest, ApprovedBaseline, error) {
	candidate, err := getJSON[Candidate](tx, candidateBucket, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) && tx.Bucket([]byte(candidateMetaBucket)).Get([]byte(id)) != nil {
			err = integrity("retained candidate record is missing")
		}
		return Candidate{}, StoredManifest{}, ApprovedBaseline{}, err
	}
	if err := checkRecordBinding(tx, candidateBucket, candidateMetaBucket, id); err != nil {
		return Candidate{}, StoredManifest{}, ApprovedBaseline{}, err
	}
	manifest, err := getJSON[StoredManifest](tx, manifestBucket, candidate.ManifestDigest)
	if errors.Is(err, ErrNotFound) {
		err = integrity("retained candidate manifest is missing")
	}
	if err != nil {
		return Candidate{}, StoredManifest{}, ApprovedBaseline{}, err
	}
	baseline, err := getApprovedBaseline(tx, candidate.BaselineDigest)
	if errors.Is(err, ErrNotFound) {
		err = integrity("retained candidate baseline is missing")
	}
	return candidate, manifest, baseline, err
}
