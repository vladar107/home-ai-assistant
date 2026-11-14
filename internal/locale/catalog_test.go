package locale

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCatalogIsComplete(t *testing.T) {
	root := moduleRoot(t)
	module := modulePath(t, root)
	fset := token.NewFileSet()
	packages := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(file string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if file != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(file))
		if err != nil {
			return err
		}
		importPath := path.Join(module, filepath.ToSlash(rel))
		packages[importPath] = append(packages[importPath], parsed)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if problems := checkModule(fset, packages); len(problems) > 0 {
		slices.Sort(problems)
		t.Fatalf("the catalog has %d problems:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if module, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(module)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
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
			t.Fatal("go.mod not found above the package")
		}
		dir = parent
	}
}

const sampleHeader = "package sample\n\nimport (\n\t\"context\"\n\t\"time\"\n\n\t\"github.com/vladar107/home-ai-assistant/internal/locale\"\n)\n\nvar _ context.Context\nvar _ time.Time\n\n"

func TestCatalogCheckFindsProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		want   string
	}{
		"empty Russian text": {
			source: `var m = locale.T("Hello", "")`,
			want:   "is empty",
		},
		"empty plural form": {
			source: `var m = locale.N("%d task", "%d tasks", "%d задача", "", "%d задач")`,
			want:   "is empty",
		},
		"string against number": {
			source: `var m = locale.T("%s tasks", "%d задач")`,
			want:   "verbs differ",
		},
		"index differs": {
			source: `var m = locale.T("%s of %s", "%[1]s из %[1]s")`,
			want:   "verbs differ",
		},
		"plural form without the number": {
			source: `var m = locale.N("a task", "%d tasks", "%d задача", "%d задачи", "%d задач")`,
			want:   "verbs differ",
		},
		"too few arguments": {
			source: "var m = locale.T(\"%s and %s\", \"%s и %s\")\n\nfunc f(ctx context.Context) string { return m.Format(ctx, \"a\") }",
			want:   "1 arguments",
		},
		"too few arguments for a language": {
			source: "var m = locale.T(\"%s and %s\", \"%s и %s\")\n\nfunc f() string { return m.FormatIn(locale.Russian, \"a\") }",
			want:   "1 arguments",
		},
		"too many arguments": {
			source: "var m = locale.T(\"%s\", \"%s\")\n\nfunc f(ctx context.Context) string { return m.Format(ctx, \"a\", \"b\") }",
			want:   "2 arguments",
		},
		"too few arguments for a count": {
			source: "var m = locale.N(\"%d of %s\", \"%d of %s\", \"%d из %s\", \"%d из %s\", \"%d из %s\")\n\nfunc f(ctx context.Context) string { return m.Format(ctx, 2) }",
			want:   "1 arguments",
		},
		"not a literal": {
			source: "func f(s string) locale.Text { return locale.T(s, \"да\") }",
			want:   "string literals",
		},
		"In on a message with verbs": {
			source: "var m = locale.T(\"%s\", \"%s\")\n\nfunc f() string { return m.In(locale.Russian) }",
			want:   "In is called on a message with format verbs",
		},
		"In on a message with an escaped percent": {
			source: "var m = locale.T(\"100%% done\", \"готово на 100%%\")\n\nfunc f() string { return m.In(locale.Russian) }",
			want:   "In is called on a message with format verbs",
		},
	} {
		t.Run(name, func(t *testing.T) {
			problems := checkSources(t, map[string]string{"example.com/sample": sampleHeader + tc.source})
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("problems = %q, want one containing %q", problems, tc.want)
			}
		})
	}
}

func TestCatalogCheckFindsProblemsAcrossPackages(t *testing.T) {
	sources := map[string]string{
		"example.com/msgs":   "package msgs\n\nimport \"github.com/vladar107/home-ai-assistant/internal/locale\"\n\nvar Two = locale.T(\"%s and %s\", \"%s и %s\")\n",
		"example.com/sample": "package sample\n\nimport (\n\t\"context\"\n\n\t\"example.com/msgs\"\n)\n\nfunc f(ctx context.Context) string { return msgs.Two.Format(ctx, \"a\") }\n",
	}
	if problems := checkSources(t, sources); len(problems) != 1 || !strings.Contains(problems[0], "msgs.Two.Format is called with 1 arguments") {
		t.Fatalf("problems = %q", problems)
	}
}

func TestCatalogCheckRejectsDotImport(t *testing.T) {
	source := "package sample\n\nimport . \"github.com/vladar107/home-ai-assistant/internal/locale\"\n\nvar m = T(\"a\", \"б\")\n"
	if problems := checkSources(t, map[string]string{"example.com/sample": source}); len(problems) != 1 || !strings.Contains(problems[0], "dot-import") {
		t.Fatalf("problems = %q", problems)
	}
}

