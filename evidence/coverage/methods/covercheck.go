package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const helperPrefix = "__rebMeasure"

type site struct {
	ID     int    `json:"id"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Kind   string `json:"kind"`
	Hit    bool   `json:"hit"`
}

type fileReport struct {
	Path            string `json:"path"`
	Active          bool   `json:"active"`
	Sites           []site `json:"sites"`
	UnmeasuredLines []int  `json:"unmeasured_expression_lines"`
}

type report struct {
	Version   int                  `json:"version"`
	Target    string               `json:"target"`
	Files     []fileReport         `json:"files"`
	TestExit  int                  `json:"test_exit"`
	Observers []browserObservation `json:"browser_observers,omitempty"`
}

type browserObserver struct {
	ID      string            `json:"id"`
	Python  string            `json:"python"`
	Harness string            `json:"harness"`
	Driver  string            `json:"driver"`
	Browser string            `json:"browser"`
	Kind    string            `json:"kind"`
	Output  string            `json:"output"`
	Env     map[string]string `json:"env"`
}

type browserObservation struct {
	ID            string   `json:"id"`
	Argv          []string `json:"argv"`
	Artifact      string   `json:"artifact"`
	ExecutableSHA string   `json:"executable_sha256"`
	Exit          int      `json:"exit"`
	Error         string   `json:"error"`
}

type edit struct {
	offset int
	end    int
	order  int
	code   string
}

type sourceFile struct {
	positions *token.FileSet
	parsed    *ast.File
	typeInfo  *types.Info
	original  []byte
	edits     []edit
	file      fileReport
	nextID    *int
}

func (f *sourceFile) add(pos token.Pos, order int, code string) {
	offset := f.positions.PositionFor(pos, false).Offset
	f.edits = append(f.edits, edit{offset: offset, end: offset, order: order, code: code})
}

func (f *sourceFile) note(pos token.Pos, kind string) string {
	position := f.positions.PositionFor(pos, false)
	*f.nextID++
	s := site{ID: *f.nextID, Line: position.Line, Column: position.Column, Kind: kind}
	f.file.Sites = append(f.file.Sites, s)
	return strconv.Itoa(s.ID)
}

func (f *sourceFile) hit(pos token.Pos, kind string) string {
	return helperPrefix + "(" + f.note(pos, kind) + ");"
}

func (f *sourceFile) wrapCondition(x ast.Expr, depth int, kind string) {
	yes := f.note(x.Pos(), kind+":true")
	no := f.note(x.Pos(), kind+":false")
	line := f.note(x.Pos(), "line")
	f.add(x.Pos(), 100+depth, helperPrefix+"Bool("+yes+","+no+","+line+",")
	f.add(x.End(), -100-depth, ")")
}

type expressionProbe struct {
	expression ast.Expr
	parent     ast.Node
	depth      int
}

func (probe expressionProbe) valueConsumed() bool {
	switch parent := probe.parent.(type) {
	case *ast.CallExpr:
		for _, argument := range parent.Args {
			if argument == probe.expression {
				return true
			}
		}
	case *ast.AssignStmt:
		for _, value := range parent.Rhs {
			if value == probe.expression {
				return parent.Tok == token.ASSIGN || parent.Tok == token.DEFINE || len(parent.Lhs) == 1
			}
		}
	case *ast.ReturnStmt, *ast.BinaryExpr:
		return true
	case *ast.UnaryExpr:
		return parent.Op != token.AND && parent.Op != token.ARROW
	case *ast.KeyValueExpr:
		return parent.Value == probe.expression
	case *ast.CompositeLit:
		for _, value := range parent.Elts {
			if value == probe.expression {
				return true
			}
		}
	case *ast.ValueSpec:
		for _, value := range parent.Values {
			if value == probe.expression {
				return true
			}
		}
	}
	return false
}

func (f *sourceFile) measurableExpression(probe expressionProbe) bool {
	if literal, composite := probe.expression.(*ast.CompositeLit); composite && literal.Type == nil {
		// An elided element literal is legal only in its original composite.
		// Its safely typed evaluated fields can still provide line probes.
		return false
	}
	typeValue := f.typeInfo.Types[probe.expression]
	if !typeValue.IsValue() || typeValue.Value != nil || typeValue.IsNil() || typeValue.Type == nil {
		return false
	}
	if _, tuple := typeValue.Type.(*types.Tuple); tuple {
		return false
	}
	basic, isBasic := typeValue.Type.Underlying().(*types.Basic)
	return (!isBasic || basic.Info()&types.IsUntyped == 0) && probe.valueConsumed()
}

func (f *sourceFile) continuationLines(candidates []expressionProbe) {
	measured := map[int]bool{}
	for _, probe := range f.file.Sites {
		if probe.Kind == "line" {
			measured[probe.Line] = true
		}
	}
	gaps := map[int]bool{}
	for _, candidate := range candidates {
		start := candidate.expression.Pos()
		line := f.positions.PositionFor(start, false).Line
		if measured[line] {
			continue
		}
		// Preserve constant-folding, implicit conversions, multiple-result
		// assignments and addressability. Report any remaining original line
		// rather than projecting an unrelated statement's entry count onto it.
		typed := f.typeInfo.Types[candidate.expression]
		if !typed.IsValue() || typed.Value != nil {
			continue
		}
		if !f.measurableExpression(candidate) {
			gaps[line] = true
			continue
		}
		id := f.note(start, "line")
		f.add(start, 500+candidate.depth, helperPrefix+"Value("+helperPrefix+"Begin("+id+"),")
		f.add(candidate.expression.End(), -500-candidate.depth, ")")
		measured[line] = true
		delete(gaps, line)
	}
	for line := range gaps {
		f.file.UnmeasuredLines = append(f.file.UnmeasuredLines, line)
	}
	sort.Ints(f.file.UnmeasuredLines)
}

func (f *sourceFile) instrument() error {
	if f.parsed.Name.Name == "_" {
		return errors.New("blank package name")
	}
	var stack []ast.Node
	ranges := map[*ast.RangeStmt]string{}
	var expressions []expressionProbe
	var unsupported error
	ast.Inspect(f.parsed, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		var parent ast.Node
		if len(stack) != 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, n)
		if expression, isExpression := n.(ast.Expr); isExpression {
			switch expression.(type) {
			case *ast.FuncLit, *ast.FuncType:
			default:
				expressions = append(expressions, expressionProbe{expression, parent, len(stack)})
			}
		}
		if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, helperPrefix) {
			unsupported = fmt.Errorf("reserved identifier at %s", f.positions.Position(id.Pos()))
		}
		if declaration, ok := n.(*ast.GenDecl); ok {
			if declaration.Tok == token.CONST || declaration.Tok == token.TYPE || declaration.Tok == token.IMPORT {
				stack = stack[:len(stack)-1]
				return false
			}
			if _, atPackageScope := parent.(*ast.File); atPackageScope {
				for _, specification := range declaration.Specs {
					value := specification.(*ast.ValueSpec)
					for _, expression := range value.Values {
						// Multi-valued initializers cannot be passed through a generic hook.
						// Go executes their expressions as part of package initialization.
						// A companion initializer in the same declaration records the absolute
						// source positions independently of whether tests invoke any function.
						kind := f.note(expression.Pos(), "line")
						name := helperPrefix + "Init" + kind
						f.add(declaration.End(), 200, ";var "+name+" = "+helperPrefix+"Init("+kind+")")
					}
				}
			}
		}
		if statement, ok := n.(ast.Stmt); ok {
			communication, selectCase := parent.(*ast.CommClause)
			switch parent.(type) {
			case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause:
				switch statement.(type) {
				case *ast.EmptyStmt, *ast.CaseClause, *ast.CommClause, *ast.LabeledStmt:
				default:
					// The select communication is part of the case grammar.
					// Its case-entry probe fires only if that communication wins.
					if !selectCase || statement != communication.Comm {
						f.add(statement.Pos(), 0, f.hit(statement.Pos(), "line"))
					}
				}
			}
		}
		switch node := n.(type) {
		case *ast.IfStmt:
			if outer, nested := parent.(*ast.IfStmt); nested && outer.Else == node {
				f.add(node.Pos(), -50, "{")
				f.add(node.End(), 200, "}")
			}
			f.wrapCondition(node.Cond, len(stack), "if")
		case *ast.ForStmt:
			if node.Cond != nil {
				f.wrapCondition(node.Cond, len(stack), "for")
			}
			if node.Post != nil {
				// The post statement follows only a complete iteration (or continue).
				// A distinct source line requires a statement-position probe which the
				// current transform cannot add without changing continue semantics.
				if f.positions.Position(node.Post.Pos()).Line != f.positions.Position(node.For).Line {
					unsupported = fmt.Errorf("multiline for post statement at %s", f.positions.Position(node.Post.Pos()))
				}
			}
		case *ast.RangeStmt:
			entered := f.hit(node.For, "range:iteration")
			exhausted := f.hit(node.For, "range:exhausted")
			marker := helperPrefix + "Range" + strconv.Itoa(*f.nextID)
			ranges[node] = marker
			f.add(node.Pos(), 10, "{"+marker+":=true;")
			f.add(node.Body.Lbrace+1, 0, entered)
			f.add(node.End(), 0, ";if "+marker+"{"+exhausted+"}}")
		case *ast.SwitchStmt:
			f.instrumentDefault(node.Body, node.Switch)
			if err := f.instrumentAlternatives(node, len(stack)); err != nil {
				unsupported = err
			}
		case *ast.TypeSwitchStmt:
			f.instrumentDefault(node.Body, node.Switch)
			if groupedCases(node.Body) {
				unsupported = fmt.Errorf("grouped type switch needs a dedicated adapter at %s", f.positions.Position(node.Switch))
			}
		case *ast.CaseClause:
			probe := f.hit(node.Case, "line")
			if len(node.List) <= 1 {
				probe += f.hit(node.Case, "switch:case")
			}
			f.add(node.Colon+1, 0, probe)
		case *ast.CommClause:
			f.add(node.Colon+1, 0, f.hit(node.Case, "select:case")+f.hit(node.Case, "line"))
		case *ast.BinaryExpr:
			if node.Op == token.LAND || node.Op == token.LOR {
				// Recording each operand preserves short circuiting. A right operand
				// that never runs cannot contribute an observed outcome.
				f.wrapCondition(node.X, len(stack), "logical-left:"+node.Op.String())
				f.wrapCondition(node.Y, len(stack), "logical-right:"+node.Op.String())
			}
		case *ast.LabeledStmt:
			unsupported = fmt.Errorf("labeled execution needs a dedicated adapter at %s", f.positions.Position(node.Pos()))
		case *ast.BranchStmt:
			switch node.Tok {
			case token.FALLTHROUGH:
				unsupported = fmt.Errorf("fallthrough needs dispatch-sensitive measurement at %s", f.positions.Position(node.Pos()))
			case token.BREAK:
				f.instrumentBreak(stack[:len(stack)-1], ranges, node)
			}
		}
		return true
	})
	f.continuationLines(expressions)
	return unsupported
}

func groupedCases(body *ast.BlockStmt) bool {
	for _, item := range body.List {
		if len(item.(*ast.CaseClause).List) > 1 {
			return true
		}
	}
	return false
}

func (f *sourceFile) instrumentAlternatives(node *ast.SwitchStmt, depth int) error {
	if !groupedCases(node.Body) {
		return nil
	}
	var tag string
	if node.Tag != nil {
		value := f.typeInfo.TypeOf(node.Tag)
		var basic *types.Basic
		if value != nil {
			basic, _ = value.Underlying().(*types.Basic)
		}
		if basic == nil || basic.Info()&(types.IsBoolean|types.IsString|types.IsNumeric) == 0 {
			return fmt.Errorf("grouped switch tag must have a concrete boolean, string, or numeric type at %s", f.positions.Position(node.Tag.Pos()))
		}
		// Keep the initializer and tag in the original switch scope. Evaluate the
		// tag once and let the tagless switch evaluate comparisons in source order.
		tag = helperPrefix + "Switch" + strconv.Itoa(int(node.Switch))
		offset := f.positions.PositionFor(node.Switch, false).Offset
		f.edits = append(f.edits, edit{offset: offset, end: offset + len("switch"), order: 90, code: "{"})
		f.add(node.Tag.Pos(), 80, tag+":=")
		f.add(node.Tag.End(), 90, ";switch")
		f.add(node.End(), 50, "}")
	}
	for _, item := range node.Body.List {
		clause := item.(*ast.CaseClause)
		for _, expression := range clause.List {
			if len(clause.List) > 1 {
				f.wrapCondition(expression, depth, "switch:alternative")
			}
			if tag != "" {
				// The parentheses keep the whole case expression inside the equality,
				// with any alternative probe outside it and operand probes inside it.
				f.add(expression.Pos(), 101+depth, tag+"==(")
				f.add(expression.End(), -101-depth, ")")
			}
		}
	}
	return nil
}

func (f *sourceFile) instrumentDefault(body *ast.BlockStmt, keyword token.Pos) {
	for _, item := range body.List {
		if item.(*ast.CaseClause).List == nil {
			return
		}
	}
	prefix := "default:"
	if len(body.List) > 0 {
		prefix = ";default:"
	}
	f.add(body.Rbrace, 200, prefix+f.hit(keyword, "switch:no-match"))
}

func (f *sourceFile) instrumentBreak(stack []ast.Node, ranges map[*ast.RangeStmt]string, branch *ast.BranchStmt) {
	for index := len(stack) - 1; index >= 0; index-- {
		switch enclosing := stack[index].(type) {
		case *ast.RangeStmt:
			f.add(branch.Pos(), 10, ranges[enclosing]+"=false;")
			return
		case *ast.ForStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.FuncLit:
			return
		}
	}
}

func (f *sourceFile) generated() ([]byte, error) {
	sort.SliceStable(f.edits, func(i, j int) bool {
		if f.edits[i].offset == f.edits[j].offset {
			return f.edits[i].order < f.edits[j].order
		}
		return f.edits[i].offset < f.edits[j].offset
	})
	var result bytes.Buffer
	offset := 0
	for _, change := range f.edits {
		if change.offset < offset {
			return nil, fmt.Errorf("overlapping instrumentation edits in %s at byte %d", f.file.Path, change.offset)
		}
		result.Write(f.original[offset:change.offset])
		result.WriteString(change.code)
		offset = change.end
	}
	result.Write(f.original[offset:])
	return result.Bytes(), nil
}

type listedPackage struct {
	Dir        string
	Name       string
	ImportPath string
	DepOnly    bool
	Export     string
	GoFiles    []string
	CgoFiles   []string
	Error      *struct{ Err string }
}

func listPackages(goExe, root string) (map[string]listedPackage, map[string]string, error) {
	command := exec.Command(goExe, "list", "-deps", "-export", "-json", "./...")
	command.Dir = root
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("go list: %w: %s", err, stderr.String())
	}
	packages := map[string]listedPackage{}
	exports := map[string]string{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var p listedPackage
		if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, nil, fmt.Errorf("go list decoding: %w", err)
		}
		exports[p.ImportPath] = p.Export
		if p.DepOnly {
			continue
		}
		if len(p.CgoFiles) != 0 {
			return nil, nil, fmt.Errorf("CGO instrumentation is unsupported in %s", p.Dir)
		}
		for _, name := range p.GoFiles {
			path, err := filepath.Rel(root, filepath.Join(p.Dir, name))
			if err != nil {
				return nil, nil, err
			}
			packages[filepath.ToSlash(path)] = p
		}
	}
	return packages, exports, nil
}

type packageSyntax struct {
	positions *token.FileSet
	files     map[string]*ast.File
	info      *types.Info
}

func checkTypes(p listedPackage, exports map[string]string) (packageSyntax, error) {
	positions := token.NewFileSet()
	files := map[string]*ast.File{}
	var syntax []*ast.File
	for _, name := range p.GoFiles {
		file, err := parser.ParseFile(positions, filepath.Join(p.Dir, name), nil, parser.ParseComments)
		if err != nil {
			return packageSyntax{}, err
		}
		files[name] = file
		syntax = append(syntax, file)
	}
	imported := importer.ForCompiler(positions, "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("missing export data for %s", path)
		}
		return os.Open(exports[path])
	})
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	config := types.Config{Importer: imported}
	if _, err := config.Check(p.ImportPath, positions, syntax, info); err != nil {
		return packageSyntax{}, err
	}
	return packageSyntax{positions, files, info}, nil
}

func writePackageHelper(dir string, pkg listedPackage) error {
	path := filepath.Join(dir, "zz_reb_measure.go")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(pkg.ImportPath))
	_, writeErr := fmt.Fprintf(file, helper, pkg.Name, fmt.Sprintf("%x", digest[:8]))
	return errors.Join(writeErr, file.Close())
}

const helper = `package %s

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"text/template"
)

