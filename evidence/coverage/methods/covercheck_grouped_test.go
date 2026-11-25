package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestGroupedSwitchCapability(t *testing.T) {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	goExe := filepath.Join(runtime.GOROOT(), "bin", "go"+suffix)
	adapter := filepath.Join(t.TempDir(), "covercheck"+suffix)
	build := exec.CommandContext(t.Context(), goExe, "build", "-o", adapter, "covercheck.go", "templatecheck.go")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native adapter: %v\n%s", err, output)
	}
	fixture, err := filepath.Abs(filepath.Join("..", "fixtures", "coverage-grouped-switch"))
	if err != nil {
		t.Fatal(err)
	}
	evidence := os.Getenv("REB_GROUPED_EVIDENCE")
	if evidence == "" {
		evidence = t.TempDir()
	}
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	measure := func(t *testing.T, name, directory, selection string) (report, []byte) {
		t.Helper()
		path := filepath.Join(evidence, name+".json")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("measurement path is not fresh: %s, %v", path, err)
		}
		command := exec.CommandContext(t.Context(), adapter, goExe, directory, path, runtime.GOOS+"-"+runtime.GOARCH, selection)
		output, err := command.CombinedOutput()
		writeGroupedEvidence(t, evidence, name+".stdout", output)
		if err != nil {
			t.Fatalf("instrumented execution failed: %v\n%s", err, output)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var result report
		if err := json.Unmarshal(raw, &result); err != nil || result.TestExit != 0 {
			t.Fatalf("native fixture did not pass under measurement: %v, exit=%d\n%s", err, result.TestExit, output)
		}
		return result, output
	}
	t.Run("partial_success", func(t *testing.T) {
		result, _ := measure(t, "partial", fixture, "^TestPartialGroupedSwitchValues$")
		assertGroupedRoleOutcomes(t, fixture, result, false)
	})
	t.Run("all_successes", func(t *testing.T) {
		result, _ := measure(t, "complete", fixture, "^TestCompleteGroupedSwitchValues$")
		assertGroupedRoleOutcomes(t, fixture, result, true)
	})
	t.Run("ordinary_equivalence", func(t *testing.T) {
		const selection = "^TestGroupedSwitchOrdinarySemantics$"
		command := exec.CommandContext(t.Context(), goExe, "test", "-count=1", "-json", "-run", selection, "./...")
		command.Dir = fixture
		ordinary, err := command.CombinedOutput()
		writeGroupedEvidence(t, evidence, "ordinary.stdout", ordinary)
		if err != nil {
			t.Fatalf("ordinary fixture failed: %v\n%s", err, ordinary)
		}
		_, measured := measure(t, "equivalent", fixture, selection)
		want, got := groupedNativeTraces(t, ordinary), groupedNativeTraces(t, measured)
		if len(want) != 11 || !slices.Equal(got, want) {
			t.Fatalf("instrumented dispatch differed: got %q, want %q", got, want)
		}
		t.Logf("ordinary and instrumented dispatch agree on %d runtime traces", len(got))
	})
	for _, sample := range []struct{ name, source, diagnostic string }{
		{"grouped_type", `func Match(x any) bool { switch x.(type) { case int, string: return true }; return false }`, "grouped type switch"},
		{"interface_tag", `func Match(x any) bool { switch x { case 1, "two": return true }; return false }`, "grouped switch tag"},
		{"type_parameter", `func Match[T ~string](x T) bool { switch x { case "one", "two": return true }; return false }`, "grouped switch tag"},
		{"fallthrough", `func Match(x int) bool { switch x { case 1, 2: fallthrough; case 3: return true }; return false }`, "fallthrough"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGroupedEvidence(t, dir, "go.mod", []byte("module unsupported-fixture\n\ngo 1.27.4\n"))
			writeGroupedEvidence(t, dir, "fixture.go", []byte("package fixture\n"+sample.source+"\n"))
			ordinary := exec.CommandContext(t.Context(), goExe, "test", "-json", "./...")
			ordinary.Dir = dir
			output, err := ordinary.CombinedOutput()
			writeGroupedEvidence(t, evidence, sample.name+".ordinary.stdout", output)
			if err != nil {
				t.Fatalf("unsupported measurement control must be legal Go: %v\n%s", err, output)
			}
			path := filepath.Join(evidence, sample.name+".json")
			command := exec.CommandContext(t.Context(), adapter, goExe, dir, path, runtime.GOOS+"-"+runtime.GOARCH, "^")
			output, err = command.CombinedOutput()
			writeGroupedEvidence(t, evidence, sample.name+".stdout", output)
			if err == nil || !bytes.Contains(output, []byte(sample.diagnostic)) {
				t.Fatalf("expected fail-closed %q diagnostic, got error=%v\n%s", sample.diagnostic, err, output)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("unsupported switch published coverage data: %v", err)
			}
			t.Logf("unsupported instrumentation refused: %s", strings.TrimSpace(string(output)))
		})
	}
}

func assertGroupedRoleOutcomes(t *testing.T, fixture string, result report, contributorMatched bool) {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(fixture, "switches.go"))
	if err != nil {
		t.Fatal(err)
	}
	const clause = "case Viewer, Contributor:"
	line := 0
	caseSource := ""
	for index, text := range strings.Split(string(source), "\n") {
		if strings.Contains(text, clause) {
			line, caseSource = index+1, text
			break
		}
	}
	if line == 0 {
		t.Fatal("expected grouped switch fixture clause is absent")
	}
	var clauseSites []site
	noMatch := false
	for _, file := range result.Files {
		if file.Path != "switches.go" {
			continue
		}
		for _, item := range file.Sites {
			if item.Line == line && item.Kind != "line" {
				clauseSites = append(clauseSites, item)
			}
			if item.Line == line-1 && item.Kind == "switch:no-match" && item.Hit {
				noMatch = true
			}
		}
	}
	t.Logf("original clause %d outcomes=%+v no-match-observed=%t", line, clauseSites, noMatch)
	if len(clauseSites) != 4 || !noMatch {
		t.Fatalf("expected four independent case-alternative outcomes and the unmatched-switch edge, got %d outcomes and no-match=%t", len(clauseSites), noMatch)
	}
	for _, expected := range []struct {
		text, kind string
		hit        bool
	}{
		{"Viewer", "switch:alternative:true", true}, {"Viewer", "switch:alternative:false", true},
		{"Contributor", "switch:alternative:true", contributorMatched}, {"Contributor", "switch:alternative:false", true},
	} {
		column := strings.Index(caseSource, expected.text) + 1
		index := slices.IndexFunc(clauseSites, func(item site) bool {
			return item.Column == column && item.Kind == expected.kind
		})
		if index < 0 || clauseSites[index].Hit != expected.hit {
			t.Errorf("case %s at %d expects %s hit=%t, observed %+v", expected.text, column, expected.kind, expected.hit, clauseSites)
		}
	}
}

func groupedNativeTraces(t *testing.T, output []byte) []string {
	t.Helper()
	var traces []string
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte{'\n'}) {
		var event struct{ Action, Output string }
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("expected a Go test event: %v\n%s", err, line)
		}
		if _, trace, ok := strings.Cut(event.Output, "TRACE "); event.Action == "output" && ok {
			traces = append(traces, strings.TrimSpace(trace))
		}
	}
	return traces
}

func writeGroupedEvidence(t *testing.T, directory, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}
