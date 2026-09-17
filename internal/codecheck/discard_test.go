// Package codecheck holds checks that run inside the ordinary test suite.
//
// It exists because of a defect class this repository produced twice: a call
// whose error was assigned to the blank identifier, so a failure to persist a
// fired timer or to clear a speakerphone's off-hook report vanished. Requiring
// a faults.Reporter in the constructors of the types that own background work
// makes the right thing available, but Go cannot forbid writing `_ = f()`, so
// the guard lives here instead of in a lint job someone can skip.
package codecheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// justification marks a blank discard as deliberate:
//
//	_ = binary.Write(&b, binary.LittleEndian, x) // discard: writes to a
//	bytes.Buffer cannot fail
const justification = "discard:"

// TestNoUnjustifiedDiscardedResults fails on any `_ = call()` in the module
// that does not say, on the line or the line above, why throwing the result
// away is safe.
func TestNoUnjustifiedDiscardedResults(t *testing.T) {
	root := moduleRoot(t)
	var offences []string
	files := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			// A file the check cannot read is exactly how a guard goes quiet.
			return parseErr
		}
		files++
		justified := justifiedLines(fset, file)
		for _, o := range discards(fset, file, justified) {
			rel, relErr := filepath.Rel(root, o.file)
			if relErr != nil {
				rel = o.file
			}
			offences = append(offences, rel+":"+strconv.Itoa(o.line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if files == 0 {
		t.Fatal("parsed no Go files: the check would pass regardless of the code")
	}
	if len(offences) > 0 {
		t.Errorf("%d call result(s) discarded with no justification comment:\n  %s\n\n"+
			"Report the failure through a faults.Reporter, return it, or write "+
			"`// %s <why it cannot matter>` on the line.",
			len(offences), strings.Join(offences, "\n  "), justification)
	}
	t.Logf("checked %d Go files under %s", files, root)
}

type offence struct {
	file string
	line int
}

// discards finds assignments whose left-hand side is entirely blank and whose
// right-hand side calls something.
func discards(fset *token.FileSet, file *ast.File, justified map[int]bool) []offence {
	var found []offence
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.ASSIGN {
			return true
		}
		for _, lhs := range assign.Lhs {
			if ident, ok := lhs.(*ast.Ident); !ok || ident.Name != "_" {
				return true
			}
		}
		if !callsSomething(assign.Rhs) {
			return true
		}
		pos := fset.Position(assign.Pos())
		if justified[pos.Line] {
			return true
		}
		found = append(found, offence{file: pos.Filename, line: pos.Line})
		return true
	})
	return found
}

func callsSomething(exprs []ast.Expr) bool {
	for _, e := range exprs {
		calls := false
		ast.Inspect(e, func(n ast.Node) bool {
			if _, ok := n.(*ast.CallExpr); ok {
				calls = true
				return false
			}
			return true
		})
		if calls {
			return true
		}
	}
	return false
}

// justifiedLines collects the lines a justification covers: every line of the
// comment group carrying the marker, and the line after it, so a multi-line
// justification written above a statement covers that statement.
func justifiedLines(fset *token.FileSet, file *ast.File) map[int]bool {
	lines := map[int]bool{}
	for _, group := range file.Comments {
		marked := false
		for _, c := range group.List {
			if strings.Contains(c.Text, justification) {
				marked = true
				break
			}
		}
		if !marked {
			continue
		}
		first := fset.Position(group.Pos()).Line
		last := fset.Position(group.End()).Line
		for l := first; l <= last+1; l++ {
			lines[l] = true
		}
	}
	return lines
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test's working directory")
		}
		dir = parent
	}
}
