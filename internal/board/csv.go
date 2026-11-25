package board

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ResultSchema = "reb-results/1.3"
	MaxCSVBytes  = 10 * 1024 * 1024
	MaxCSVRows   = 10_000
)

const (
	colSchema = iota
	colType
	colResult
	colProduct
	colRelease
	colManifest
	colBaseline
	colBehavior
	colBehaviorRev
	colPolicy
	colPolicyRev
	colTest
	colRun
	colAttempt
	colFinished
	colRunFinished
	colOutcome
	colSource
	colExcerpt
	columnCount
)

var resultColumns = [columnCount]string{
	"schema_version", "record_type", "result_id", "product_id", "release_id", "manifest_digest",
	"baseline_digest", "behavior_id", "behavior_revision", "policy_id", "policy_revision",
	"test_id", "run_id", "attempt_index", "finished_at", "run_finished_at", "outcome",
	"source_uri", "failure_excerpt",
}

const (
	Result  = "RESULT"
	RunOnly = "RUN_ONLY"
	Pass    = "PASS"
	Fail    = "FAIL"
	Unknown = "UNKNOWN"
	Stale   = "STALE"
)

type SourceRow struct {
	Fields                   [columnCount]string
	LogicalRow, PhysicalLine int
}

func (r SourceRow) Kind() string       { return r.Fields[colType] }
func (r SourceRow) ResultID() string   { return r.Fields[colResult] }
func (r SourceRow) RunID() string      { return r.Fields[colRun] }
func (r SourceRow) TestID() string     { return r.Fields[colTest] }
func (r SourceRow) Outcome() string    { return r.Fields[colOutcome] }
func (r SourceRow) FinishedAt() string { return r.Fields[colFinished] }
func (r SourceRow) ClosedAt() string   { return r.Fields[colRunFinished] }
func (r SourceRow) SourceURI() string  { return r.Fields[colSource] }
func (r SourceRow) Excerpt() string    { return r.Fields[colExcerpt] }
func (r SourceRow) Attempt() string    { return r.Fields[colAttempt] }

type Diagnostic struct {
	LogicalRow, PhysicalLine int
	Column, Code, Message    string
}

func diagnostic(row SourceRow, col int, code, message string) Diagnostic {
	return Diagnostic{LogicalRow: row.LogicalRow, PhysicalLine: row.PhysicalLine, Column: resultColumns[col], Code: code, Message: message}
}

// The scanner retains every parsed field byte, including CRLF inside quotes.
type csvScanner struct {
	raw     []byte
	index   int
	line    int
	logical int
}

func ParseResultsCSV(raw []byte) ([]SourceRow, []Diagnostic) {
	if len(raw) > MaxCSVBytes {
		return nil, []Diagnostic{{Code: "FILE_TOO_LARGE", Message: "CSV exceeds 10 MiB"}}
	}
	data := bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	scanner := csvScanner{raw: data, line: 1, logical: 1}
	header, fields, problem := scanner.record()
	if problem != nil {
		return nil, []Diagnostic{*problem}
	}
	if fields != columnCount {
		return nil, []Diagnostic{{LogicalRow: 1, PhysicalLine: 1, Code: "HEADER", Message: "CSV requires the exact 19-column results header"}}
	}
	for col, name := range resultColumns {
		if header.Fields[col] != name {
			return nil, []Diagnostic{diagnostic(header, col, "HEADER", "Use the exact results header and column order")}
		}
	}
	var rows []SourceRow
	var issues []Diagnostic
	for scanner.index < len(scanner.raw) {
		if scanner.logical > MaxCSVRows+1 {
			issues = append(issues, Diagnostic{LogicalRow: scanner.logical, PhysicalLine: scanner.line, Code: "TOO_MANY_ROWS", Message: "CSV exceeds 10,000 data rows"})
			break
		}
		row, count, problem := scanner.record()
		if problem != nil {
			issues = append(issues, *problem)
			break
		}
		if count == columnCount {
			issues = append(issues, validateRowValues(row)...)
			rows = append(rows, row)
		} else {
			issues = append(issues, Diagnostic{LogicalRow: row.LogicalRow, PhysicalLine: row.PhysicalLine, Code: "COLUMN_COUNT", Message: "Each record requires exactly 19 columns"})
		}
	}
	return rows, issues
}

func (c *csvScanner) record() (SourceRow, int, *Diagnostic) {
	row := SourceRow{LogicalRow: c.logical, PhysicalLine: c.line}
	columns := 0
	for {
		field, err := c.field()
		if err != nil {
			issue := Diagnostic{LogicalRow: row.LogicalRow, PhysicalLine: row.PhysicalLine, Code: "CSV_SYNTAX", Message: err.Error()}
			if columns < columnCount {
				issue.Column = resultColumns[columns]
			}
			return row, columns, &issue
		}
		if columns < columnCount {
			row.Fields[columns] = field
		}
		columns++
		if c.index == len(c.raw) {
			break
		}
		switch c.raw[c.index] {
		case ',':
			c.index++
			continue
		case '\n':
			c.index++
			c.line++
		case '\r':
			if c.index+1 == len(c.raw) || c.raw[c.index+1] != '\n' {
				return row, columns, &Diagnostic{LogicalRow: row.LogicalRow, PhysicalLine: row.PhysicalLine, Code: "CSV_SYNTAX", Message: "Use LF or CRLF line breaks"}
			}
			c.index += 2
			c.line++
		default:
			issue := Diagnostic{LogicalRow: row.LogicalRow, PhysicalLine: row.PhysicalLine, Code: "CSV_SYNTAX", Message: "A quoted field must be followed by a delimiter or line ending"}
			if columns <= columnCount {
				issue.Column = resultColumns[columns-1]
			}
			return row, columns, &issue
		}
		break
	}
	c.logical++
	return row, columns, nil
}

