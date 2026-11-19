package ark_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestDocumentedIdentifiersExist fails on every qualified identifier
// (ark.Open, arksftp.Config.Host) in a code span or a Go code block of
// README.md and docs/ (adr/ and node_modules skipped) that the named package
// of a go.work module does not export.
func TestDocumentedIdentifiersExist(t *testing.T) {
	exported := exportedIdents(t, workModules(t))
	for _, file := range docFiles(t) {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range unknownIdents(exported, string(b)) {
			t.Errorf("%s:%s", file, msg)
		}
	}
}

// TestDocIdentScanner pins the rules the docs test relies on: fences and code
// spans after CommonMark, members promoted through embedding and aliases.
func TestDocIdentScanner(t *testing.T) {
	dir := t.TempDir()
	src := `package p
type Source interface{ Walk() }
type Rewalk interface{ Source; Rewalk() }
type inner struct{ Field int }
func (inner) Method() {}
type Outer struct{ *inner }
type Alias = Outer
type Ext struct{ strings.Builder }
`
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	exported := exportedIdents(t, []string{dir})
	doc := "`p.Rewalk.Walk` `p.Outer.Field` `p.Outer.Method` `p.Alias.Method` `p.Ext.String`\n" +
		"`p.Rewalk.Gone` ``a ` p.Span`` `p.Wrapped\n" +
		"Gone`\n" +
		"````md\n```go\n```\n`p.InMd`\n````\n" +
		"```gotmpl\n{{ p.Tmpl }}\n```\n" +
		"```go title=x\np.InGo\n```\n" +
		"`p.After`\n" +
		"```text\n```go\n`p.InText`\n```\n`p.Last`\n" +
		"```p.Inline``` is a span\n"
	want := []string{
		"2: p.Rewalk.Gone is not a method or field of p.Rewalk",
		"2: p.Span is not exported by package p",
		"2: p.Wrapped is not exported by package p",
		"13: p.InGo is not exported by package p",
		"15: p.After is not exported by package p",
		"20: p.Last is not exported by package p",
		"21: p.Inline is not exported by package p",
	}
	if got := unknownIdents(exported, doc); !slices.Equal(got, want) {
		t.Errorf("unknownIdents:\n got %q\nwant %q", got, want)
	}
}

// unknownIdents returns "line: message" for every qualified identifier in a
// code span or a go fence of doc that exported does not know.
func unknownIdents(exported map[string]map[string]bool, doc string) []string {
	names := make([]string, 0, len(exported))
	for name := range exported {
		names = append(names, regexp.QuoteMeta(name))
	}
	slices.Sort(names)
	ident := regexp.MustCompile(`\b(` + strings.Join(names, "|") + `)\.([A-Z]\w*)(?:\.([A-Z]\w*))?`)

	var msgs []string
	scanDoc(doc, func(line int, text string) {
		for _, m := range ident.FindAllStringSubmatchIndex(text, -1) {
			at := line + strings.Count(text[:m[0]], "\n")
			pkg, name := text[m[2]:m[3]], text[m[4]:m[5]]
			idents := exported[pkg]
			switch {
			case !idents[name]:
				msgs = append(msgs, fmt.Sprintf("%d: %s.%s is not exported by package %s", at, pkg, name, pkg))
			case m[6] >= 0 && idents[name+"."] && !idents[name+"."+text[m[6]:m[7]]]:
				msgs = append(msgs, fmt.Sprintf("%d: %s.%s.%s is not a method or field of %s.%s",
					at, pkg, name, text[m[6]:m[7]], pkg, name))
			}
		}
	})
	return msgs
}

// scanDoc calls fn with the contents of every code span outside fences, which
// may run over several lines of a paragraph, and with every line of a fence
// whose info string is go, each with the 1-based line it starts on.
func scanDoc(doc string, fn func(line int, text string)) {
	var (
		fenceChar byte
		fenceLen  int
		goFence   bool
		para      []string
		paraLine  int
	)
	flush := func() {
		text := strings.Join(para, "\n")
		for _, s := range codeSpans(text) {
			fn(paraLine+strings.Count(text[:s[0]], "\n"), text[s[0]:s[1]])
		}
		para = nil
	}
	for i, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if fenceLen > 0 {
			if n := charRun(trimmed, fenceChar); n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				fenceLen = 0
			} else if goFence {
				fn(i+1, line)
			}
			continue
		}
		if trimmed != "" && (trimmed[0] == '`' || trimmed[0] == '~') {
			c := trimmed[0]
			n := charRun(trimmed, c)
			info := trimmed[n:]
			if n >= 3 && (c == '~' || !strings.Contains(info, "`")) {
				flush()
				fenceChar, fenceLen = c, n
				words := strings.Fields(info)
				goFence = len(words) > 0 && words[0] == "go"
				continue
			}
		}
		if trimmed == "" {
			flush()
			continue
		}
		if para == nil {
			paraLine = i + 1
		}
		para = append(para, line)
	}
	flush()
}

