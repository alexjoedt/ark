package ark_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var codeSpan = regexp.MustCompile("`[^`]+`")

// TestDocumentedIdentifiersExist fails on every qualified identifier
// (ark.Open, arksftp.Config.Host) in a code span or a Go code block of
// README.md and docs/ (adr/ and node_modules skipped) that the named package
// of a go.work module does not export.
func TestDocumentedIdentifiersExist(t *testing.T) {
	exported := exportedIdents(t, workModules(t))
	names := make([]string, 0, len(exported))
	for name := range exported {
		names = append(names, regexp.QuoteMeta(name))
	}
	slices.Sort(names)
	ident := regexp.MustCompile(`\b(` + strings.Join(names, "|") + `)\.([A-Z]\w*)(?:\.([A-Z]\w*))?`)

	for _, file := range docFiles(t) {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		fence, goFence := "", false
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if fence == "" && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
				fence = trimmed[:3]
				goFence = strings.HasPrefix(strings.TrimSpace(trimmed[3:]), "go")
				continue
			}
			if fence != "" && strings.HasPrefix(trimmed, fence) {
				fence = ""
				continue
			}
			var spans []string
			switch {
			case fence == "":
				spans = codeSpan.FindAllString(line, -1)
			case goFence:
				spans = []string{line}
			}
			for _, span := range spans {
				for _, m := range ident.FindAllStringSubmatch(span, -1) {
					pkg, name, member := m[1], m[2], m[3]
					idents := exported[pkg]
					switch {
					case !idents[name]:
						t.Errorf("%s:%d: %s.%s is not exported by package %s", file, i+1, pkg, name, pkg)
					case member != "" && idents[name+"."] && !idents[name+"."+member]:
						t.Errorf(
							"%s:%d: %s.%s.%s is not a method or field of %s.%s",
							file,
							i+1,
							pkg,
							name,
							member,
							pkg,
							name,
						)
					}
				}
			}
		}
	}
}

// exportedIdents returns, per package name, the exported identifiers of the
// non-main, non-internal packages of the go.work modules: "X" for every
// top-level declaration, "T." for every type and "T.M" for its methods,
// fields and interface methods. Files under any build constraint count, so an
// identifier of another platform is still known.
func exportedIdents(t *testing.T, modules []string) map[string]map[string]bool {
	t.Helper()
	exported := map[string]map[string]bool{}
	fset := token.NewFileSet()
	walkModules(t, modules, func(path string) error {
		if strings.HasSuffix(path, "_test.go") ||
			slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "internal") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if f.Name.Name == "main" {
			return nil
		}
		idents := exported[f.Name.Name]
		if idents == nil {
			idents = map[string]bool{}
			exported[f.Name.Name] = idents
		}
		add := func(name string) {
			if ast.IsExported(name) {
				idents[name] = true
			}
		}
		addMembers := func(typ string, fields *ast.FieldList) {
			if fields == nil {
				return
			}
			for _, field := range fields.List {
				for _, n := range field.Names {
					if n.IsExported() {
						idents[typ+"."+n.Name] = true
					}
				}
			}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					add(d.Name.Name)
				} else if recv := recvType(d.Recv.List[0].Type); ast.IsExported(recv) && d.Name.IsExported() {
					idents[recv+"."+d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						add(s.Name.Name)
						idents[s.Name.Name+"."] = true
						switch typ := s.Type.(type) {
						case *ast.StructType:
							addMembers(s.Name.Name, typ.Fields)
						case *ast.InterfaceType:
							addMembers(s.Name.Name, typ.Methods)
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							add(n.Name)
						}
					}
				}
			}
		}
		return nil
	})
	return exported
}

// recvType returns the type name of a method receiver, without pointer and
// type parameters.
func recvType(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
}

// docFiles returns README.md and the Markdown pages of docs/, leaving out
// adr/, node_modules and dot directories.
func docFiles(t *testing.T) []string {
	t.Helper()
	files := []string{"README.md"}
	err := filepath.WalkDir("docs", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "adr" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
