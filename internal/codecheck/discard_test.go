// Package codecheck holds checks that run inside the ordinary test suite.
//
// It exists because of a defect class this repository produced repeatedly: a
// call whose error was assigned to the blank identifier, so a failure to
// persist a fired timer or to clear a speakerphone's off-hook report vanished.
//
// CI runs `errcheck -blank -ignoretests` with no exclusions, which is the real
// guarantee for production code: it is type-aware and enumerates nothing. This
// check covers the test tree that CI skips, and it fails fast in the suite
// people actually run. It is name-scoped by design - a syntactic pass cannot
// know a call returns an error - so it is a fast regression guard, not a
// complete one, and the errcheck run is what makes completeness a property of
// production rather than of a list.
package codecheck

import (
	"fmt"
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

// scan walks a tree and reports unjustified discards. It is a function rather
// than inline test code so the guard's own failure modes can be exercised
// against fixtures: a check that goes quiet on a file it cannot read, or that
// examines nothing at all, would pass over any code in the repository.
func scan(root string) (files int, offences []string, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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
			// A file the check cannot read is exactly how a guard goes quiet,
			// so this is a failure and never a skip.
			return parseErr
		}
		files++
		for _, o := range discards(fset, file, justifiedLines(fset, file)) {
			rel, relErr := filepath.Rel(root, o.file)
			if relErr != nil {
				rel = o.file
			}
			offences = append(offences, rel+":"+strconv.Itoa(o.line))
		}
		return nil
	})
	if err != nil {
		return files, offences, err
	}
	if files == 0 {
		return 0, nil, fmt.Errorf("parsed no Go files under %s: the check would pass regardless of the code", root)
	}
	return files, offences, nil
}

// TestNoUnjustifiedDiscardedResults fails on any discarded call result in the
// module that does not say why throwing it away is safe.
func TestNoUnjustifiedDiscardedResults(t *testing.T) {
	root := moduleRoot(t)
	files, offences, err := scan(root)
	if err != nil {
		t.Fatalf("scanning the module: %v", err)
	}
	if len(offences) > 0 {
		t.Errorf("%d call result(s) discarded with no justification comment:\n  %s\n\n"+
			"Report the failure through a faults.Reporter, return it, or write "+
			"`// %s <why it cannot matter>` on the line.",
			len(offences), strings.Join(offences, "\n  "), justification)
	}
	t.Logf("checked %d Go files under %s", files, root)
}

// The guard has been wrong three times about what it can see, so its own
// failure modes are assertions rather than something to be trusted: a file it
// cannot parse must fail, a tree with nothing in it must fail, and an
// unjustified discard must be reported.
func TestScanFailsRatherThanGoingQuiet(t *testing.T) {
	t.Run("parse error fails", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package p\nfunc oops( {\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := scan(dir)
		if err == nil {
			t.Fatal("an unparseable file was skipped instead of failing the check")
		}
		if !strings.Contains(err.Error(), "broken.go") {
			t.Errorf("error does not name the file it could not read: %v", err)
		}
	})
	t.Run("empty tree fails", func(t *testing.T) {
		if _, _, err := scan(t.TempDir()); err == nil {
			t.Fatal("a tree with no Go files passed, so the check proves nothing")
		}
	})
	t.Run("unjustified discard is reported", func(t *testing.T) {
		dir := t.TempDir()
		src := "package p\n\nfunc f() error { return nil }\n\nfunc g() { _ = f() }\n"
		if err := os.WriteFile(filepath.Join(dir, "sloppy.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		_, offences, err := scan(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(offences) != 1 {
			t.Fatalf("offences = %v, want exactly one", offences)
		}
	})
	t.Run("justified discard is accepted", func(t *testing.T) {
		dir := t.TempDir()
		src := "package p\n\nfunc f() error { return nil }\n\nfunc g() {\n\t// discard: cannot fail\n\t_ = f()\n}\n"
		if err := os.WriteFile(filepath.Join(dir, "fine.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		_, offences, err := scan(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(offences) != 0 {
			t.Fatalf("offences = %v, want none", offences)
		}
	})
}

type offence struct {
	file string
	line int
}

// mustHandle names calls whose result is an error worth handling even when
// the call is written as a bare statement. A syntactic check cannot know a
// call's return type without type information, and type-checking the module's
// dependency tree would cost more than this guard is worth, so the list is
// explicit: these are the operations whose silent failure has actually cost
// something in this repository.
var mustHandle = map[string]bool{
	"Close": true, "Save": true, "Sync": true, "Flush": true,
	"Write": true, "SetDeadline": true, "Remove": true, "Chmod": true,
	"Rename": true, "Speak": true, "Exec": true,
}

// discards finds three shapes: an assignment whose left side is entirely
// blank, a short declaration that drops a value into a blank, and a bare call
// statement to something whose error matters.
func discards(fset *token.FileSet, file *ast.File, justified map[int]bool) []offence {
	var found []offence
	report := func(pos token.Position) {
		if !justified[pos.Line] {
			found = append(found, offence{file: pos.Filename, line: pos.Line})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			if !callsSomething(stmt.Rhs) {
				return true
			}
			blanks, named := 0, 0
			for _, lhs := range stmt.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name == "_" {
					blanks++
					continue
				}
				named++
			}
			switch {
			case blanks == 0:
				return true
			case stmt.Tok == token.ASSIGN && named == 0:
				// `_ = f()` and `_, _ = f()`: every result thrown away.
			case blanks > 0 && trailingBlank(stmt.Lhs) && callsMustHandle(stmt.Rhs):
				// `v, _ := save()` drops the error just as quietly. Only the
				// trailing position is checked, since that is where Go puts
				// an error, and only for the named calls - without type
				// information a blank could be discarding a bool.
			default:
				return true
			}
			report(fset.Position(stmt.Pos()))
		case *ast.ExprStmt:
			// A bare call: nothing is assigned, so any result is dropped.
			if namedCall(stmt.X) {
				report(fset.Position(stmt.Pos()))
			}
		case *ast.DeferStmt:
			// `defer f.Close()` drops the error as completely as a bare call
			// does, and on a write path that is how data goes missing.
			if namedCall(stmt.Call) {
				report(fset.Position(stmt.Pos()))
			}
		case *ast.GoStmt:
			if namedCall(stmt.Call) {
				report(fset.Position(stmt.Pos()))
			}
		}
		return true
	})
	return found
}

// namedCall reports whether an expression is a call to one of the operations
// whose failure matters.
func namedCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && mustHandle[sel.Sel.Name]
}

