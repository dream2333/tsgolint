package main

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestMirrorFieldType(t *testing.T) {
	pkg := typeCheck(t, "example.com/checker", `package checker

type Exported struct{ X int }
type elem struct{ x, y uint64 }
type kind uint8
type Box[T any] struct{ items []T }
type Inline[T any] struct{ item T }

type fields struct {
	slice    []elem
	pointer  *elem
	mapping  map[kind]*elem
	channel  chan elem
	boxed    Box[elem]
	array    [4]elem
	direct   kind
	inline   Inline[elem]
	exported []*Exported
	builtin  map[string][]uint32
}
`, nil)
	qualifier := func(p *types.Package) string { return p.Name() }
	want := map[string]string{
		"slice":    "[]struct{}",
		"pointer":  "*struct{}",
		"mapping":  "map[struct{}]*struct{}",
		"channel":  "chan struct{}",
		"boxed":    "checker.Box[struct{}]",
		"array":    "[4]struct{x uint64; y uint64}",
		"direct":   "uint8",
		"inline":   "checker.Inline[struct{x uint64; y uint64}]",
		"exported": "[]*checker.Exported",
		"builtin":  "map[string][]uint32",
	}
	s := pkg.Scope().Lookup("fields").Type().Underlying().(*types.Struct)
	for field := range s.Fields() {
		mirror, err := mirrorFieldType(field.Type())
		if err != nil {
			t.Errorf("%s: %v", field.Name(), err)
			continue
		}
		if !sameLayout(field.Type(), mirror) {
			t.Errorf("%s: mirror %s has a different layout", field.Name(), mirror)
		}
		if got := types.TypeString(mirror, qualifier); got != want[field.Name()] {
			t.Errorf("%s: got %q, want %q", field.Name(), got, want[field.Name()])
		}
	}
}

const mirrorTestSource = `package checker

type Exported struct{ X int }

type node struct {
	next *node
	peer *other
	v    uint8
}

type other struct {
	back *node
	n    int32
}

type base struct {
	a uint16
	b uint64
}

type elem struct{ x, y uint64 }

type kind uint8

type Host struct {
	base
	first *node
	items []elem
	k     kind
	e     *Exported
	tail  uint8
}
`

type mapImporter map[string]*types.Package

func (m mapImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := m[path]; ok {
		return pkg, nil
	}
	return importer.Default().Import(path)
}

func typeCheck(t *testing.T, path string, source string, imp types.Importer) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path+".go", source, 0)
	if err != nil {
		t.Fatalf("parsing %v: %v\n%v", path, err, source)
	}
	pkg, err := (&types.Config{Importer: imp}).Check(path, fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatalf("type-checking %v: %v\n%v", path, err, source)
	}
	return pkg
}

func TestWriteMirrorStruct(t *testing.T) {
	real := typeCheck(t, "checker", mirrorTestSource, nil)
	host := real.Scope().Lookup("Host").Type().Underlying().(*types.Struct)

	var b strings.Builder
	// node and other refer to each other and node refers to itself; without
	// the emitted guard this call does not return.
	writeMirrorStruct(&b, "Host", host, func(p *types.Package) string { return p.Name() }, map[string]bool{})
	out := b.String()

	for _, want := range []string{
		"\n  base struct{a uint16; b uint64}\n", // embedded unexported type gets a field name
		"\n  first *extra_node\n",               // pointer to an unexported struct points to its mirror
		"\n  next *extra_node\n",
		"\n  peer *extra_other\n",
		"\n  back *extra_node\n",
		"\n  items []struct{}\n", // unexported slice element becomes a placeholder
		"\n  k uint8\n",          // unexported named type is replaced by its underlying type
		"\n  e *checker.Exported\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mirror is missing %q:\n%v", want, out)
		}
	}
	for _, name := range []string{"Host", "node", "other"} {
		if n := strings.Count(out, "type extra_"+name+" struct {"); n != 1 {
			t.Errorf("extra_%v emitted %v times, want 1:\n%v", name, n, out)
		}
	}

	// The emitted mirrors must be valid Go and lay out like the originals.
	mirror := typeCheck(t, "shim", "package shim\n\nimport \"checker\"\n\nvar _ checker.Exported\n\n"+out, mapImporter{"checker": real})
	for _, name := range []string{"Host", "node", "other"} {
		compareLayout(t, name, mirror.Scope().Lookup("extra_"+name), real.Scope().Lookup(name))
	}
}

const tsgoShimPrefix = "github.com/microsoft/TypeScript/tsc/shim/"

var layoutArchs = []string{"amd64", "arm64", "386"}

