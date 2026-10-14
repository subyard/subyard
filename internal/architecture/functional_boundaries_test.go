package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// These boundaries share legacy packages with physical adapters. Follow local
// calls and declared internal dependencies so a helper cannot silently reintroduce
// a filesystem/process dependency into their semantic call graph. Selector calls
// are conservatively followed by name across this bounded package set, including
// methods whose concrete receiver cannot be resolved by the syntax tree.
func TestFunctionalBoundaryCallGraphsDoNotReachPhysicalAdapters(t *testing.T) {
	type function struct {
		body    *ast.BlockStmt
		imports map[string]string
		path    string
		method  bool
	}
	functions := map[string][]function{}
	packages := []string{"profile", "config", "resource", "domain", "adapters/reconcileruntime", "releasetransition"}
	for _, packageName := range packages {
		directory := filepath.Join("..", packageName)
		files, err := filepath.Glob(filepath.Join(directory, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			aliases := map[string]string{}
			for _, item := range file.Imports {
				imported, err := strconv.Unquote(item.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				alias := filepath.Base(imported)
				if item.Name != nil {
					alias = item.Name.Name
				}
				aliases[alias] = imported
			}
			for _, declaration := range file.Decls {
				if declaration, ok := declaration.(*ast.FuncDecl); ok {
					functions[declaration.Name.Name] = append(functions[declaration.Name.Name], function{declaration.Body, aliases, path, declaration.Recv != nil})
				}
			}
		}
	}
	roots := []string{"Decode", "DecodeIntegration", "ValidateMetadata", "BuildCatalog", "EvaluateAssignments", "EvaluateLayer", "ParseResource", "BuildRegistry", "checkObservations", "decodeProfileRuntimeObservation", "SelectMaterializationSources", "PrepareMaterializationContent", "AssessMaterializedArtifact", "AssessMaterialization", "AssessMaterializationActivation", "planSettingsV2Yard", "classifySettingsV2Inheritance", "selectSettingsV2Yard", "settingsV2AwaitSourceImport"}
	for _, root := range roots {
		t.Run(root, func(t *testing.T) {
			visited := map[string]bool{}
			chain := []string{}
			var visit func(string, bool)
			visit = func(name string, methods bool) {
				key := name
				if methods {
					key += ":method"
				}
				if visited[key] {
					return
				}
				visited[key] = true
				chain = append(chain, name)
				defer func() { chain = chain[:len(chain)-1] }()
				for _, function := range functions[name] {
					if function.method && !methods {
						continue
					}
					ast.Inspect(function.body, func(node ast.Node) bool {
						call, ok := node.(*ast.CallExpr)
						if !ok {
							return true
						}
						switch target := call.Fun.(type) {
						case *ast.Ident:
							visit(target.Name, false)
						case *ast.SelectorExpr:
							if qualifier, ok := target.X.(*ast.Ident); ok {
								if imported, exists := function.imports[qualifier.Name]; exists {
									if imported == "os" || imported == "os/exec" || imported == "io/fs" || imported == "syscall" || imported == "golang.org/x/sys/unix" || imported == "net" || imported == "net/http" || strings.Contains(imported, "/internal/adapters/") || strings.Contains(imported, "lxc/incus") || imported == "path/filepath" && (target.Sel.Name == "EvalSymlinks" || target.Sel.Name == "Walk" || target.Sel.Name == "WalkDir" || target.Sel.Name == "Glob") {
										t.Errorf("%s reaches physical call %s.%s in %s", strings.Join(chain, " -> "), imported, target.Sel.Name, function.path)
									}
									if !strings.Contains(imported, "/internal/") {
										return true
									}
									if !slices.Contains(packages, strings.TrimPrefix(imported, "github.com/Subyard/Subyard/internal/")) {
										t.Errorf("%s reaches an uninspected internal dependency %s in %s", root, imported, function.path)
									}

								}
							}
							visit(target.Sel.Name, true)
						}
						return true
					})
				}
			}
			if len(functions[root]) == 0 {
				t.Fatal("functional boundary is missing")
			}
			visit(root, true)
		})
	}
}

// Policy extraction must remain on the production paths, including prospective
// ingress and journal rechecks; pure helpers alone do not establish that contract.
func TestSettingsMigrationProductionUsesFunctionalPolicy(t *testing.T) {
	contracts := map[string]map[string][]string{
		"settings_test_vms_v2.go": {
			"inspectYard":              {"observeSettingsV2Yard", "planSettingsV2Yard"},
			"observeInheritedSettings": {"classifySettingsV2Inheritance"},
		},
		"transition_v2.go": {
			"observeYard":               {"observeSettingsV2Yard", "selectSettingsV2Yard"},
			"observeSettingsWork":       {"observeYard", "observeInheritedSettings"},
			"settingsAwaitSourceImport": {"observeSettingsV2Yard", "settingsV2AwaitSourceImport"},
		},
	}
	for filename, expected := range contracts {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "releasetransition", filename), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			calls, exists := expected[function.Name.Name]
			if !exists {
				continue
			}
			seen := map[string]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					switch target := call.Fun.(type) {
					case *ast.Ident:
						seen[target.Name] = true
					case *ast.SelectorExpr:
						seen[target.Sel.Name] = true
					}
				}
				return true
			})
			for _, call := range calls {
				if !seen[call] {
					t.Errorf("%s.%s no longer calls %s", filename, function.Name.Name, call)
				}
			}
			delete(expected, function.Name.Name)
		}
		for function := range expected {
			t.Errorf("%s missing production function %s", filename, function)
		}
	}
}
