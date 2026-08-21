package httpapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestExtractorSingleCallSite fails if anything other than ingestLead calls the
// extractor.
//
// The email ingestion path puts a filter pipeline in front of the model: mail
// that looks like a newsletter, an auto-reply or a bounce is quarantined
// before it costs anything, and a per-user cap bounds the rest. That is a cost
// control, and a cost control with a second door is not a control at all — any
// new handler that called s.extractor directly would be a way for unfiltered
// mail to reach a billable API.
//
// So the property is structural: exactly one function in this package extracts.
// If you are here because this test failed, route your new path through
// ingestLead rather than relaxing the assertion.
func TestExtractorSingleCallSite(t *testing.T) {
	const allowed = "ingestLead"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	found := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			// Track the enclosing function so a violation names it.
			var fn string
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.FuncDecl:
					fn = node.Name.Name
				case *ast.CallExpr:
					sel, ok := node.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Extract" {
						return true
					}
					// Match s.extractor.Extract(...) specifically, not any
					// method that happens to be named Extract.
					inner, ok := sel.X.(*ast.SelectorExpr)
					if !ok || inner.Sel.Name != "extractor" {
						return true
					}
					found++
					if fn != allowed {
						t.Errorf("%s: %s calls s.extractor.Extract directly; every extraction must go through %s so the ingestion filters cannot be bypassed",
							fset.Position(node.Pos()), fn, allowed)
					}
				}
				return true
			})
			_ = path
		}
	}

	if found == 0 {
		t.Fatalf("found no call to s.extractor.Extract at all — this test has stopped testing anything (was the field renamed?)")
	}
}
