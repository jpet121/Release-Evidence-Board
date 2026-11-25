package board_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestAC002RosterConstraintBoundaries(t *testing.T) {
	editBehavior := func(change func(*board.Behavior)) func(*board.Baseline) {
		return func(b *board.Baseline) { change(&b.Behaviors[0]) }
	}
	editTest := func(change func(*board.TestPlan)) func(*board.Baseline) {
		return editBehavior(func(b *board.Behavior) { change(&b.Tests[0]) })
	}
	for _, check := range []struct {
		name  string
		valid bool
		edit  func(*board.Baseline)
	}{
		{"pass_mark_0", true, editBehavior(func(b *board.Behavior) { b.Policy.MinPassBasisPoints = 0 })},
		{"pass_mark_10000", true, editBehavior(func(b *board.Behavior) { b.Policy.MinPassBasisPoints = 10000 })},
		{"pass_mark_negative", false, editBehavior(func(b *board.Behavior) { b.Policy.MinPassBasisPoints = -1 })},
		{"pass_mark_10001", false, editBehavior(func(b *board.Behavior) { b.Policy.MinPassBasisPoints = 10001 })},
		{"one_attempt", true, editTest(func(p *board.TestPlan) { p.PlannedAttempts = 1 })},
		{"hundred_attempts", true, editTest(func(p *board.TestPlan) { p.PlannedAttempts = 100 })},
		{"zero_attempts", false, editTest(func(p *board.TestPlan) { p.PlannedAttempts = 0 })},
		{"101_attempts", false, editTest(func(p *board.TestPlan) { p.PlannedAttempts = 101 })},
		{"one_hour", true, editBehavior(func(b *board.Behavior) { b.Policy.MaxAgeHours = 1 })},
		{"720_hours", true, editBehavior(func(b *board.Behavior) { b.Policy.MaxAgeHours = 720 })},
		{"no_age", false, editBehavior(func(b *board.Behavior) { b.Policy.MaxAgeHours = 0 })},
		{"721_hours", false, editBehavior(func(b *board.Behavior) { b.Policy.MaxAgeHours = 721 })},
		{"empty_roster", false, func(b *board.Baseline) { b.Behaviors = nil }},
		{"no_required", false, editBehavior(func(b *board.Behavior) { b.Required = false })},
		{"no_tests", false, editBehavior(func(b *board.Behavior) { b.Tests = nil })},
		{"no_must_pass", false, editTest(func(p *board.TestPlan) { p.MustPass = false })},
		{"duplicate_behavior", false, func(b *board.Baseline) { b.Behaviors = append(b.Behaviors, b.Behaviors[0]) }},
		{"duplicate_test", false, editBehavior(func(b *board.Behavior) { b.Tests = append(b.Tests, b.Tests[0]) })},
		{"empty_behavior_text", false, editBehavior(func(b *board.Behavior) { b.Text = "" })},
		{"4000_behavior_scalars", true, editBehavior(func(b *board.Behavior) { b.Text = strings.Repeat("😀", 4000) })},
		{"4001_behavior_scalars", false, editBehavior(func(b *board.Behavior) { b.Text = strings.Repeat("😀", 4001) })},
		{"empty_expected", false, editTest(func(p *board.TestPlan) { p.ExpectedOutcome = "" })},
		{"2000_expected_scalars", true, editTest(func(p *board.TestPlan) { p.ExpectedOutcome = strings.Repeat("é", 2000) })},
		{"2001_expected_scalars", false, editTest(func(p *board.TestPlan) { p.ExpectedOutcome = strings.Repeat("é", 2001) })},
		{"baseline_revision_zero", false, func(b *board.Baseline) { b.Revision = 0 }},
		{"behavior_revision_zero", false, editBehavior(func(b *board.Behavior) { b.Revision = 0 })},
		{"policy_revision_zero", false, editBehavior(func(b *board.Behavior) { b.Policy.Revision = 0 })},
		{"capacity_500_10000", true, func(b *board.Baseline) { b.Behaviors = capacityRoster(500, 20) }},
		{"capacity_501", false, func(b *board.Baseline) { b.Behaviors = capacityRoster(501, 10) }},
		{"capacity_10001", false, func(b *board.Baseline) {
			b.Behaviors = capacityRoster(500, 20)
			b.Behaviors[499].Tests[0].PlannedAttempts++
		}},
	} {
		t.Run(check.name, func(t *testing.T) {
			baseline := vectorBaseline()
			check.edit(&baseline)
			descriptor, err := baseline.Descriptor()
			if (err == nil) != check.valid {
				t.Fatalf("descriptor validity = %v, want %v: %v", err == nil, check.valid, err)
			}
			if check.valid && len(descriptor.Digest) != 64 {
				t.Fatalf("valid draft lacks canonical digest: %#v", descriptor)
			}
		})
	}
	for _, check := range []struct {
		name  string
		valid bool
		tests []board.TestPlan
		rev   int
	}{
		{"optional_with_non_must_pass", true, []board.TestPlan{{ID: "T2", ExpectedOutcome: "Optional", PlannedAttempts: 1}}, 1},
		{"optional_without_tests", false, nil, 1},
		{"optional_with_invalid_revision", false, []board.TestPlan{{ID: "T2", ExpectedOutcome: "Optional", PlannedAttempts: 1}}, 0},
	} {
		t.Run(check.name, func(t *testing.T) {
			baseline := vectorBaseline()
			extra := vectorBaseline().Behaviors[0]
			extra.Required, extra.ID, extra.Tests, extra.Revision = false, "optional", check.tests, check.rev
			baseline.Behaviors = append(baseline.Behaviors, extra)
			_, err := baseline.Descriptor()
			if (err == nil) != check.valid {
				t.Fatalf("optional roster validity = %v, want %v", err, check.valid)
			}
		})
	}
}

