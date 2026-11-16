package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshLedgerConstructorHasOnlyBootstrapCallers(t *testing.T) {
	root := filepath.Join("..")
	callers := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch function := call.Fun.(type) {
			case *ast.Ident:
				name = function.Name
			case *ast.SelectorExpr:
				name = function.Sel.Name
			}
			if name == "BaselineLedgerV2" {
				t.Errorf("ambiguous missing-ledger constructor returned in %s", path)
			}
			if name == "FreshBootstrapLedgerV2" {
				callers++
				if filepath.Clean(path) != filepath.Join(root, "releasetransition", "bootstrap_ledger.go") {
					t.Errorf("fresh baseline outside bootstrap boundary: %s", path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if callers == 0 {
		t.Fatal("bootstrap constructor was not inspected")
	}
}
