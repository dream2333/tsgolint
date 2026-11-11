package main

import (
	"bytes"
	"fmt"
	"go/types"
	"log"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/go-json-experiment/json"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/tools/go/packages"
)

// mirrorSizes are the size models a mirror field is checked against.
var mirrorSizes = []types.Sizes{
	types.SizesFor("gc", "amd64"),
	types.SizesFor("gc", "arm64"),
	types.SizesFor("gc", "386"),
}

// emptyStruct stands in for an unexported type that does not affect layout.
// Mirrors are only ever used through pointer casts and never allocated, so the
// placeholder costs nothing at run time.
var emptyStruct = types.NewStruct(nil, nil)

// mirrorFieldType returns a type with the same size and alignment as t that a
// shim package can spell, i.e. one that names no unexported type.
//
// Shims read private fields by casting a pointer to the real struct into a
// pointer to its extra_ mirror, so the mirror has to repeat every field of the
// real struct, including the ones no accessor reads, to keep the offsets of
// the ones that are read. Some of those fields have types the shim package is
// not allowed to name, for example
//
//	signatureExtraArena core.Arena[signatureExtra]
//
// An unexported type behind a pointer, slice, map or channel cannot change the
// layout of the field that holds it, so it becomes struct{}; the field above
// is mirrored as core.Arena[struct{}]. Anywhere else the type is stored inline
// and its size matters, so it is replaced by its underlying type.
//
// Whether a type argument is stored inline depends on the generic type, which
// is not visible from here. Type arguments are therefore tried as struct{}
// first, and by their underlying type if that changes the layout.
//
// A mirror with the wrong layout still compiles and then reads the wrong
// memory, so an error is returned when neither attempt matches.
func mirrorFieldType(t types.Type) (types.Type, error) {
	for _, opaqueArgs := range []bool{true, false} {
		m := rewriteUnexported(t, false, opaqueArgs)
		if sameLayout(t, m) {
			return m, nil
		}
	}
	return nil, fmt.Errorf("no mirror with the same layout for %s", t)
}

func sameLayout(a types.Type, b types.Type) bool {
	for _, sizes := range mirrorSizes {
		if sizes.Sizeof(a) != sizes.Sizeof(b) || sizes.Alignof(a) != sizes.Alignof(b) {
			return false
		}
	}
	return true
}

// rewriteUnexported replaces the unexported named types in t. indirect reports
// whether t sits behind a pointer, slice, map or channel, where a placeholder
// is enough. opaqueArgs treats type arguments as if they were indirect.
func rewriteUnexported(t types.Type, indirect bool, opaqueArgs bool) types.Type {
	switch t := t.(type) {
	case *types.Named:
		if t.Obj().Pkg() != nil && !t.Obj().Exported() {
			if indirect {
				return emptyStruct
			}
			return rewriteUnexported(t.Underlying(), false, opaqueArgs)
		}
		args := t.TypeArgs()
		if args.Len() == 0 {
			return t
		}
		rewritten := make([]types.Type, args.Len())
		changed := false
		for i := range args.Len() {
			rewritten[i] = rewriteUnexported(args.At(i), opaqueArgs, opaqueArgs)
			changed = changed || rewritten[i] != args.At(i)
		}
		if !changed {
			return t
		}
		inst, err := types.Instantiate(nil, t.Origin(), rewritten, false)
		if err != nil {
			return t
		}
		return inst
	case *types.Pointer:
		if elem := rewriteUnexported(t.Elem(), true, opaqueArgs); elem != t.Elem() {
			return types.NewPointer(elem)
		}
	case *types.Slice:
		if elem := rewriteUnexported(t.Elem(), true, opaqueArgs); elem != t.Elem() {
			return types.NewSlice(elem)
		}
	case *types.Chan:
		if elem := rewriteUnexported(t.Elem(), true, opaqueArgs); elem != t.Elem() {
			return types.NewChan(t.Dir(), elem)
		}
	case *types.Map:
		key := rewriteUnexported(t.Key(), true, opaqueArgs)
		elem := rewriteUnexported(t.Elem(), true, opaqueArgs)
		if key != t.Key() || elem != t.Elem() {
			return types.NewMap(key, elem)
		}
	case *types.Array:
		if elem := rewriteUnexported(t.Elem(), indirect, opaqueArgs); elem != t.Elem() {
			return types.NewArray(elem, t.Len())
		}
	}
	return t
}

