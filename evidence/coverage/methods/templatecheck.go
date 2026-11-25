package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	tmplparse "text/template/parse"
)

type templatePlan struct {
	rewritten  []byte
	report     fileReport
	registered bool
}

func templateFiles(root string, nextID *int) (map[string]*templatePlan, error) {
	plans := map[string]*templatePlan{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() && rel != "." && (strings.HasPrefix(entry.Name(), ".") || rel == "compliance" || rel == "docs" || rel == "prototype" || rel == "bin" || entry.Name() == "testdata") {
			return fs.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(rel, ".html") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(content, []byte("{{")) {
			return nil
		}
		plan, err := instrumentTemplate(rel, content, nextID)
		if err != nil {
			return err
		}
		plans[rel] = plan
		return nil
	})
	return plans, err
}

func instrumentTemplate(path string, content []byte, nextID *int) (*templatePlan, error) {
	if bytes.Contains(content, []byte(helperPrefix)) {
		return nil, fmt.Errorf("reserved template probe identifier found in %s", path)
	}
	tree := tmplparse.New(path)
	tree.Mode = tmplparse.SkipFuncCheck
	definitions := map[string]*tmplparse.Tree{}
	if _, err := tree.Parse(string(content), "", "", definitions); err != nil {
		return nil, err
	}
	source := &sourceFile{positions: token.NewFileSet(), nextID: nextID, file: fileReport{
		Path: path, Active: true, Sites: []site{}, UnmeasuredLines: []int{},
	}}
	original := source.positions.AddFile(path, -1, len(content))
	original.SetLinesForContent(content)
	instrumenter := templateInstrumenter{source: source, original: original, expressionLines: map[int]bool{}}
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		instrumenter.list(definitions[name].Root)
	}
	if instrumenter.err != nil {
		return nil, instrumenter.err
	}
	var rewritten strings.Builder
	rewritten.WriteString(tree.Root.String())
	for _, name := range names {
		if name != path {
			fmt.Fprintf(&rewritten, "{{define %s}}%s{{end}}", strconv.Quote(name), definitions[name].Root.String())
		}
	}
	for _, site := range source.file.Sites {
		if site.Kind == "line" {
			delete(instrumenter.expressionLines, site.Line)
		}
	}
	for line := range instrumenter.expressionLines {
		source.file.UnmeasuredLines = append(source.file.UnmeasuredLines, line)
	}
	sort.Ints(source.file.UnmeasuredLines)
	return &templatePlan{rewritten: []byte(rewritten.String()), report: source.file}, nil
}

type templateInstrumenter struct {
	source          *sourceFile
	original        *token.File
	err             error
	expressionLines map[int]bool
	rangeStack      []string
}

func (t *templateInstrumenter) note(position tmplparse.Pos, kind string) string {
	return t.source.note(t.original.Pos(int(position)), kind)
}

func (t *templateInstrumenter) parse(expression string) *tmplparse.ActionNode {
	parsed := tmplparse.New("probe")
	parsed.Mode = tmplparse.SkipFuncCheck
	if _, err := parsed.Parse(expression, "", "", map[string]*tmplparse.Tree{}); err != nil {
		t.err = err
		return &tmplparse.ActionNode{}
	}
	return parsed.Root.Nodes[0].(*tmplparse.ActionNode)
}

func (t *templateInstrumenter) action(position tmplparse.Pos, kind string) tmplparse.Node {
	return t.parse("{{$" + helperPrefix + "Discard := " + helperPrefix + "Template " + t.note(position, kind) + "}}")
}

func (t *templateInstrumenter) list(list *tmplparse.ListNode) {
	if list == nil || t.err != nil {
		return
	}
	var nodes []tmplparse.Node
	for _, node := range list.Nodes {
		var after []tmplparse.Node
		switch control := node.(type) {
		case *tmplparse.IfNode:
			nodes = append(nodes, t.action(node.Position(), "line"))
			t.branch(&control.BranchNode, "template-if")
		case *tmplparse.WithNode:
			nodes = append(nodes, t.action(node.Position(), "line"))
			t.branch(&control.BranchNode, "template-with")
		case *tmplparse.RangeNode:
			nodes = append(nodes, t.action(node.Position(), "line"))
			if control.ElseList != nil {
				// Explicit else has real empty/nonempty alternatives. Its break
				// belongs to this range, never to an outer implicit-else range.
				t.rangeStack = append(t.rangeStack, "")
				t.branch(&control.BranchNode, "template-range")
				t.rangeStack = t.rangeStack[:len(t.rangeStack)-1]
			} else {
				marker := helperPrefix + "Range" + strconv.Itoa(*t.source.nextID)
				nodes = append(nodes, t.parse("{{$"+marker+":=true}}"))
				t.rangeStack = append(t.rangeStack, marker)
				t.pipeline(control.Pipe)
				t.list(control.List)
				t.rangeStack = t.rangeStack[:len(t.rangeStack)-1]
				control.List.Nodes = append([]tmplparse.Node{t.action(control.Pos, "template-range:iteration")}, control.List.Nodes...)
				after = append(after, t.rangeExhaustion(control.Pos, marker))
			}
		case *tmplparse.ActionNode:
			nodes = append(nodes, t.action(node.Position(), "line"))
			t.pipeline(control.Pipe)
		case *tmplparse.TemplateNode:
			nodes = append(nodes, t.action(node.Position(), "line"))
			t.pipeline(control.Pipe)
		case *tmplparse.BreakNode:
			nodes = append(nodes, t.action(node.Position(), "line"))
			if len(t.rangeStack) != 0 && t.rangeStack[len(t.rangeStack)-1] != "" {
				nodes = append(nodes, t.rangeAssignment(t.rangeStack[len(t.rangeStack)-1]))
			}
		case *tmplparse.ContinueNode:
			nodes = append(nodes, t.action(node.Position(), "line"))
		case *tmplparse.TextNode, *tmplparse.CommentNode:
		default:
			t.err = fmt.Errorf("unsupported template node %T in %s", node, t.source.file.Path)
		}
		nodes = append(nodes, node)
		nodes = append(nodes, after...)
	}
	list.Nodes = nodes
}

