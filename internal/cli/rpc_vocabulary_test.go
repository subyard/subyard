package cli

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/Subyard/Subyard/internal/rpc"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func init() {
	rpc.StrictErrorCodes = true
}

func rpcSchemaCompiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	paths, err := filepath.Glob(filepath.Join(repositoryRoot(t), "api/yard-rpc/v1/*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		payload, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(payload)
		_ = payload.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(path, document); err != nil {
			t.Fatal(err)
		}
	}
	return compiler
}

func TestRPCManifestMethodsAndDispatch(t *testing.T) {
	root := filepath.Join(repositoryRoot(t), "api/yard-rpc/v1")
	compiler := rpcSchemaCompiler(t)
	schema, err := compiler.Compile(filepath.Join(root, "vocabulary.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.Open(filepath.Join(root, "vocabulary.json"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(data)
	_ = data.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatal(err)
	}
	methods := rpc.MethodCapabilities()
	schemaData, err := os.ReadFile(filepath.Join(root, "methods.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var methodSchema struct {
		OneOf []struct {
			Properties struct {
				Method struct {
					Const string
					Enum  []string
				}
			}
		}
	}
	if err := json.Unmarshal(schemaData, &methodSchema); err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, branch := range methodSchema.OneOf {
		if branch.Properties.Method.Const != "" {
			actual = append(actual, branch.Properties.Method.Const)
		}
		actual = append(actual, branch.Properties.Method.Enum...)
	}
	var want []string
	for method := range methods {
		want = append(want, method)
	}
	slices.Sort(actual)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		t.Fatalf("schema methods %v; manifest %v", actual, want)
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Only parse the method table, not incidental mentions in prose.
	rows := regexp.MustCompile("(?m)^\\| `([^|]+) \\|").FindAllSubmatch(readme, -1)
	actual = nil
	for _, row := range rows {
		for _, name := range regexp.MustCompile("[a-z]+(?:[.-][a-z]+)+").FindAllString(string(row[1]), -1) {
			actual = append(actual, name)
		}
	}
	slices.Sort(actual)
	if !slices.Equal(actual, want) {
		t.Fatalf("documented methods %v; manifest %v", actual, want)
	}
	for _, method := range append(want, "unknown.method") {
		if method == rpc.MethodRpcNegotiate {
			continue
		} // Session owns the handshake.
		t.Run(method, func(t *testing.T) {
			handler, _, _ := ownerQueryFixture(t)
			defer handler.closePlans()
			// Invalid params stop mutating/streaming methods before side effects. Queries
			// with no params still execute against their ordinary native fixture.
			_, err := handler.Handle(context.Background(), rpc.Call{ID: "dispatch", OperationID: "dispatch", Method: method, Params: json.RawMessage(`{"unknown":true}`)}, func(string, any) (uint64, error) { return 1, nil })
			var fault *rpc.Error
			missing := errors.As(err, &fault) && fault.Code == rpc.CodeMethodNotFound
			if (method == "unknown.method") != missing {
				t.Fatalf("dispatch %s: %v", method, err)
			}
		})
	}
}

// This explicit source contract closes the reverse direction of the runtime
// dispatch test: a new case cannot silently bypass the vocabulary registry.
func TestRPCDispatchMethodsDeclared(t *testing.T) {
	root := repositoryRoot(t)
	files := token.NewFileSet()
	generated, err := parser.ParseFile(files, filepath.Join(root, "internal/rpc/vocabulary_gen.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	constants := make(map[string]string)
	ast.Inspect(generated, func(node ast.Node) bool {
		declaration, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for index, name := range declaration.Names {
			if index >= len(declaration.Values) {
				continue
			}
			value, ok := declaration.Values[index].(*ast.BasicLit)
			if !ok || value.Kind != token.STRING {
				continue
			}
			decoded, err := strconv.Unquote(value.Value)
			if err != nil {
				t.Fatal(err)
			}
			constants[name.Name] = decoded
		}
		return false
	})
	source, err := parser.ParseFile(files, filepath.Join(root, "internal/cli/cli.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	methods, switches := rpc.MethodCapabilities(), 0
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "Handle" || function.Recv == nil {
			continue
		}
		receiver, ok := function.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		name, ok := receiver.X.(*ast.Ident)
		if !ok || name.Name != "rpcHandler" {
			continue
		}
		for _, statement := range function.Body.List {
			dispatch, ok := statement.(*ast.SwitchStmt)
			if !ok {
				continue
			}
			tag, ok := dispatch.Tag.(*ast.SelectorExpr)
			if !ok || tag.Sel.Name != "Method" {
				continue
			}
			call, ok := tag.X.(*ast.Ident)
			if !ok || call.Name != "call" {
				continue
			}
			switches++
			for _, branch := range dispatch.Body.List {
				for _, expression := range branch.(*ast.CaseClause).List {
					method := ""
					switch value := expression.(type) {
					case *ast.SelectorExpr:
						qualifier, ok := value.X.(*ast.Ident)
						if ok && qualifier.Name == "rpc" {
							method = constants[value.Sel.Name]
						}
					case *ast.BasicLit:
						if value.Kind == token.STRING {
							method, err = strconv.Unquote(value.Value)
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					if _, declared := methods[method]; !declared {
						t.Errorf("%s: dispatch method %q is not declared in RPC vocabulary", files.Position(expression.Pos()), method)
					}
				}
			}
		}
	}
	if switches != 1 {
		t.Fatalf("expected one rpcHandler.Handle method switch, found %d", switches)
	}
}