const tsgoInternalPrefix = "github.com/microsoft/TypeScript/tsc/internal/"

// writeMirrorStruct writes a struct named extra_<name> that mirrors the layout of
// s, followed by mirrors of the unexported structs its pointer fields refer to.
// emitted records the mirrors already written.
func writeMirrorStruct(b *strings.Builder, name string, s *types.Struct, qualifier types.Qualifier, emitted map[string]bool) {
	// Unexported structs can refer back to themselves or to each other.
	if emitted[name] {
		return
	}
	emitted[name] = true
	b.WriteString("type extra_")
	b.WriteString(name)
	b.WriteString(" struct {")

	dependencies := [](struct {
		string
		*types.Struct
	}){}
	for field := range s.Fields() {
		b.WriteString("\n  ")
		if !field.Embedded() {
			b.WriteString(field.Name())
			b.WriteByte(' ')
		}

		if named, ok := field.Type().(*types.Named); ok && !named.Obj().Exported() {
			if field.Embedded() {
				// An embedded unexported type is mirrored by its underlying
				// type, which needs a field name to be valid Go.
				b.WriteString(field.Name())
				b.WriteByte(' ')
			}
			// A mirror cannot refer to an unexported named type from the
			// source package. Its instantiated underlying type has the same
			// layout and only needs to be structurally equivalent here.
			b.WriteString(types.TypeString(named.Underlying(), qualifier))
			continue
		}

		ptrType, ok := field.Type().(*types.Pointer)
		if ok {
			named, ok := ptrType.Elem().(*types.Named)
			if ok && !named.Obj().Exported() {
				strct, ok := named.Underlying().(*types.Struct)
				if ok {
					n := named.Obj().Name()
					dependencies = append(dependencies, struct {
						string
						*types.Struct
					}{n, strct})
					b.WriteString("*extra_")
					b.WriteString(n)
					continue
				}
			}
		}

		mirrorType, err := mirrorFieldType(field.Type())
		if err != nil {
			log.Fatalf("extra_%s.%s: %v", name, field.Name(), err)
		}
		b.WriteString(types.TypeString(mirrorType, qualifier))
	}
	b.WriteString("\n}\n")

	for _, dep := range dependencies {
		writeMirrorStruct(b, dep.string, dep.Struct, qualifier, emitted)
	}
}

func signatureHasUnexportedType(t types.Signature) bool {
	if params := t.Params(); params != nil {
		for v := range params.Variables() {
			ty := v.Type()

			if ptrType, ok := ty.(*types.Pointer); ok {
				ty = ptrType.Elem()
			}
			if named, ok := ty.(*types.Named); ok {
				if !named.Obj().Exported() {
					return true
				}
			}
		}
	}
	return false
}

type ExtraShim struct {
	ExtraFunctions  []string
	ExtraMethods    map[string]([]string)
	ExtraFields     map[string]([]string)
	IgnoreFunctions []string
}