var __rebMeasureLock sync.Mutex
var __rebMeasureSeen = map[int]bool{}

func __rebMeasure(id int) {
	__rebMeasureLock.Lock()
	defer __rebMeasureLock.Unlock()
	if __rebMeasureSeen[id] {
		return
	}
	directory := os.Getenv("REB_MEASUREMENT_DIRECTORY")
	if directory == "" {
		panic("missing coverage measurement directory")
	}
	file, err := os.OpenFile(filepath.Join(directory, "hits-%s-"+strconv.Itoa(os.Getpid())), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	if _, err := fmt.Fprintln(file, id); err != nil {
		panic(err)
	}
	if err := file.Close(); err != nil {
		panic(err)
	}
	__rebMeasureSeen[id] = true
}

func __rebMeasureBool[T ~bool](yes, no, line int, outcome T) T {
	__rebMeasure(line)
	if outcome {
		__rebMeasure(yes)
	} else {
		__rebMeasure(no)
	}
	return outcome
}

func __rebMeasureInit(id int) struct{} {
	__rebMeasure(id)
	return struct{}{}
}

func __rebMeasureBegin(id int) struct{} {
	__rebMeasure(id)
	return struct{}{}
}

func __rebMeasureValue[T any](_ struct{}, value T) T {
	return value
}

func __rebMeasureTemplate(id int) string {
	__rebMeasure(id)
	return ""
}

func __rebMeasureTemplateValue(yes, no, line int, value any) any {
	outcome, _ := template.IsTrue(value)
	__rebMeasureBool(yes, no, line, outcome)
	return value
}
`

func copyInstrumented(root, copyRoot string, packages map[string]listedPackage, exports map[string]string, omission string) ([]fileReport, error) {
	files := []fileReport{}
	written := map[string]bool{}
	typedPackages := map[string]packageSyntax{}
	nextID := 0
	templates, err := templateFiles(root, &nextID)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		target := filepath.Join(copyRoot, relative)
		if entry.IsDir() {
			if relative != "." && (strings.HasPrefix(entry.Name(), ".") ||
				relative == "compliance/evidence" || relative == "compliance/tools" ||
				relative == "compliance/fixtures" || relative == "prototype" || relative == "bin") {
				return fs.SkipDir
			}
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported non-regular file %s", relative)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(relative, ".go") && !strings.HasSuffix(relative, "_test.go") &&
			!strings.HasPrefix(relative, "compliance/") && !strings.Contains("/"+relative, "/testdata/") && relative != omission {
			p, active := packages[relative]
			file := fileReport{Path: relative, Active: active, Sites: []site{}, UnmeasuredLines: []int{}}
			if active {
				typed, checked := typedPackages[p.ImportPath]
				if !checked {
					typed, err = checkTypes(p, exports)
					if err != nil {
						return err
					}
					typedPackages[p.ImportPath] = typed
				}
				source := sourceFile{
					positions: typed.positions, parsed: typed.files[entry.Name()], typeInfo: typed.info,
					original: content, file: file, nextID: &nextID,
				}
				if err := source.instrument(); err != nil {
					return err
				}
				source.registerTemplates(templates)
				content, err = source.generated()
				if err != nil {
					return err
				}
				file = source.file
				if dir := filepath.Dir(target); !written[dir] {
					if err := writePackageHelper(dir, p); err != nil {
						return err
					}
					written[dir] = true
				}
			}
			files = append(files, file)
		}
		return os.WriteFile(target, content, 0600)
	})
	if err != nil {
		return nil, err
	}
	for relative, plan := range templates {
		if plan.registered {
			if err := os.WriteFile(filepath.Join(copyRoot, relative), plan.rewritten, 0600); err != nil {
				return nil, err
			}
		} else {
			plan.report.Active = false
			plan.report.Sites = []site{}
		}
		files = append(files, plan.report)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func collectHits(directory string, files []fileReport) error {
	index := map[int]*site{}
	for _, file := range files {
		for i := range file.Sites {
			probe := &file.Sites[i]
			index[probe.ID] = probe
		}
	}
	logs, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, log := range logs {
		content, err := os.ReadFile(filepath.Join(directory, log.Name()))
		if err != nil {
			return err
		}
		for _, line := range strings.Fields(string(content)) {
			id, err := strconv.Atoi(line)
			if err != nil || index[id] == nil {
				return fmt.Errorf("unknown measurement ID %q", line)
			}
			index[id].Hit = true
		}
	}
	return nil
}

func measuredObservers(goExe, copyRoot, workspace, logDir, target, planPath string) ([]browserObservation, error) {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return nil, err
	}
	var observers []browserObserver
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observers); err != nil || len(observers) == 0 {
		return nil, fmt.Errorf("invalid browser observer plan: %v", err)
	}
	binary := filepath.Join(workspace, "instrumented-board")
	if target == "windows-amd64" {
		binary += ".exe"
	}
	build := exec.Command(goExe, "build", "-trimpath", "-buildvcs=false", "-o", binary, "./cmd/evidence-board")
	build.Dir = copyRoot
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return nil, fmt.Errorf("build instrumented standalone CLI: %w", err)
	}
	contents, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(contents))
	results := make([]browserObservation, 0, len(observers))
	seen := map[string]bool{}
	for _, observer := range observers {
		if observer.ID == "" || seen[observer.ID] || strings.ContainsAny(observer.ID, "/\\.") {
			return nil, fmt.Errorf("invalid observer id %q", observer.ID)
		}
		seen[observer.ID] = true
		if !filepath.IsAbs(observer.Python) || !filepath.IsAbs(observer.Harness) || !filepath.IsAbs(observer.Driver) ||
			!filepath.IsAbs(observer.Browser) || !filepath.IsAbs(observer.Output) {
			return nil, fmt.Errorf("observer %s needs pinned absolute paths", observer.ID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		command := exec.CommandContext(ctx, observer.Python, "-I", "-B", observer.Harness,
			"--browser-kind", observer.Kind, "--driver", observer.Driver, "--browser", observer.Browser,
			"--application", binary, "--evidence-dir", observer.Output, "--label", observer.ID)
		command.Dir = copyRoot
		command.Env = append(os.Environ(), "REB_MEASUREMENT_DIRECTORY="+logDir)
		for key, value := range observer.Env {
			if key != "LD_LIBRARY_PATH" {
				cancel()
				return nil, fmt.Errorf("unexpected browser observer environment key %q", key)
			}
			command.Env = append(command.Env, key+"="+value)
		}
		command.Stdout, command.Stderr = os.Stderr, os.Stderr
		current := browserObservation{ID: observer.ID, Argv: command.Args,
			Artifact: filepath.Join(observer.Output, observer.ID, "results.json"), ExecutableSHA: digest}
		if err := command.Run(); err != nil {
			current.Exit = -1
			current.Error = err.Error()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				current.Exit = exit.ExitCode()
			}
		}
		cancel()
		results = append(results, current)
	}
	return results, nil
}

func run(args []string) error {
	if len(args) < 5 || len(args) > 7 {
		return errors.New("usage: covercheck GO_EXE APP_ROOT REPORT TARGET TEST_PATTERN [OMIT_FIXTURE_FILE | --observe PLAN]")
	}
	goExe, root, output, target, pattern := args[0], args[1], args[2], args[3], args[4]
	omission := ""
	if len(args) == 6 {
		omission = args[5]
	}
	if len(args) == 7 && args[5] != "--observe" {
		return errors.New("unknown observer option")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	packages, exports, err := listPackages(goExe, root)
	if err != nil {
		return err
	}
	if len(packages) == 0 {
		return errors.New("Go did not list any production sources")
	}
	workspace, err := os.MkdirTemp("", "reb-measure-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	copyRoot := filepath.Join(workspace, "src")
	logDir := filepath.Join(workspace, "hits")
	if err := os.Mkdir(logDir, 0700); err != nil {
		return err
	}
	files, err := copyInstrumented(root, copyRoot, packages, exports, omission)
	if err != nil {
		return err
	}
	command := exec.Command(goExe, "test", "-json", "-count=1", "-run", pattern, "./...")
	command.Dir = copyRoot
	command.Env = append(os.Environ(), "REB_MEASUREMENT_DIRECTORY="+logDir)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	result := report{Version: 1, Target: target, Files: files}
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return err
		}
		result.TestExit = exit.ExitCode()
	}
	if len(args) == 7 && result.TestExit == 0 {
		observed, err := measuredObservers(goExe, copyRoot, workspace, logDir, target, args[6])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		result.Observers = observed
	}
	if err := collectHits(logDir, files); err != nil {
		return err
	}
	resultBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(output, append(resultBytes, '\n'), 0600)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