// Parse with a declaration so the parser can validate the variable reference.
// Only the assignment/if node is retained; it refers to the real range-local
// declaration inserted in the same template scope.
func (t *templateInstrumenter) rangeFragment(marker, suffix string) []tmplparse.Node {
	parsed := tmplparse.New("range-probe")
	parsed.Mode = tmplparse.SkipFuncCheck
	if _, err := parsed.Parse("{{$"+marker+":=true}}"+suffix, "", "", map[string]*tmplparse.Tree{}); err != nil {
		t.err = err
		return nil
	}
	return parsed.Root.Nodes[1:]
}

func (t *templateInstrumenter) rangeAssignment(marker string) tmplparse.Node {
	nodes := t.rangeFragment(marker, "{{$"+marker+" = false}}")
	if len(nodes) == 0 {
		return &tmplparse.ActionNode{}
	}
	return nodes[0]
}

func (t *templateInstrumenter) rangeExhaustion(position tmplparse.Pos, marker string) tmplparse.Node {
	nodes := t.rangeFragment(marker, "{{if $"+marker+"}}{{end}}")
	if len(nodes) == 0 {
		return &tmplparse.ActionNode{}
	}
	condition := nodes[0].(*tmplparse.IfNode)
	condition.List.Nodes = []tmplparse.Node{t.action(position, "template-range:exhausted")}
	return condition
}

func (t *templateInstrumenter) branch(branch *tmplparse.BranchNode, kind string) {
	t.pipeline(branch.Pipe)
	t.list(branch.List)
	t.list(branch.ElseList)
	branch.List.Nodes = append([]tmplparse.Node{t.action(branch.Pos, kind+":true")}, branch.List.Nodes...)
	if branch.ElseList == nil {
		branch.ElseList = &tmplparse.ListNode{NodeType: tmplparse.NodeList}
	}
	branch.ElseList.Nodes = append([]tmplparse.Node{t.action(branch.Pos, kind+":false")}, branch.ElseList.Nodes...)
}

func (t *templateInstrumenter) pipeline(pipeline *tmplparse.PipeNode) {
	if pipeline == nil || t.err != nil {
		return
	}
	for commandIndex, command := range pipeline.Cmds {
		for _, argument := range command.Args {
			switch nested := argument.(type) {
			case *tmplparse.PipeNode:
				t.pipeline(nested)
			case *tmplparse.FieldNode, *tmplparse.ChainNode, *tmplparse.VariableNode, *tmplparse.IdentifierNode:
				t.expressionLines[t.original.Line(t.original.Pos(int(argument.Position())))] = true
			}
		}
		first, named := command.Args[0].(*tmplparse.IdentifierNode)
		if !named || (first.Ident != "and" && first.Ident != "or") {
			continue
		}
		if commandIndex != 0 {
			t.err = fmt.Errorf("template short-circuit with implicit piped operand at %s:%d requires its own adapter",
				t.source.file.Path, t.original.Line(t.original.Pos(int(command.Position()))))
			return
		}
		for index := 1; index < len(command.Args); index++ {
			original := command.Args[index]
			yes := t.note(original.Position(), "template-"+first.Ident+":true")
			no := t.note(original.Position(), "template-"+first.Ident+":false")
			line := t.note(original.Position(), "line")
			parsed := t.parse("{{print (" + helperPrefix + "TemplateValue " + yes + " " + no + " " + line + " .)}}")
			if t.err != nil {
				return
			}
			nested := parsed.Pipe.Cmds[0].Args[1].(*tmplparse.PipeNode)
			nested.Cmds[0].Args[4] = original
			command.Args[index] = nested
		}
	}
}

func (f *sourceFile) registerTemplates(plans map[string]*templatePlan) {
	embedded := map[types.Object]*templatePlan{}
	for _, declaration := range f.parsed.Decls {
		variable, ok := declaration.(*ast.GenDecl)
		if !ok || variable.Tok != token.VAR || variable.Doc == nil || len(variable.Specs) != 1 {
			continue
		}
		value := variable.Specs[0].(*ast.ValueSpec)
		for _, comment := range variable.Doc.List {
			fields := strings.Fields(comment.Text)
			if len(fields) == 2 && fields[0] == "//go:embed" && len(value.Names) == 1 {
				path := filepath.ToSlash(filepath.Join(filepath.Dir(f.file.Path), fields[1]))
				if plan := plans[path]; plan != nil {
					embedded[f.typeInfo.Defs[value.Names[0]]] = plan
				}
			}
		}
	}
	ast.Inspect(f.parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		selected, selection := call.Fun.(*ast.SelectorExpr)
		argument, identifier := call.Args[0].(*ast.Ident)
		if !selection || selected.Sel.Name != "Parse" || !identifier || embedded[f.typeInfo.Uses[argument]] == nil {
			return true
		}
		result, tuple := f.typeInfo.Types[call].Type.(*types.Tuple)
		if !tuple || result.Len() != 2 || result.At(0).Type().String() != "*html/template.Template" {
			return true
		}
		f.add(selected.X.End(), 80, ".Funcs(map[string]any{\""+helperPrefix+"Template\":"+helperPrefix+"Template,\""+helperPrefix+"TemplateValue\":"+helperPrefix+"TemplateValue})")
		embedded[f.typeInfo.Uses[argument]].registered = true
		return true
	})
}
