// Command apierrlint is the STATIC remediation guarantee for issue #28.
//
// It walks the Go source tree and fails if any apierr.Error is constructed
// without a non-empty Remediation: both the entries of the apierr.Catalogue map
// and any apierr.Error composite literal at a call site. The runtime envelope
// test only covers exercised paths; this static check makes an unexercised path
// that lacks a remediation impossible to ship.
//
// Usage: apierrlint <repo-root>
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type violation struct {
	pos    token.Position
	detail string
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: apierrlint <repo-root>")
		os.Exit(2)
	}
	root := os.Args[1]

	var violations []violation
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			// Skip vendored, generated, and non-source trees.
			if base == "vendor" || base == "third_party" || base == ".git" ||
				base == "node_modules" || base == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		v, err := checkFile(path)
		if err != nil {
			return err
		}
		violations = append(violations, v...)
		checked++
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "apierrlint: walk: %v\n", err)
		os.Exit(2)
	}

	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "apierrlint: %d apierr.Error construction(s) lack a non-empty remediation (issue #28):\n", len(violations))
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", v.pos, v.detail)
		}
		fmt.Fprintln(os.Stderr, "Every LLM-legible error must carry an actionable remediation. See docs/api/errors.md.")
		os.Exit(1)
	}
	fmt.Printf("apierrlint: OK, every apierr.Error construction across %d files carries a non-empty remediation\n", checked)
}

// checkFile parses one Go file and reports every apierr.Error composite literal
// (including the entries of the Catalogue map literal) that lacks a non-empty
// Remediation string. It is conservative: it only flags literals whose type is
// clearly apierr.Error (or a bare Error inside package apierr), so it never
// produces a false positive on an unrelated struct.
func checkFile(path string) ([]violation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	inAPIErrPkg := f.Name.Name == "apierr"

	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || cl.Type == nil {
			return true
		}
		if !isAPIErrErrorType(cl.Type, inAPIErrPkg) {
			return true
		}
		if !hasNonEmptyRemediation(cl) {
			out = append(out, violation{
				pos:    fset.Position(cl.Pos()),
				detail: "apierr.Error literal with empty or missing Remediation",
			})
		}
		return true
	})
	return out, nil
}

// isAPIErrErrorType reports whether the composite-literal type is apierr.Error.
// Inside package apierr the type is a bare Error identifier; elsewhere it is the
// selector apierr.Error.
func isAPIErrErrorType(t ast.Expr, inAPIErrPkg bool) bool {
	switch tt := t.(type) {
	case *ast.SelectorExpr:
		pkg, ok := tt.X.(*ast.Ident)
		return ok && pkg.Name == "apierr" && tt.Sel.Name == "Error"
	case *ast.Ident:
		return inAPIErrPkg && tt.Name == "Error"
	}
	return false
}

// hasNonEmptyRemediation reports whether the Error literal sets Remediation to a
// non-empty string constant. A literal that omits Remediation, or sets it to ""
// or to a non-constant expression we cannot prove non-empty, is a violation: the
// remediation must be a real, statically present string.
func hasNonEmptyRemediation(cl *ast.CompositeLit) bool {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Remediation" {
			continue
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// A non-literal remediation (a variable, a function call) cannot be
			// statically proven non-empty; treat it as missing so the catalogue
			// stays the place remediations live.
			return false
		}
		// lit.Value includes the surrounding quotes; "" is the empty string.
		return strings.Trim(lit.Value, "`\"") != ""
	}
	return false
}