// trailingBlank reports whether the last assigned name is blank, which is
// where a Go call puts its error.
func trailingBlank(lhs []ast.Expr) bool {
	if len(lhs) == 0 {
		return false
	}
	ident, ok := lhs[len(lhs)-1].(*ast.Ident)
	return ok && ident.Name == "_"
}

// callsMustHandle reports whether any call in these expressions is one of the
// named operations whose failure matters.
func callsMustHandle(exprs []ast.Expr) bool {
	for _, e := range exprs {
		named := false
		ast.Inspect(e, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && mustHandle[sel.Sel.Name] {
				named = true
				return false
			}
			return true
		})
		if named {
			return true
		}
	}
	return false
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

// The guard must catch the shapes a reviewer found it blind to, so those
// shapes are exercised against the checker itself rather than trusted to a
// reading of it.
func TestCheckerCatchesEveryDiscardShape(t *testing.T) {
	for name, src := range map[string]string{
		"blank assign":          "package p\nfunc f() error { return nil }\nfunc g() { _ = f() }\n",
		"two blanks":            "package p\nfunc f() (int, error) { return 0, nil }\nfunc g() { _, _ = f() }\n",
		"bare call":             "package p\nimport \"os\"\nfunc g(f *os.File) { f.Close() }\n",
		"trailing blank define": "package p\nimport \"os\"\nfunc g(f *os.File) { x, _ := f.Write(nil); _ = x }\n",
	} {
		if got := offencesIn(t, name+".go", src); got != 1 {
			t.Errorf("%s: checker found %d offences, want 1", name, got)
		}
	}
	for name, src := range map[string]string{
		"justified":      "package p\nfunc f() error { return nil }\nfunc g() {\n\t// discard: nothing can fail\n\t_ = f()\n}\n",
		"handled":        "package p\nfunc f() error { return nil }\nfunc g() error { return f() }\n",
		"void bare call": "package p\nimport \"fmt\"\nfunc g() { fmt.Print(\"x\") }\n",
		"bool discarded": "package p\nfunc f() (int, bool) { return 0, true }\nfunc g() { v, _ := f(); _ = v }\n",
	} {
		if got := offencesIn(t, name+".go", src); got != 0 {
			t.Errorf("%s: checker found %d offences, want 0", name, got)
		}
	}
}

func offencesIn(t *testing.T, name, src string) int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return len(discards(fset, file, justifiedLines(fset, file)))
}

// defer and go statements hide a dropped error exactly as well as a bare
// call, which is the blind spot that let four Close discards live in this
// repository while the checker reported clean.
func TestCheckerCatchesDeferredAndConcurrentDiscards(t *testing.T) {
	for name, src := range map[string]string{
		"deferred close":     "package p\nimport \"os\"\nfunc g(f *os.File) { defer f.Close() }\n",
		"go close":           "package p\nimport \"os\"\nfunc g(f *os.File) { go f.Close() }\n",
		"deferred justified": "package p\nimport \"os\"\nfunc g(f *os.File) {\n\t// discard: read-only handle\n\tdefer f.Close()\n}\n",
	} {
		want := 1
		if name == "deferred justified" {
			want = 0
		}
		if got := offencesIn(t, name+".go", src); got != want {
			t.Errorf("%s: checker found %d offences, want %d", name, got, want)
		}
	}
}