func (c *csvScanner) field() (string, error) {
	if c.index < len(c.raw) && c.raw[c.index] == '"' {
		return c.quotedField()
	}
	start := c.index
	for c.index < len(c.raw) {
		char := c.raw[c.index]
		if char == ',' || char == '\n' || char == '\r' {
			break
		}
		if char == '"' {
			return "", fmt.Errorf("quotes inside a quoted field must be doubled")
		}
		c.index++
	}
	return string(c.raw[start:c.index]), nil
}

func (c *csvScanner) quotedField() (string, error) {
	var value strings.Builder
	c.index++
	for c.index < len(c.raw) {
		char := c.raw[c.index]
		c.index++
		if char == '"' {
			if c.index == len(c.raw) || c.raw[c.index] != '"' {
				return value.String(), nil
			}
			c.index++
		}
		if char == '\n' {
			c.line++
		}
		value.WriteByte(char)
	}
	return "", fmt.Errorf("quoted field has no closing quote")
}

func validateRowValues(row SourceRow) []Diagnostic {
	var issues []Diagnostic
	add := func(col int, code, message string) {
		issues = append(issues, diagnostic(row, col, code, message))
	}
	fields := row.Fields
	only := row.Kind() == RunOnly
	for col, value := range fields {
		if strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
			add(col, "ENCODING", "Use valid UTF-8 text without NUL characters")
		}
		emptyForRunOnly := col == colResult || col == colTest || col == colAttempt || col == colFinished ||
			col == colOutcome || col == colSource || col == colExcerpt
		if only && emptyForRunOnly {
			if value != "" {
				add(col, "RUN_ONLY_FIELD", "Leave result fields empty for RUN_ONLY")
			}
			continue
		}
		if value == "" && col != colSource && col != colExcerpt {
			add(col, "REQUIRED_FIELD", "Supply a value for this field")
		}
	}
	if fields[colSchema] != ResultSchema {
		add(colSchema, "SCHEMA_VERSION", "Use reb-results/1.3")
	}
	if row.Kind() != Result && !only {
		add(colType, "RECORD_TYPE", "Use RESULT or RUN_ONLY")
	}
	for _, col := range []int{colResult, colProduct, colRelease, colBehavior, colPolicy, colTest, colRun} {
		if fields[col] != "" && !idPattern.MatchString(fields[col]) {
			add(col, "ID", "Use 1–80 ASCII identifier characters")
		}
	}
	for _, col := range []int{colManifest, colBaseline} {
		if !digestPattern.MatchString(fields[col]) {
			add(col, "DIGEST", "Use a lowercase SHA-256 digest")
		}
	}
	for _, col := range []int{colBehaviorRev, colPolicyRev, colAttempt} {
		if only && col == colAttempt {
			continue
		}
		if _, ok := positiveDecimal(fields[col]); !ok {
			add(col, "INTEGER", "Use a positive decimal integer without leading zeros")
		}
	}
	closed, closedOK := parseTimestamp(fields[colRunFinished])
	if !closedOK {
		add(colRunFinished, "TIMESTAMP", "Use a UTC timestamp with seconds, up to six fractional digits and a final Z")
	}
	if only {
		return issues
	}
	finished, finishedOK := parseTimestamp(row.FinishedAt())
	switch {
	case !finishedOK:
		add(colFinished, "TIMESTAMP", "Use a UTC timestamp with seconds, up to six fractional digits and a final Z")
	case closedOK && finished.After(closed):
		add(colFinished, "AFTER_RUN_FINISH", "A result cannot finish after its run closes")
	}
	switch row.Outcome() {
	case Pass, Fail, "ERROR", "SKIPPED", Unknown:
	default:
		add(colOutcome, "OUTCOME", "Use PASS, FAIL, ERROR, SKIPPED or UNKNOWN")
	}
	if row.SourceURI() != "" && !validSourceURI(row.SourceURI()) {
		add(colSource, "SOURCE_URI", "Use an absolute HTTPS URL with a hostname, no credentials and at most 2,048 characters")
	}
	if utf8.RuneCountInString(row.Excerpt()) > 4000 {
		add(colExcerpt, "EXCERPT", "Use at most 4,000 characters of plain text")
	}
	return issues
}

var (
	decimalPattern   = regexp.MustCompile(`^[1-9][0-9]*$`)
	timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?Z$`)
)

func positiveDecimal(value string) (int, bool) {
	if !decimalPattern.MatchString(value) {
		return 0, false
	}
	number, err := strconv.Atoi(value)
	return number, err == nil
}

func parseTimestamp(value string) (time.Time, bool) {
	if !timestampPattern.MatchString(value) {
		return time.Time{}, false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	return timestamp, err == nil
}

func validSourceURI(value string) bool {
	if utf8.RuneCountInString(value) > 2048 || strings.ContainsAny(value, " \r\n\t\\") {
		return false
	}
	uri, err := url.Parse(value)
	return err == nil && uri.Scheme == "https" && uri.Hostname() != "" && uri.User == nil
}
