package board

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxBehaviors = 500
	MaxAttempts  = 10_000
)

var (
	idPattern     = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Descriptor struct {
	Text   string
	Digest string
}

type Manifest struct {
	ProductID, ReleaseID, ApplicationVersion     string
	BuildDigest, PromptDigest, ModelConfigDigest string
	KnowledgeDigest, EnvironmentID               string
}

type Baseline struct {
	ProductID, ID string
	Revision      int
	Behaviors     []Behavior
}

type Behavior struct {
	ID       string
	Revision int
	Required bool
	Text     string
	Policy   Policy
	Tests    []TestPlan
}

type Policy struct {
	ID                 string
	Revision           int
	MinPassBasisPoints int
	MaxAgeHours        int
}

type TestPlan struct {
	ID, ExpectedOutcome string
	MustPass            bool
	PlannedAttempts     int
}

func digest(data []byte) string {
	value := sha256.Sum256(data)
	return hex.EncodeToString(value[:])
}

func describe(parts []any) (Descriptor, error) {
	var out strings.Builder
	if err := writeCanonical(&out, parts); err != nil {
		return Descriptor{}, err
	}
	return Descriptor{Text: out.String(), Digest: digest([]byte(out.String()))}, nil
}

func writeCanonical(out *strings.Builder, value any) error {
	switch value := value.(type) {
	case []any:
		out.WriteByte('[')
		for i, item := range value {
			if i != 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case string:
		if !utf8.ValidString(value) {
			return fmt.Errorf("descriptor contains invalid UTF-8")
		}
		out.WriteByte('"')
		for _, char := range value {
			switch {
			case char == '"' || char == '\\':
				out.WriteByte('\\')
				out.WriteRune(char)
			case char <= 31:
				const hexDigits = "0123456789abcdef"
				out.WriteString("\\u00")
				out.WriteByte(hexDigits[char>>4])
				out.WriteByte(hexDigits[char&15])
			default:
				out.WriteRune(char)
			}
		}
		out.WriteByte('"')
	case int:
		out.WriteString(strconv.Itoa(value))
	case uint64:
		out.WriteString(strconv.FormatUint(value, 10))
	case bool:
		out.WriteString(strconv.FormatBool(value))
	default:
		return fmt.Errorf("descriptor value has an unsupported type")
	}
	return nil
}

func (m Manifest) Descriptor() (Descriptor, error) {
	if !idPattern.MatchString(m.ProductID) || !idPattern.MatchString(m.ReleaseID) || !idPattern.MatchString(m.EnvironmentID) {
		return Descriptor{}, fmt.Errorf("product, release and environment revision IDs must use 1–80 ASCII identifier characters")
	}
	if m.ApplicationVersion == "" || !utf8.ValidString(m.ApplicationVersion) {
		return Descriptor{}, fmt.Errorf("application version must be nonempty valid UTF-8")
	}
	if !digestPattern.MatchString(m.BuildDigest) {
		return Descriptor{}, fmt.Errorf("build digest must be 64 lowercase hexadecimal characters")
	}
	for _, component := range []struct{ name, value string }{
		{"prompt", m.PromptDigest}, {"model configuration", m.ModelConfigDigest}, {"knowledge", m.KnowledgeDigest},
	} {
		if component.value != "not_applicable" && !digestPattern.MatchString(component.value) {
			return Descriptor{}, fmt.Errorf("%s digest must be 64 lowercase hexadecimal characters or not_applicable", component.name)
		}
	}
	return describe([]any{"reb-manifest/1", m.ProductID, m.ReleaseID, m.ApplicationVersion, m.BuildDigest, m.PromptDigest, m.ModelConfigDigest, m.KnowledgeDigest, m.EnvironmentID})
}

func (b Baseline) Descriptor() (Descriptor, error) {
	if !idPattern.MatchString(b.ProductID) || !idPattern.MatchString(b.ID) || b.Revision < 1 {
		return Descriptor{}, fmt.Errorf("baseline requires valid product and baseline IDs and a positive revision")
	}
	if len(b.Behaviors) > MaxBehaviors {
		return Descriptor{}, fmt.Errorf("baseline exceeds 500 behaviors")
	}
	behaviors := slices.Clone(b.Behaviors)
	slices.SortFunc(behaviors, func(a, b Behavior) int { return strings.Compare(a.ID, b.ID) })
	out := make([]any, 0, len(behaviors))
	required := false
	planned := 0
	behaviorIDs := make(map[string]bool)
	testIDs := make(map[string]bool)
	for _, behavior := range behaviors {
		if behaviorIDs[behavior.ID] {
			return Descriptor{}, fmt.Errorf("duplicate behavior ID %q", behavior.ID)
		}
		behaviorIDs[behavior.ID] = true
		tests, attempts, err := canonicalTests(behavior, testIDs)
		if err != nil {
			return Descriptor{}, err
		}
		planned += attempts
		required = required || behavior.Required
		policy := behavior.Policy
		out = append(out, []any{behavior.ID, behavior.Revision, behavior.Required, behavior.Text, policy.ID, policy.Revision, policy.MinPassBasisPoints, policy.MaxAgeHours, tests})
	}
	if !required {
		return Descriptor{}, fmt.Errorf("baseline requires at least one required behavior")
	}
	if planned > MaxAttempts {
		return Descriptor{}, fmt.Errorf("baseline exceeds 10,000 planned attempts")
	}
	return describe([]any{"reb-baseline/1", b.ProductID, b.ID, b.Revision, out})
}

func canonicalTests(behavior Behavior, testIDs map[string]bool) ([]any, int, error) {
	if !idPattern.MatchString(behavior.ID) || behavior.Revision < 1 || !validText(behavior.Text, 4000) {
		return nil, 0, fmt.Errorf("behavior requires a valid ID, positive revision and 1–4000 Unicode characters of text")
	}
	policy := behavior.Policy
	if !idPattern.MatchString(policy.ID) || policy.Revision < 1 || policy.MinPassBasisPoints < 0 || policy.MinPassBasisPoints > 10000 || policy.MaxAgeHours < 1 || policy.MaxAgeHours > 720 {
		return nil, 0, fmt.Errorf("invalid policy for behavior %q: use a valid ID and positive revision, 0–10000 basis points and 1–720 hours", behavior.ID)
	}
	if len(behavior.Tests) == 0 {
		return nil, 0, fmt.Errorf("behavior %q requires a planned test", behavior.ID)
	}
	tests := slices.Clone(behavior.Tests)
	slices.SortFunc(tests, func(a, b TestPlan) int { return strings.Compare(a.ID, b.ID) })
	out := make([]any, 0, len(tests))
	mustPass := false
	planned := 0
	for _, test := range tests {
		if !idPattern.MatchString(test.ID) || !validText(test.ExpectedOutcome, 2000) || test.PlannedAttempts < 1 || test.PlannedAttempts > 100 {
			return nil, 0, fmt.Errorf("invalid test in behavior %q: use a valid ID, 1–2000 characters of expected outcome and 1–100 attempts", behavior.ID)
		}
		if testIDs[test.ID] {
			return nil, 0, fmt.Errorf("duplicate test ID %q", test.ID)
		}
		testIDs[test.ID] = true
		mustPass = mustPass || test.MustPass
		planned += test.PlannedAttempts
		out = append(out, []any{test.ID, test.ExpectedOutcome, test.MustPass, test.PlannedAttempts})
	}
	if behavior.Required && !mustPass {
		return nil, 0, fmt.Errorf("required behavior %q needs a must-pass test", behavior.ID)
	}
	return out, planned, nil
}

func validText(value string, limit int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit
}