// codeSpans returns the start and end offsets of the contents of the code
// spans of a paragraph: a run of n backticks up to the next run of exactly n.
func codeSpans(text string) [][2]int {
	var spans [][2]int
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		n := charRun(text[i:], '`')
		start, end := i+n, -1
		for j := start; j < len(text); {
			if text[j] != '`' {
				j++
				continue
			}
			m := charRun(text[j:], '`')
			if m == n {
				end = j
				break
			}
			j += m
		}
		if end < 0 {
			i = start
			continue
		}
		spans = append(spans, [2]int{start, end})
		i = end + n
	}
	return spans
}

func charRun(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// docType is what exportedIdents knows of a type of a package: its own
// fields and methods, the same-package types whose members it gets through
// embedding or an alias, and whether it gets members from a type it cannot
// see (another package, a builtin), which leaves its members unchecked.
type docType struct {
	members map[string]bool
	embeds  []string
	open    bool
}

// exportedIdents returns, per package name, the exported identifiers of the
// non-main, non-internal packages of the go.work modules: "X" for every
// top-level declaration, "T." for every type whose members are all known and
// "T.M" for its fields and methods, promoted ones included. Files under any
// build constraint count, so an identifier of another platform is still
// known. Two packages of the same name fail the test, as `name.X` in the docs
// could not tell them apart.
func exportedIdents(t *testing.T, modules []string) map[string]map[string]bool {
	t.Helper()
	exported := map[string]map[string]bool{}
	dirs := map[string]string{}
	typeDecls := map[string]map[string]*docType{}
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
		pkg := f.Name.Name
		if pkg == "main" {
			return nil
		}
		if dir, ok := dirs[pkg]; ok && dir != filepath.Dir(path) {
			return fmt.Errorf("packages %s and %s share the name %s", dir, filepath.Dir(path), pkg)
		}
		dirs[pkg] = filepath.Dir(path)
		if exported[pkg] == nil {
			exported[pkg] = map[string]bool{}
			typeDecls[pkg] = map[string]*docType{}
		}
		idents, pkgTypes := exported[pkg], typeDecls[pkg]
		typ := func(name string) *docType {
			if pkgTypes[name] == nil {
				pkgTypes[name] = &docType{members: map[string]bool{}}
			}
			return pkgTypes[name]
		}
		inherit := func(dt *docType, expr ast.Expr) {
			name, local := typeName(expr)
			switch {
			case name == "":
			case !local:
				dt.open = true
			case name == "error":
				dt.members["Error"] = true
			case types.Universe.Lookup(name) == nil:
				dt.embeds = append(dt.embeds, name)
			}
		}
		addMembers := func(dt *docType, fields *ast.FieldList) {
			for _, field := range fields.List {
				if len(field.Names) == 0 {
					if name, _ := typeName(field.Type); name != "" {
						dt.members[name] = true
					}
					inherit(dt, field.Type)
				}
				for _, n := range field.Names {
					dt.members[n.Name] = true
				}
			}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					if d.Name.IsExported() {
						idents[d.Name.Name] = true
					}
				} else if recv, _ := typeName(d.Recv.List[0].Type); recv != "" {
					typ(recv).members[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							idents[s.Name.Name] = true
						}
						dt := typ(s.Name.Name)
						switch st := s.Type.(type) {
						case *ast.StructType:
							addMembers(dt, st.Fields)
						case *ast.InterfaceType:
							addMembers(dt, st.Methods)
						default:
							inherit(dt, st)
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								idents[n.Name] = true
							}
						}
					}
				}
			}
		}
		return nil
	})
	for pkg, pkgTypes := range typeDecls {
		for name := range pkgTypes {
			if !ast.IsExported(name) {
				continue
			}
			members := map[string]bool{}
			if !collectMembers(pkgTypes, name, members, map[string]bool{}) {
				continue
			}
			exported[pkg][name+"."] = true
			for m := range members {
				if ast.IsExported(m) {
					exported[pkg][name+"."+m] = true
				}
			}
		}
	}
	return exported
}

// collectMembers adds the fields and methods of type name, own and promoted,
// to members. It reports false when one of them comes from a type it does
// not know.
func collectMembers(decls map[string]*docType, name string, members, seen map[string]bool) bool {
	dt := decls[name]
	if dt == nil || dt.open {
		return false
	}
	if seen[name] {
		return true
	}
	seen[name] = true
	for m := range dt.members {
		members[m] = true
	}
	for _, e := range dt.embeds {
		if !collectMembers(decls, e, members, seen) {
			return false
		}
	}
	return true
}

// typeName returns the name of a named type expression without pointer and
// type arguments, and whether it is declared in the same package.
func typeName(expr ast.Expr) (string, bool) {
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
			return e.Name, true
		case *ast.SelectorExpr:
			return e.Sel.Name, false
		default:
			return "", false
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