func capacityRoster(behaviors, attempts int) []board.Behavior {
	var plan []board.Behavior
	for i := 0; i < behaviors; i++ {
		id := strconv.Itoa(i)
		plan = append(plan, board.Behavior{
			ID: "B" + id, Revision: 1, Required: i == 0, Text: "Synthetic capacity behavior",
			Policy: board.Policy{ID: "P" + id, Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []board.TestPlan{{ID: "T" + id, ExpectedOutcome: "Capacity check", MustPass: i == 0, PlannedAttempts: attempts}},
		})
	}
	return plan
}

func TestAC004CSVValuesAndDiagnostics(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	c := fixtureCandidate(t, app, vectorBaseline())
	at := "2026-11-17T12:00:00Z"
	valid := resultRow(c, "T1", "1", "RUN1", "RES1", at, at, "PASS")
	tests := []struct {
		name, code, value string
		column            int
	}{
		{"wrong_schema", "SCHEMA_VERSION", "reb-results/1.2", 0},
		{"unknown_record_type", "RECORD_TYPE", "RESULTS", 1},
		{"id_long", "ID", strings.Repeat("X", 81), 2},
		{"cross_product", "PRODUCT", "OTHER", 3},
		{"release_inconsistent", "RELEASE_MISMATCH", "R-incorrect", 4},
		{"unknown_manifest", "UNKNOWN_PAIR", strings.Repeat("a", 64), 5},
		{"unknown_baseline", "UNKNOWN_PAIR", strings.Repeat("b", 64), 6},
		{"wrong_digest_case", "DIGEST", strings.Repeat("B", 64), 6},
		{"unknown_behavior", "UNKNOWN_BEHAVIOR", "OTHER", 7},
		{"wrong_behavior_revision", "BEHAVIOR_REVISION", "2", 8},
		{"wrong_policy", "POLICY_ID", "OTHER", 9},
		{"wrong_policy_revision", "POLICY_REVISION", "2", 10},
		{"unknown_test", "UNKNOWN_TEST", "T2", 11},
		{"space_run", "ID", " RUN1", 12},
		{"attempt_above", "UNPLANNED_ATTEMPT", "2", 13},
		{"attempt_zero", "INTEGER", "0", 13},
		{"invalid_date", "TIMESTAMP", "2026-02-30T12:00:00Z", 14},
		{"leap_second", "TIMESTAMP", "2026-11-17T11:59:60Z", 14},
		{"decimal_too_long", "TIMESTAMP", "2026-11-17T12:00:00.0000001Z", 14},
		{"result_after_run", "AFTER_RUN_FINISH", "2026-11-17T12:00:00.000001Z", 14},
		{"future_run", "CLOCK_SKEW_LIMIT", "2026-11-17T12:05:00.000001Z", 15},
		{"wrong_outcome", "OUTCOME", "PAS", 16},
		{"http_uri", "SOURCE_URI", "http://example.invalid/results/1", 17},
		{"uri_password", "SOURCE_URI", "https://user:secret@example.invalid/result", 17},
		{"uri_no_host", "SOURCE_URI", "https:/result", 17},
		{"uri_too_long", "SOURCE_URI", "https://example.invalid/" + strings.Repeat("x", 2025), 17},
		{"excerpt_too_long", "EXCERPT", strings.Repeat("é", 4001), 18},
		{"nul", "ENCODING", "bad\x00data", 18},
		{"invalid_utf8", "ENCODING", string([]byte{0xed, 0xa0, 0x80}), 18},
	}
	for column := 0; column < 17; column++ {
		tests = append(tests, struct {
			name, code, value string
			column            int
		}{fmt.Sprintf("missing_column_%02d", column+1), "REQUIRED_FIELD", "", column})
	}
	for _, revision := range []string{"+1", "-1", "0", "01", "1e0", "1.0"} {
		tests = append(tests, struct {
			name, code, value string
			column            int
		}{"invalid_revision_" + revision, "INTEGER", revision, 8})
	}
	for _, check := range tests {
		t.Run(check.name, func(t *testing.T) {
			row := slices.Clone(valid)
			row[check.column] = check.value
			preview, err := app.PreviewImport(supervisor, project, c.ID, csvBytes(t, row))
			if err != nil || preview.Token != "" {
				t.Fatalf("invalid import became committable: %#v %v", preview, err)
			}
			wantColumn := ""
			if check.name == "nul" {
				wantColumn = "failure_excerpt"
			} else if check.name == "missing_column_16" {
				wantColumn = "run_finished_at"
			}
			if !slices.ContainsFunc(preview.Issues, func(issue board.Diagnostic) bool {
				return issue.Code == check.code && issue.LogicalRow == 2 && issue.PhysicalLine == 2 && issue.Column != "" &&
					(wantColumn == "" || issue.Column == wantColumn)
			}) {
				t.Fatalf("issues = %#v, want row and column with %s", preview.Issues, check.code)
			}
			recordCSVValueInstance(t, check.name, check.column, check.value)
		})
	}
	t.Run("attempt_above_explicit_three_planned", func(t *testing.T) {
		caseApp, _ := openBoard(t, func() time.Time { return fixtureTime })
		baseline := vectorBaseline()
		baseline.Behaviors[0].Tests[0].PlannedAttempts = 3
		candidate := fixtureCandidate(t, caseApp, baseline)
		row := resultRow(candidate, "T1", "4", "RUN1", "RES1", at, at, "PASS")
		preview, err := caseApp.PreviewImport(supervisor, project, candidate.ID, csvBytes(t, row))
		if err != nil || preview.Token != "" || !slices.ContainsFunc(preview.Issues, func(issue board.Diagnostic) bool {
			return issue.Code == "UNPLANNED_ATTEMPT" && issue.LogicalRow == 2 && issue.PhysicalLine == 2 && issue.Column == "attempt_index"
		}) {
			t.Fatalf("attempt four with three planned slots was not rejected at attempt_index: %+v, %v", preview, err)
		}
		recordApprovalInstance(t, "AC-004-08.P001", map[string]any{"plannedAttempts": 3, "attemptIndex": 4}, "Whole file rejected")
	})
	for _, check := range []struct {
		name, data, issue string
	}{
		{"missing_header_column", strings.Replace(resultsHeader, ",failure_excerpt", "", 1), "HEADER"},
		{"extra_header_column", strings.Replace(resultsHeader, "failure_excerpt", "failure_excerpt,extra", 1), "HEADER"},
		{"duplicate_header_column", strings.Replace(resultsHeader, "failure_excerpt", "source_uri", 1), "HEADER"},
		{"reordered_header", strings.Replace(resultsHeader, "source_uri,failure_excerpt", "failure_excerpt,source_uri", 1), "HEADER"},
		{"bare_quote", resultsHeader + "bad\"data", "CSV_SYNTAX"},
		{"unterminated_quote", resultsHeader + "\"bad\r\n", "CSV_SYNTAX"},
		{"characters_after_quote", resultsHeader + "\"bad\"unsafe\n", "CSV_SYNTAX"},
		{"bare_cr", resultsHeader + "bad\rdata", "CSV_SYNTAX"},
		{"blank_data_line", resultsHeader + "\n", "COLUMN_COUNT"},
	} {
		t.Run(check.name, func(t *testing.T) {
			_, issues := board.ParseResultsCSV([]byte(check.data))
			if len(issues) != 1 || issues[0].Code != check.issue {
				t.Fatalf("diagnostics = %#v, want %s", issues, check.issue)
			}
		})
	}
	t.Run("preserve_quoted_CRLF_and_original_logical_rows", func(t *testing.T) {
		row := slices.Clone(valid)
		row[18] = "CSV, \"quote\"\r\nsecond line"
		next := slices.Clone(valid)
		next[2], next[12] = "RES2", "RUN2"
		raw := append([]byte{0xef, 0xbb, 0xbf}, csvBytes(t, row, next)...)
		rows, issues := board.ParseResultsCSV(raw)
		if len(issues) != 0 || len(rows) != 2 || rows[0].Excerpt() != row[18] || rows[1].LogicalRow != 3 || rows[1].PhysicalLine != 4 {
			t.Fatalf("rows = %#v, issues %#v", rows, issues)
		}
	})
	t.Run("maximum_values", func(t *testing.T) {
		row := slices.Clone(valid)
		row[2], row[12] = strings.Repeat("a", 80), strings.Repeat("b", 80)
		prefix := "https://example.invalid/"
		row[17] = prefix + strings.Repeat("x", 2048-len(prefix))
		row[18] = strings.Repeat("😀", 4000)
		preview, err := app.PreviewImport(supervisor, project, c.ID, csvBytes(t, row))
		if err != nil || preview.Token == "" || len(preview.Issues) != 0 {
			t.Fatalf("maximum valid values = %#v %v", preview, err)
		}
	})
}

func recordCSVValueInstance(t *testing.T, name string, column int, input string) {
	t.Helper()
	const rejected = "Rejected"
	const whole = "Whole file rejected"
	if strings.HasPrefix(name, "missing_column_") {
		fields := strings.Split(strings.TrimSpace(resultsHeader), ",")
		recordApprovalInstance(t, fmt.Sprintf("AC-004-16.P%03d", column+1),
			map[string]any{"recordType": "RESULT", "emptyField": fields[column]}, rejected)
		if name == "missing_column_16" {
			recordApprovalInstance(t, "AC-004-24.P001", map[string]any{"timingDefect": "missing RESULT run_finished_at"},
				"Reject the whole file with the offending timestamp or run identified")
		}
		return
	}
	if strings.HasPrefix(name, "invalid_revision_") {
		for index, lexeme := range []string{"+1", "-1", "0", "01", "1e0"} {
			if input == lexeme {
				recordApprovalInstance(t, fmt.Sprintf("AC-004-12.P%03d", index+6),
					map[string]any{"field": "behavior_revision", "lexeme": input}, whole)
				return
			}
		}
		return
	}
	checks := map[string]struct {
		id, key, value, outcome string
	}{
		"unknown_test":            {"AC-004-07.P001", "referenceDefect", "unknown test", whole},
		"wrong_behavior_revision": {"AC-004-07.P002", "referenceDefect", "wrong behavior_revision", whole},
		"wrong_policy_revision":   {"AC-004-07.P003", "referenceDefect", "wrong policy_revision", whole},
		"nul":                     {"AC-004-10.P002", "syntaxDefect", "NUL", "Whole file rejected with logical row/physical line and column where identifiable"},
		"wrong_outcome":           {"AC-004-12.P001", "valueDefect", "invalid outcome enum", whole},
		"invalid_date":            {"AC-004-12.P002", "valueDefect", "invalid date in finished_at", whole},
		"leap_second":             {"AC-004-12.P004", "valueDefect", "leap second in finished_at", whole},
		"http_uri":                {"AC-013-02.P001", "uriDefect", "non-HTTPS", rejected},
		"uri_password":            {"AC-013-02.P002", "uriDefect", "credentials", rejected},
		"uri_no_host":             {"AC-013-02.P003", "uriDefect", "no host", rejected},
	}
	if check, ok := checks[name]; ok {
		recordApprovalInstance(t, check.id, map[string]any{check.key: check.value}, check.outcome)
		return
	}
	switch name {
	case "id_long":
		recordApprovalInstance(t, "AC-004-19.P001", map[string]any{"field": "result_id", "characters": len(input)}, rejected)
	case "uri_too_long":
		recordApprovalInstance(t, "AC-004-19.P008", map[string]any{"field": "source_uri", "characters": len(input)}, rejected)
	case "excerpt_too_long":
		recordApprovalInstance(t, "AC-004-19.P009", map[string]any{"field": "failure_excerpt", "unicodeScalars": len([]rune(input))}, rejected)
	}
}