func TestCatalogCheckAcceptsSound(t *testing.T) {
	source := sampleHeader + `var (
	restored = locale.T("Restored "+"the watch %s at %s.", "Восстановлено: %[1]s, "+"время %[2]s.")
	reorder  = locale.T("%s of %s", "%[2]s: %[1]s")
	tasks    = locale.N("%d task", "%d tasks", "%d задача", "%d задачи", "%d задач")
	plain    = locale.T("100%% done", "готово на 100%%")
	word     = locale.T("Back", "Назад")
)

func f(ctx context.Context, rest ...any) []string {
	return []string{
		restored.Format(ctx, "a", "b"),
		restored.FormatIn(locale.Russian, "a", "b"),
		reorder.Format(ctx, "a", "b"),
		tasks.Format(ctx, 3),
		plain.Format(ctx),
		restored.Format(ctx, rest...),
		word.In(locale.Russian),
	}
}

func shadowed(at time.Time, reorder time.Time) string {
	restored := at
	return at.Format("15:04") + reorder.Format("15:04") + restored.Format("15:04")
}
`
	if problems := checkSources(t, map[string]string{"example.com/sample": source}); len(problems) != 0 {
		t.Fatalf("problems = %q", problems)
	}
}

func checkSources(t *testing.T, sources map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	packages := map[string][]*ast.File{}
	for importPath, source := range sources {
		file, err := parser.ParseFile(fset, importPath+".go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		packages[importPath] = []*ast.File{file}
	}
	return checkModule(fset, packages)
}

// message is a package-level locale.T or locale.N and what a call on it must satisfy.
type message struct {
	needed  int
	indexed bool
	percent bool
}

const localePath = "/internal/locale"

func checkModule(fset *token.FileSet, packages map[string][]*ast.File) []string {
	var problems []string
	report := func(pos token.Pos, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: %s", fset.Position(pos), fmt.Sprintf(format, args...)))
	}
	silent := func(token.Pos, string, ...any) {}
	messages := map[string]message{}
	topLevel := map[*ast.ValueSpec]bool{}
	for importPath, files := range packages {
		for _, file := range files {
			name := localeName(file, report)
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					topLevel[value] = true
					if len(value.Names) != len(value.Values) {
						continue
					}
					for i, expr := range value.Values {
						if call, ok := expr.(*ast.CallExpr); ok {
							if msg, ok := checkLocaleCall(call, name, file.Name.Name, silent); ok {
								messages[importPath+"."+value.Names[i].Name] = msg
							}
						}
					}
				}
			}
		}
	}
	for importPath, files := range packages {
		for _, file := range files {
			name := localeName(file, silent)
			imports := map[string]string{}
			for _, spec := range file.Imports {
				imported, _ := strconv.Unquote(spec.Path.Value)
				if spec.Name != nil {
					imports[spec.Name.Name] = imported
				} else {
					imports[path.Base(imported)] = imported
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if isLocaleCall(call, name, file.Name.Name) != "" {
					checkLocaleCall(call, name, file.Name.Name, report)
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !slices.Contains([]string{"Format", "FormatIn", "In"}, selector.Sel.Name) {
					return true
				}
				key, label := receiverKey(selector.X, importPath, imports, topLevel)
				msg, ok := messages[key]
				if !ok {
					return true
				}
				switch {
				case selector.Sel.Name == "In":
					if msg.percent {
						report(call.Pos(), "%s.In is called on a message with format verbs; use Format or FormatIn", label)
					}
				case call.Ellipsis.IsValid():
				default:
					given := len(call.Args) - 1
					if given < msg.needed || !msg.indexed && given != msg.needed {
						report(call.Pos(), "%s.%s is called with %d arguments, its verbs take %d", label, selector.Sel.Name, given, msg.needed)
					}
				}
				return true
			})
		}
	}
	return problems
}

// receiverKey names the package-level message an expression refers to, as "import path.Name";
// the key is empty for anything else, such as a local variable that shadows a message.
func receiverKey(expr ast.Expr, importPath string, imports map[string]string, topLevel map[*ast.ValueSpec]bool) (key, label string) {
	switch expr := expr.(type) {
	case *ast.Ident:
		if expr.Obj != nil {
			if spec, ok := expr.Obj.Decl.(*ast.ValueSpec); !ok || !topLevel[spec] {
				return "", ""
			}
		}
		return importPath + "." + expr.Name, expr.Name
	case *ast.SelectorExpr:
		pkg, ok := expr.X.(*ast.Ident)
		if !ok || pkg.Obj != nil || imports[pkg.Name] == "" {
			return "", ""
		}
		return imports[pkg.Name] + "." + expr.Sel.Name, pkg.Name + "." + expr.Sel.Name
	}
	return "", ""
}