// compareLayout reports differences in field names, offsets, sizes and
// alignments between a mirror struct and the struct it mirrors, as computed by
// go/types for the gc compiler.
func compareLayout(t *testing.T, name string, mirrorObj types.Object, realObj types.Object) {
	t.Helper()
	if mirrorObj == nil || realObj == nil {
		t.Errorf("%v: mirror found: %v, mirrored type found: %v", name, mirrorObj != nil, realObj != nil)
		return
	}
	mirror, ok := mirrorObj.Type().Underlying().(*types.Struct)
	if !ok {
		t.Errorf("%v: mirror is not a struct", name)
		return
	}
	real, ok := realObj.Type().Underlying().(*types.Struct)
	if !ok {
		t.Errorf("%v: mirrored type is not a struct", name)
		return
	}
	if mirror.NumFields() != real.NumFields() {
		t.Errorf("%v: mirror has %v fields, mirrored type has %v", name, mirror.NumFields(), real.NumFields())
		return
	}
	var mirrorFields, realFields []*types.Var
	for i := range real.NumFields() {
		mirrorFields = append(mirrorFields, mirror.Field(i))
		realFields = append(realFields, real.Field(i))
		if mirror.Field(i).Name() != real.Field(i).Name() {
			t.Errorf("%v: field %v is named %q in the mirror and %q in the mirrored type", name, i, mirror.Field(i).Name(), real.Field(i).Name())
		}
	}
	for _, arch := range layoutArchs {
		sizes := types.SizesFor("gc", arch)
		if got, want := sizes.Sizeof(mirror), sizes.Sizeof(real); got != want {
			t.Errorf("%v (%v): mirror size %v, want %v", name, arch, got, want)
		}
		mirrorOffsets, realOffsets := sizes.Offsetsof(mirrorFields), sizes.Offsetsof(realFields)
		for i, field := range realFields {
			mirrorType, realType := mirrorFields[i].Type(), field.Type()
			if mirrorOffsets[i] != realOffsets[i] ||
				sizes.Sizeof(mirrorType) != sizes.Sizeof(realType) ||
				sizes.Alignof(mirrorType) != sizes.Alignof(realType) {
				t.Errorf("%v.%v (%v): mirror %v at offset %v (size %v, align %v), want %v at offset %v (size %v, align %v)",
					name, field.Name(), arch,
					mirrorType, mirrorOffsets[i], sizes.Sizeof(mirrorType), sizes.Alignof(mirrorType),
					realType, realOffsets[i], sizes.Sizeof(realType), sizes.Alignof(realType))
			}
		}
	}
}

// TestGeneratedMirrorLayout checks every extra_* mirror struct in the
// generated shims against the struct it mirrors. The shims read private fields
// through these mirrors with unsafe.Pointer casts, so a layout mismatch reads
// the wrong memory instead of failing to compile.
func TestGeneratedMirrorLayout(t *testing.T) {
	shimRoot := filepath.Join("..", "..", "shim")
	var shimDirs []string
	err := filepath.WalkDir(shimRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "shim.go" {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), "\ntype extra_") {
			shimDirs = append(shimDirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shimDirs) == 0 {
		t.Fatal("found no generated shim with mirror structs")
	}

	for _, dir := range shimDirs {
		rel, err := filepath.Rel(shimRoot, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			pkgs, err := packages.Load(&packages.Config{
				// The shim modules are resolved through the replace directives
				// of the root module.
				Dir:  filepath.Join("..", ".."),
				Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
			}, tsgoShimPrefix+filepath.ToSlash(rel))
			if err != nil {
				t.Fatal(err)
			}
			if len(pkgs) != 1 || len(pkgs[0].Errors) > 0 {
				t.Fatalf("loading %v: %v packages, errors: %v", dir, len(pkgs), pkgs[0].Errors)
			}
			shim := pkgs[0]
			internal := shim.Imports[tsgoInternalPrefix+filepath.ToSlash(rel)]
			if internal == nil || internal.Types == nil {
				t.Fatalf("%v does not import %v", shim.PkgPath, tsgoInternalPrefix+filepath.ToSlash(rel))
			}

			checked := 0
			for _, name := range shim.Types.Scope().Names() {
				realName, ok := strings.CutPrefix(name, "extra_")
				if !ok {
					continue
				}
				compareLayout(t, realName, shim.Types.Scope().Lookup(name), internal.Types.Scope().Lookup(realName))
				checked++
			}
			if checked == 0 {
				t.Fatal("found no mirror structs to check")
			}
			t.Logf("checked %v mirror structs", checked)
		})
	}
}
