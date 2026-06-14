// Generic typed SDK and API reference generator helpers.

package apisdkgen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var pathParamRe = regexp.MustCompile(`\{(\w+)\}`)

// swiftReservedWords is the set of Swift keywords that require backtick escaping
// when used as property names.
var swiftReservedWords = map[string]struct{}{
	"init": {}, "deinit": {}, "class": {}, "struct": {}, "enum": {},
	"extension": {}, "protocol": {}, "var": {}, "let": {}, "func": {},
	"return": {}, "if": {}, "else": {}, "switch": {}, "case": {},
	"default": {}, "for": {}, "in": {}, "while": {}, "repeat": {},
	"do": {}, "try": {}, "catch": {}, "throw": {}, "throws": {},
	"import": {}, "typealias": {}, "where": {}, "guard": {},
	"defer": {}, "break": {}, "continue": {}, "fallthrough": {},
	"as": {}, "is": {}, "nil": {}, "true": {}, "false": {},
	"self": {}, "Self": {}, "super": {}, "static": {}, "operator": {},
	"type": {},
}

// loadDocsInDir parses Go source files in dir and extracts documentation
// comments, source file tracking, and alias type definitions.
func loadDocsInDir(dir string) (*docRegistry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	reg := &docRegistry{
		typeDoc:  map[string]string{},
		typeFile: map[string]string{},
		fieldDoc: map[string]map[string]string{},
	}

	// Pass 1: collect struct types and string alias declarations.
	stringAliases := map[string]string{} // type name → source file
	for _, file := range files {
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				switch t := typeSpec.Type.(type) {
				case *ast.StructType:
					var doc string
					if typeSpec.Doc != nil {
						doc = typeSpec.Doc.Text()
					} else if genDecl.Doc != nil && len(genDecl.Specs) == 1 {
						doc = genDecl.Doc.Text()
					}
					fn := filepath.Base(fset.Position(typeSpec.Pos()).Filename)
					reg.typeFile[typeSpec.Name.Name] = fn
					if doc = strings.TrimSpace(doc); doc != "" {
						reg.typeDoc[typeSpec.Name.Name] = doc
					}
					fieldDocs := map[string]string{}
					for _, field := range t.Fields.List {
						var fdoc string
						if field.Doc != nil {
							fdoc = strings.TrimSpace(field.Doc.Text())
						} else if field.Comment != nil {
							fdoc = strings.TrimSpace(field.Comment.Text())
						}
						if fdoc != "" {
							for _, name := range field.Names {
								fieldDocs[name.Name] = fdoc
							}
						}
					}
					if len(fieldDocs) > 0 {
						reg.fieldDoc[typeSpec.Name.Name] = fieldDocs
					}
				case *ast.Ident:
					if t.Name == "string" {
						fn := filepath.Base(fset.Position(typeSpec.Pos()).Filename)
						stringAliases[typeSpec.Name.Name] = fn
						reg.typeFile[typeSpec.Name.Name] = fn
					}
				case *ast.ArrayType:
					// Named slice type.
					fn := filepath.Base(fset.Position(typeSpec.Pos()).Filename)
					reg.typeFile[typeSpec.Name.Name] = fn
					var doc string
					if typeSpec.Doc != nil {
						doc = typeSpec.Doc.Text()
					} else if genDecl.Doc != nil && len(genDecl.Specs) == 1 {
						doc = genDecl.Doc.Text()
					}
					if doc = strings.TrimSpace(doc); doc != "" {
						reg.typeDoc[typeSpec.Name.Name] = doc
					}
				}
			}
		}
	}

	// Pass 2: collect constants for string alias types.
	aliasConsts := map[string][]aliasConstant{}
	for _, file := range files {
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.CONST {
				continue
			}
			var lastType string
			for _, spec := range genDecl.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typeName := lastType
				if vs.Type != nil {
					if ident, ok := vs.Type.(*ast.Ident); ok {
						typeName = ident.Name
						lastType = typeName
					}
				}
				if _, isAlias := stringAliases[typeName]; !isAlias {
					continue
				}
				for _, name := range vs.Names {
					if len(vs.Values) == 0 {
						continue
					}
					lit, ok := vs.Values[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					aliasConsts[typeName] = append(aliasConsts[typeName], aliasConstant{
						name:  name.Name,
						value: val,
					})
				}
			}
		}
	}

	// Build aliases in sorted order for deterministic output.
	aliasNames := slices.Sorted(maps.Keys(stringAliases))
	for _, name := range aliasNames {
		file := stringAliases[name]
		reg.aliases = append(reg.aliases, aliasInfo{
			name:      name,
			file:      file,
			constants: aliasConsts[name],
		})
	}

	reg.aliasNames = map[string]struct{}{}
	for _, a := range reg.aliases {
		reg.aliasNames[a.name] = struct{}{}
	}

	return reg, nil
}

// formatBlockDoc formats a doc string as a /** ... */ block comment with the given indent.
// Returns an empty string when doc is empty.
func formatBlockDoc(doc, indent string) string {
	if doc == "" {
		return ""
	}
	lines := strings.Split(doc, "\n")
	// Drop trailing empty lines.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) == 1 {
		return indent + "/** " + lines[0] + " */\n"
	}
	var b strings.Builder
	b.WriteString(indent + "/**\n")
	for _, l := range lines {
		if l == "" {
			b.WriteString(indent + " *\n")
		} else {
			b.WriteString(indent + " * " + l + "\n")
		}
	}
	b.WriteString(indent + " */\n")
	return b.String()
}

// formatSwiftDoc formats a doc string as Swift triple-slash documentation comments.
// Returns an empty string when doc is empty.
func formatSwiftDoc(doc, indent string) string {
	if doc == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(doc), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range lines {
		if l == "" {
			b.WriteString(indent + "///\n")
		} else {
			b.WriteString(indent + "/// " + l + "\n")
		}
	}
	return b.String()
}

// aliasInfo describes a Go named-string type and its constant values,
// extracted from Go source files by loadDocs.
type aliasInfo struct {
	name      string          // e.g. "Harness"
	file      string          // source filename, e.g. "types.go"
	constants []aliasConstant // enum values
}

// shortName returns the const name with the type prefix stripped
// (e.g. "HarnessClaude" → "Claude"). Used for Kotlin/Swift.
func (a aliasInfo) shortName(c aliasConstant) string { return strings.TrimPrefix(c.name, a.name) }

// aliasConstant is a single enum value for a string type alias.
type aliasConstant struct {
	name  string // const name from Go source, e.g. "HarnessClaude"
	value string // wire value, e.g. "claude"
}