// localeName is the name the file imports this package under, empty when it does not.
func localeName(file *ast.File, report func(token.Pos, string, ...any)) string {
	for _, spec := range file.Imports {
		imported, _ := strconv.Unquote(spec.Path.Value)
		if !strings.HasSuffix(imported, localePath) {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "." {
				report(spec.Pos(), "dot-import of the locale package hides its calls from the catalog test")
			}
			return spec.Name.Name
		}
		return "locale"
	}
	return ""
}

// isLocaleCall returns "T" or "N" when the call is locale.T or locale.N, or T or N inside the
// package itself.
func isLocaleCall(call *ast.CallExpr, name, pkg string) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if ident, ok := fun.X.(*ast.Ident); ok && name != "" && ident.Name == name && (fun.Sel.Name == "T" || fun.Sel.Name == "N") {
			return fun.Sel.Name
		}
	case *ast.Ident:
		if pkg == "locale" && (fun.Name == "T" || fun.Name == "N") {
			return fun.Name
		}
	}
	return ""
}

func checkLocaleCall(call *ast.CallExpr, name, pkg string, report func(token.Pos, string, ...any)) (message, bool) {
	kind := isLocaleCall(call, name, pkg)
	if kind == "" {
		return message{}, false
	}
	want := map[string]int{"T": 2, "N": 5}[kind]
	if len(call.Args) != want {
		report(call.Pos(), "locale.%s takes %d texts, got %d", kind, want, len(call.Args))
		return message{}, false
	}
	var forms []string
	for i, arg := range call.Args {
		text, ok := constantString(arg)
		if !ok {
			report(arg.Pos(), "locale.%s takes string literals, argument %d is not one", kind, i+1)
			return message{}, false
		}
		if text == "" {
			report(arg.Pos(), "locale.%s form %d is empty", kind, i+1)
			return message{}, false
		}
		forms = append(forms, text)
	}
	msg := message{}
	var first []string
	for i, form := range forms {
		verbs, needed, indexed, err := parseVerbs(form)
		if err != nil {
			report(call.Args[i].Pos(), "locale.%s form %d: %v", kind, i+1, err)
			return message{}, false
		}
		if i == 0 {
			first = verbs
		} else if !slices.Equal(first, verbs) {
			report(call.Args[i].Pos(), "locale.%s form %d: verbs differ from form 1: %v against %v", kind, i+1, verbs, first)
			return message{}, false
		}
		msg.needed = max(msg.needed, needed)
		msg.indexed = msg.indexed || indexed
		msg.percent = msg.percent || strings.Contains(form, "%")
	}
	return msg, true
}

func constantString(expr ast.Expr) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(expr.Value)
		return text, err == nil
	case *ast.ParenExpr:
		return constantString(expr.X)
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			return "", false
		}
		left, ok := constantString(expr.X)
		if !ok {
			return "", false
		}
		right, ok := constantString(expr.Y)
		return left + right, ok
	}
	return "", false
}

// parseVerbs returns the sorted "argument:verb" pairs of a format, how many arguments it takes,
// and whether it uses explicit indexes.
func parseVerbs(format string) (verbs []string, needed int, indexed bool, err error) {
	arg := 1
	runes := []rune(format)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '%' {
			continue
		}
		for i++; ; i++ {
			if i >= len(runes) {
				return nil, 0, false, fmt.Errorf("the format ends inside a verb")
			}
			r := runes[i]
			switch {
			case r == '%':
			case r == '[':
				end := slices.Index(runes[i:], ']')
				if end < 0 {
					return nil, 0, false, fmt.Errorf("an argument index is not closed")
				}
				n, convErr := strconv.Atoi(string(runes[i+1 : i+end]))
				if convErr != nil || n < 1 {
					return nil, 0, false, fmt.Errorf("bad argument index %q", string(runes[i:i+end+1]))
				}
				arg, indexed = n, true
				i += end
				continue
			case r == '*':
				return nil, 0, false, fmt.Errorf("a * width is not supported")
			case strings.ContainsRune("+-# 0123456789.", r):
				continue
			default:
				verbs = append(verbs, fmt.Sprintf("%d:%c", arg, r))
				needed = max(needed, arg)
				arg++
			}
			break
		}
	}
	slices.Sort(verbs)
	return verbs, needed, indexed, nil
}