func main() {
	packagesToShim := []string{
		"ast",
		"bundled",
		"checker",
		"compiler",
		"contentmapper",
		"core",
		"jsnum",
		"lsp/lsproto",
		"parser",
		"project",
		"scanner",
		"tsoptions",
		"tspath",
		"vfs",
		"vfs/cachedvfs",
		"vfs/osvfs",
	}

	packagesToShimFullNames := make([]string, len(packagesToShim))
	for i, pkg := range packagesToShim {
		packagesToShimFullNames[i] = tsgoInternalPrefix + pkg
	}

	packages, err := packages.Load(&packages.Config{
		// TODO: path relative to repo root
		Dir:  "./shim/compiler",
		Mode: packages.LoadSyntax,
	}, packagesToShimFullNames...)
	if err != nil {
		log.Fatalf("Error loading package: %v", err)
	}

	var shimHeaderBuilder strings.Builder
	var shimBuilder strings.Builder
	var tempBuffer bytes.Buffer

	for _, pkg := range packages {
		shimDirPath := path.Join("./shim/", strings.TrimPrefix(pkg.PkgPath, tsgoInternalPrefix))
		var extraShim ExtraShim
		extraShimFilePath := path.Join(shimDirPath, "extra-shim.json")
		if data, err := os.ReadFile(extraShimFilePath); err == nil {
			if err := json.Unmarshal(data, &extraShim); err != nil {
				fmt.Printf("error parsing %v: %v", extraShimFilePath, err)
				return
			}
		}
		if extraShim.ExtraMethods == nil {
			extraShim.ExtraMethods = map[string][]string{}
		}
		if extraShim.ExtraFunctions == nil {
			extraShim.ExtraFunctions = []string{}
		}
		if extraShim.ExtraFields == nil {
			extraShim.ExtraFields = map[string]([]string){}
		}
		if extraShim.IgnoreFunctions == nil {
			extraShim.IgnoreFunctions = []string{}
		}

		// true if directly used, false otherwise
		importedPackages := map[string]bool{}

		importPackage := func(pkg string, directly bool) {
			if directly {
				importedPackages[pkg] = true
			} else if _, ok := importedPackages[pkg]; !ok {
				importedPackages[pkg] = false
			}
		}

		var qualifierOnlyPackageName types.Qualifier = func(p *types.Package) string {
			importPackage(p.Path(), true)
			return p.Name()
		}
		var qualifierEmptyPackageName types.Qualifier = func(p *types.Package) string {
			return ""
		}

		emitGoLinknameDirective := func(localName string, fn *types.Func) {
			// //go:linkname only allowed in Go files that import "unsafe"
			importPackage("unsafe", false)
			importPackage(pkg.Types.Path(), false)
			shimBuilder.WriteString("//go:linkname ")
			shimBuilder.WriteString(localName)
			shimBuilder.WriteByte(' ')
			shimBuilder.WriteString(fn.Pkg().Path())
			shimBuilder.WriteByte('.')
			if recv := fn.Signature().Recv(); recv != nil {
				shimBuilder.WriteByte('(')
				shimBuilder.WriteString(types.TypeString(recv.Type(), qualifierEmptyPackageName))
				shimBuilder.WriteByte(')')
				shimBuilder.WriteByte('.')
			}
			shimBuilder.WriteString(fn.Name())
			shimBuilder.WriteByte('\n')
		}

		emitLinkedFunction := func(fn *types.Func) bool {
			if fn.Signature().TypeParams() != nil {
				// https://github.com/golang/go/issues/60425
				// linking to functions with generics is not supported in go:linkname
				return false
			}
			if signatureHasUnexportedType(*fn.Signature()) {
				fmt.Fprintf(os.Stderr, "Skipping %s.%s: references unexported types\n", fn.Pkg().Name(), fn.Name())
				return false
			}
			name := cases.Title(language.English, cases.NoLower).String(fn.Name())
			emitGoLinknameDirective(name, fn)
			shimBuilder.WriteString("func ")
			shimBuilder.WriteString(name)
			types.WriteSignature(&tempBuffer, fn.Signature(), qualifierOnlyPackageName)
			shimBuilder.Write(tempBuffer.Bytes())
			tempBuffer.Reset()
			shimBuilder.WriteString("\n")
			return true
		}

		matchedExtraFunctions := make(map[string]bool, len(extraShim.ExtraFunctions))
		for _, name := range extraShim.ExtraFunctions {
			matchedExtraFunctions[name] = false
		}
		matchedExtraMethods := make(map[string](map[string]bool), len(extraShim.ExtraMethods))
		for name, methods := range extraShim.ExtraMethods {
			matchedExtraMethods[name] = make(map[string]bool, len(methods))
			for _, method := range methods {
				matchedExtraMethods[name][method] = false
			}
		}
		matchedExtraFields := make(map[string]bool, len(extraShim.ExtraFields))
		for name := range extraShim.ExtraFields {
			matchedExtraFields[name] = false
		}

		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			object := scope.Lookup(name)
			if !object.Exported() {
				fn, isFunc := object.(*types.Func)
				if _, exists := matchedExtraFunctions[name]; isFunc && exists {
					if emitLinkedFunction(fn) {
						matchedExtraFunctions[name] = true
					}
				}
				continue
			}

			printReexport := func(kind string) {
				importPackage(pkg.Types.Path(), true)
				shimBuilder.WriteString(kind)
				shimBuilder.WriteString(" ")
				shimBuilder.WriteString(name)
				shimBuilder.WriteString(" = ")
				shimBuilder.WriteString(pkg.Name)
				shimBuilder.WriteString(".")
				shimBuilder.WriteString(name)
				shimBuilder.WriteString("\n")
			}

			switch object.(type) {
			case *types.TypeName:
				typeName := object.(*types.TypeName)
				t := typeName.Type()
				named, isNamed := t.(*types.Named)
				if isNamed {
					_, nameWithTypeParams, _ := strings.Cut(types.TypeString(named, qualifierOnlyPackageName), ".")
					importPackage(pkg.Types.Path(), true)
					shimBuilder.WriteString("type ")
					shimBuilder.WriteString(nameWithTypeParams)
					shimBuilder.WriteString(" = ")
					shimBuilder.WriteString(pkg.Name)
					shimBuilder.WriteString(".")
					shimBuilder.WriteString(name)

					typeParams := slices.Collect(named.TypeParams().TypeParams())
					if len(typeParams) > 0 {
						// (*typeWriter)typeList
						shimBuilder.WriteByte('[')
						for i, ty := range typeParams {
							if i > 0 {
								shimBuilder.WriteByte(',')
							}
							shimBuilder.WriteString(ty.String())
						}
						shimBuilder.WriteByte(']')
					}

					shimBuilder.WriteString("\n")
				} else {
					printReexport("type")
				}

				if extraMethods, ok := matchedExtraMethods[name]; isNamed && ok {
					for method := range named.Methods() {
						methodName := method.Name()
						if _, exists := extraMethods[methodName]; !exists {
							continue
						}
						extraMethods[methodName] = true
						prefix := name + "_"
						emitGoLinknameDirective(prefix+methodName, method)
						funcDeclStr := types.ObjectString(method, qualifierOnlyPackageName)
						recvStart := 0
						recvEnd := 0
						paramsStart := 0
						for i, s := range funcDeclStr {
							if s == '(' {
								if recvStart == 0 {
									recvStart = i + 1
								}
								if recvEnd != 0 {
									paramsStart = i + 1
									break
								}
							}
							if s == ')' && recvEnd == 0 {
								recvEnd = i
							}
						}
						shimBuilder.WriteString("func ")
						shimBuilder.WriteString(prefix)
						shimBuilder.WriteString(funcDeclStr[recvEnd+2 : paramsStart])
						shimBuilder.WriteString("recv ")
						shimBuilder.WriteString(funcDeclStr[recvStart:recvEnd])
						if method.Signature().Params() != nil {
							shimBuilder.WriteString(", ")
						}
						shimBuilder.WriteString(funcDeclStr[paramsStart:])
						shimBuilder.WriteString("\n")
					}
				}

				if _, ok := matchedExtraFields[name]; isNamed && ok {
					importPackage("unsafe", true)

					matchedExtraFields[name] = true
					if err != nil {
						log.Fatalf("error formatting %v struct body: %v", name, err)
					}
					mirrorStructName := "extra_" + name

					strct, ok := named.Underlying().(*types.Struct)
					if !ok {
						log.Fatalf("expected %v to be struct", name)
					}

					writeMirrorStruct(&shimBuilder, name, strct, qualifierOnlyPackageName, map[string]bool{})

					mappedFieldTypes := make(map[string]*types.Var, strct.NumFields())
					for field := range strct.Fields() {
						mappedFieldTypes[field.Name()] = field
					}

					for _, field := range extraShim.ExtraFields[name] {
						shimBuilder.WriteString("func ")
						shimBuilder.WriteString(name)
						shimBuilder.WriteByte('_')
						shimBuilder.WriteString(field)
						shimBuilder.WriteString("(v *")
						shimBuilder.WriteString(pkg.Name)
						shimBuilder.WriteByte('.')
						shimBuilder.WriteString(name)
						shimBuilder.WriteString(") ")

						fieldVar, ok := mappedFieldTypes[field]
						if !ok {
							log.Fatalf("expected struct %q to contain field %q", name, field)
						}
						shimBuilder.WriteString(types.TypeString(fieldVar.Type(), qualifierOnlyPackageName))
						shimBuilder.WriteString(" {\n")
						shimBuilder.WriteString("  return ((*")
						shimBuilder.WriteString(mirrorStructName)
						shimBuilder.WriteString(")(unsafe.Pointer(v))).")
						shimBuilder.WriteString(field)
						shimBuilder.WriteString("\n")
						shimBuilder.WriteString("}\n")
					}
				}
			case *types.Const:
				printReexport("const")
			case *types.Var:
				printReexport("var")
			case *types.Func:
				if !slices.Contains(extraShim.IgnoreFunctions, name) {
					funcType := object.(*types.Func)
					emitLinkedFunction(funcType)
				}
			}
		}

		exit := false
		for fnName, found := range matchedExtraFunctions {
			if found {
				continue
			}
			fmt.Printf("ERROR: couldn't find %v function\n", fnName)
			exit = true
		}
		for name, methods := range matchedExtraMethods {
			for methodName, found := range methods {
				if found {
					continue
				}
				fmt.Printf("ERROR: couldn't find %v.%v method\n", name, methodName)
				exit = true
			}
		}
		if exit {
			os.Exit(1)
		}

		// https://pkg.go.dev/cmd/go#hdr-Generate_Go_files_by_processing_source
		shimHeaderBuilder.WriteString("\n// Code generated by tools/gen_shims. DO NOT EDIT.\n\n")
		shimHeaderBuilder.WriteString("package ")
		shimHeaderBuilder.WriteString(pkg.Name)
		shimHeaderBuilder.WriteString("\n\n")
		importsList := slices.Collect(maps.Keys(importedPackages))
		slices.Sort(importsList)
		for _, imported := range importsList {
			shimHeaderBuilder.WriteString("import ")
			if !importedPackages[imported] {
				shimHeaderBuilder.WriteString("_ ")
			}
			shimHeaderBuilder.WriteString("\"")
			shimHeaderBuilder.WriteString(imported)
			shimHeaderBuilder.WriteString("\"\n")
		}
		shimHeaderBuilder.WriteString("\n")

		shimGoPath := path.Join(shimDirPath, "shim.go")
		file, err := os.Create(shimGoPath)
		if err != nil {
			log.Fatalf("error opening shim file for writing: %v", err)
		}
		file.WriteString(shimHeaderBuilder.String())
		file.WriteString(shimBuilder.String())

		shimHeaderBuilder.Reset()
		shimBuilder.Reset()
	}
}
