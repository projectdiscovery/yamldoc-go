// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
// Taken from https://github.com/talos-systems/talos/blob/master/hack/docgen/main.go

package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"text/template"
	"unicode"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/pkg/errors"
	"golang.org/x/tools/go/packages"
	"gopkg.in/yaml.v2"
	"mvdan.cc/gofumpt/format"
)

var (
	inputPath   = flag.String("path", "", "Root Path to Generate Documentation From")
	structure   = flag.String("structure", "", "Structure Name to Generate Documentation From")
	output      = flag.String("output", "", "File to write generated documentation code to")
	packageName = flag.String("package", "main", "Name of the package for auto-generated code")
)

type Doc struct {
	Name    string
	Package string
	Title   string
	Header  string
	File    string
	Structs []*Struct
}

type Struct struct {
	name          string
	packagePrefix string

	Text      *Text
	Fields    []*Field
	AppearsIn []Appearance
}

// GetName returns the name of the struct. If a package name is provided, it
// is returned as well.
func (s *Struct) GetName() string {
	return wrapStructName(s.packagePrefix, s.name)
}

// GetEscapedName returns the GetName result in escaped form for templating
func (s *Struct) GetEscapedName() string {
	if s.packagePrefix == "" {
		return s.name
	}
	return strings.Join([]string{strings.ToUpper(s.packagePrefix), s.name}, "")
}

type Appearance struct {
	Struct    *Struct
	FieldName string
}

type Example struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type Field struct {
	Name    string
	Type    string
	TypeRef string
	Text    *Text
	Tag     string
	Note    string

	embeddedStruct string
}

type Text struct {
	Comment     string     `json:"-"`
	Description string     `json:"description"`
	Examples    []*Example `json:"examples"`
	Values      []string   `json:"values"`
}

func main() {
	flag.Parse()

	if err := process(); err != nil {
		log.Fatalf("FAIL: %s\n", err.Error())
	}
}

// process performs the documentation generation process on the loaded code
func process() error {
	pkgs, err := loadRootPackage()
	if err != nil {
		return errors.Wrap(err, "could not load packages")
	}

	var structures []*structType
	// Iterate through all the packages and files loaded for the root structure,
	// trying to find the main structure for which documentation is to be
	// created.
	for _, pkg := range pkgs {
		structures = append(structures, collectStructsWithOpts(&collectStructOptions{
			pkg:        pkg,
			structName: *structure,
		})...)
	}

	if len(structures) == 0 {
		log.Fatalf("failed to find types that could be documented in %s", *inputPath)
	}

	doc := &Doc{
		Package: *packageName,
		Name:    *structure,
		Structs: []*Struct{},
		File:    *output,
	}

	extraExamples := map[string][]*Example{}
	backReferences := map[string][]Appearance{}

	for i := len(structures) - 1; i >= 0; i-- {
		s := structures[i]

		fmt.Printf("generating docs for type: %q\n", s.name)

		newStruct := &Struct{
			name:          s.name,
			packagePrefix: s.packagePrefix,
			Text:          s.text,
			Fields:        s.fields,
		}

		for _, field := range s.fields {
			if field.TypeRef == "" {
				continue
			}

			if len(field.Text.Examples) > 0 {
				extraExamples[field.TypeRef] = append(extraExamples[field.TypeRef], field.Text.Examples...)
			}

			backReferences[field.TypeRef] = append(backReferences[field.TypeRef], Appearance{
				Struct:    newStruct,
				FieldName: field.Tag,
			})
		}
		doc.Structs = append(doc.Structs, newStruct)
	}

	for _, s := range doc.Structs {
		if extra, ok := extraExamples[s.GetName()]; ok {
			s.Text.Examples = append(s.Text.Examples, extra...)
		}

		if ref, ok := backReferences[s.GetName()]; ok {
			s.AppearsIn = append(s.AppearsIn, ref...)
		}
	}
	if err := render(doc, *output); err != nil {
		return errors.Wrap(err, "could not render")
	}
	return nil
}

// loadRootPackage loads the package from the disk
func loadRootPackage() ([]*decorator.Package, error) {
	abs, err := filepath.Abs(*inputPath)
	if err != nil {
		return nil, errors.Wrap(err, "could not get absolute path")
	}
	pkgs, err := decorator.Load(&packages.Config{
		Dir:  abs,
		Mode: packages.LoadAllSyntax,
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not load package")
	}
	return pkgs, nil
}

type collectStructOptions struct {
	pkg           *decorator.Package
	structName    string
	packagePrefix string // prefix of the package if not root (blank if root package)
}

type structType struct {
	node          *dst.StructType
	pkg           *decorator.Package
	name          string
	text          *Text
	fields        []*Field
	packagePrefix string
}

func wrapStructName(prefix, suffix string) string {
	if prefix == "" {
		return suffix
	}
	return strings.Join([]string{prefix, suffix}, ".")
}

// collectStructsWithOpts collects a structure from a package based on the
// the provided options.
//
// The iteration also accounts for sub-structures, or structures of structures.
// The collectStructsWithOpts function is called recursively, performing deep dive
// into the declared types and collecting all their related information
// for documentation generation.
func collectStructsWithOpts(collectOpts *collectStructOptions) []*structType {
	var structs []*structType

	for _, spec := range collectOpts.pkg.Syntax {
		structs = append(structs, collectStructsFromDSTNode(spec, collectOpts)...)
	}
	return structs
}

//  collectStructsFromDSTNode is a wrapper around parseStructuresFromDSTSpec
func collectStructsFromDSTNode(node dst.Node, collectOpts *collectStructOptions) []*structType {
	structs := []*structType{}

	collectStructs := func(n dst.Node) bool {
		g, ok := n.(*dst.GenDecl)
		if !ok {
			return true
		}

		for _, spec := range g.Specs {
			if parsed := parseStructuresFromDSTSpec(n, spec, collectOpts); parsed != nil {
				structs = append(structs, parsed...)
			}
		}
		return true
	}
	dst.Inspect(node, collectStructs)
	return structs
}

// parseStructuresFromDSTSpec parses a structure from a DST specification
// while also handling all its nested structures, etc returning a list
// of all collected structures in the end.
func parseStructuresFromDSTSpec(node dst.Node, spec dst.Spec, collectOpts *collectStructOptions) []*structType {
	t, ok := spec.(*dst.TypeSpec)
	if !ok {
		return nil
	}

	if t.Type == nil {
		return nil
	}

	x, ok := t.Type.(*dst.StructType)
	if !ok {
		return nil
	}

	gotStructName := t.Name.Name
	if !strings.EqualFold(collectOpts.structName, gotStructName) {
		return nil
	}
	if !unicode.IsUpper(rune(gotStructName[0])) {
		return nil
	}

	var results []*structType
	s := &structType{
		name:          gotStructName,
		node:          x,
		text:          parseComment([]byte(uncommentDecorationNode(node))),
		pkg:           collectOpts.pkg,
		packagePrefix: collectOpts.packagePrefix,
	}
	fields, structures := collectFields(s, collectOpts)
	for _, item := range structures {
		results = append(results, item)
	}
	s.fields = fields
	results = append(results, s)
	return results
}

// collectFields collects all the fields from a structure, as well
// as collecting any nested structures based on their types.
func collectFields(s *structType, collectOpts *collectStructOptions) (fields []*Field, structs []*structType) {
	fields = []*Field{}

	var foundStructures []*structType

	for _, f := range s.node.Fields.List {
		if f.Tag == nil {
			continue
		}
		tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
		yamlTag := tag.Get("yaml")
		yamlTag = strings.Split(yamlTag, ",")[0]

		if yamlTag == "" || yamlTag == "-" {
			continue
		}
		yamlTag = strings.ToLower(yamlTag)

		documentation := uncommentDecorationNode(f)
		if documentation == "" {
			log.Printf("field %q is missing a documentation", f.Names[0].Name)
			continue
		}
		if strings.Contains(documentation, "docgen:nodoc") {
			continue
		}

		if len(f.Names) == 0 {
			starExpr, ok := f.Type.(*dst.StarExpr)
			if !ok {
				continue
			}
			ident, ok := starExpr.X.(*dst.Ident)
			if !ok {
				continue
			}
			if ident.Obj == nil {
				continue
			}
			spec, ok := ident.Obj.Decl.(*dst.TypeSpec)
			if !ok {
				continue
			}
			log.Printf("got embedded struct: %T %T\n", spec, starExpr.X)
			for _, structure := range parseStructuresFromDSTSpec(spec, spec, &collectStructOptions{
				pkg:           collectOpts.pkg,
				structName:    spec.Name.Name,
				packagePrefix: collectOpts.packagePrefix,
			}) {
				for _, field := range structure.fields {
					fields = append(fields, field)
				}
			}
			continue
		}
		name := f.Names[0].Name

		if !unicode.IsUpper(rune(name[0])) {
			continue
		}
		fieldType := formatFieldType(f.Type, s.packagePrefix, false)
		fieldTypeRef := getFieldType(f.Type, s.packagePrefix, false)

		collectUnresolvedExternalStructs(f.Type, &foundStructures, collectOpts)

		field := &Field{
			Name:    name,
			Tag:     yamlTag,
			Type:    fieldType,
			TypeRef: fieldTypeRef,
			Text:    parseComment([]byte(documentation)),
		}
		fields = append(fields, field)
	}
	return fields, foundStructures
}

// collectUnresolvedExternalStructs collects unresolved external structures
// for a package into the list.
func collectUnresolvedExternalStructs(p interface{}, results *[]*structType, collectOpts *collectStructOptions) {
	if m, ok := p.(*dst.MapType); ok {
		collectUnresolvedExternalStructs(m.Key.(dst.Expr), results, collectOpts)
		collectUnresolvedExternalStructs(m.Value.(dst.Expr), results, collectOpts)
		return
	}

	switch t := p.(type) {
	case *dst.Ident:
		if t.Obj != nil { // in case of arrays of objects
			spec := t.Obj.Decl.(*dst.TypeSpec)
			*results = append(*results, parseStructuresFromDSTSpec(spec, spec, &collectStructOptions{
				pkg:           collectOpts.pkg,
				structName:    t.Name,
				packagePrefix: collectOpts.packagePrefix,
			})...)
		}
		if t.Path != "" {
			structPackage, ok := collectOpts.pkg.Imports[t.Path]
			if !ok {
				log.Printf("[debug] [ref] no package found for struct %s: %s\n", collectOpts.structName, t.Path)
				return
			}
			*results = append(*results, collectStructsWithOpts(&collectStructOptions{
				pkg:           structPackage,
				structName:    t.Name,
				packagePrefix: path.Base(t.Path),
			})...)
		}
	case *dst.ArrayType:
		collectUnresolvedExternalStructs(t.Elt, results, collectOpts)
	case *dst.StructType:
		//		return "struct"
	case *dst.StarExpr:
		collectUnresolvedExternalStructs(t.X, results, collectOpts)
	case *dst.SelectorExpr:
		collectUnresolvedExternalStructs(t.Sel, results, collectOpts)
	default:
	}
}

func getFieldType(p interface{}, prefix string, apply bool) string {
	if m, ok := p.(*dst.MapType); ok {
		return getFieldType(m.Value, prefix, false)
	}

	switch t := p.(type) {
	case *dst.Ident:
		if t.Path != "" {
			return wrapStructName(path.Base(t.Path), t.Name) // If we have a path
		}
		if apply && prefix != "" {
			return wrapStructName(prefix, t.Name)
		}
		return t.Name
	case *dst.ArrayType:
		return getFieldType(p.(*dst.ArrayType).Elt, prefix, false)
	case *dst.StarExpr:
		return getFieldType(t.X, prefix, true)
	case *dst.SelectorExpr:
		return getFieldType(t.Sel, prefix, false)
	default:
		return ""
	}
}

// uncommentDecorationNode uncomments comments for a dst node.
func uncommentDecorationNode(node dst.Node) string {
	decorations := node.Decorations()
	parts := decorations.Start.All()

	commentBuilder := &strings.Builder{}
	for i, part := range parts {
		commentBuilder.WriteString(strings.TrimPrefix(part, "// "))
		if i != len(parts)-1 {
			commentBuilder.WriteString("\n")
		}
	}
	return commentBuilder.String()
}

// formatFieldType returns the type of field for a structure
func formatFieldType(p interface{}, prefix string, apply bool) string {
	if m, ok := p.(*dst.MapType); ok {
		return fmt.Sprintf("map[%s]%s", formatFieldType(m.Key, prefix, false), formatFieldType(m.Value, prefix, false))
	}

	switch t := p.(type) {
	case *dst.Ident:
		if t.Path != "" {
			return wrapStructName(path.Base(t.Path), t.Name) // If we have a path
		}
		if apply && prefix != "" {
			return wrapStructName(prefix, t.Name)
		}
		return t.Name
	case *dst.ArrayType:
		return "[]" + formatFieldType(p.(*dst.ArrayType).Elt, prefix, false)
	case *dst.StructType:
		return "struct"
	case *dst.StarExpr:
		return formatFieldType(t.X, prefix, true)
	case *dst.SelectorExpr:
		return formatFieldType(t.Sel, prefix, false)
	case *dst.InterfaceType:
		return "interface{}"
	default:
		log.Printf("unknown: %#v", t)
		return ""
	}
}

func escape(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(
		strings.ReplaceAll(value, "\"", "\\\""),
		"\n",
		"\\n",
	))
}

func parseComment(comment []byte) *Text {
	text := &Text{}
	if err := yaml.Unmarshal(comment, text); err != nil {
		// not yaml, fallback
		text.Description = string(comment)
		// take only the first line from the Description for the comment
		text.Comment = strings.Split(text.Description, "\n")[0]

		// try to parse the everything except for the first line as yaml
		if err = yaml.Unmarshal([]byte(strings.Join(strings.Split(text.Description, "\n")[1:], "\n")), text); err == nil {
			// if parsed, remove it from the description
			text.Description = text.Comment
		}
	} else {
		text.Description = strings.TrimSpace(text.Description)
		// take only the first line from the Description for the comment
		text.Comment = strings.Split(text.Description, "\n")[0]
	}

	text.Description = escape(text.Description)
	for _, example := range text.Examples {
		example.Name = escape(example.Name)
		example.Value = strings.TrimSpace(example.Value)
	}
	return text
}

var tpl = `// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
// DO NOT EDIT: this file is automatically generated by docgen
package {{ .Package }}
import (
	"github.com/projectdiscovery/yamldoc-go/encoder"
)
{{ $tick := "` + "`" + `" -}}
var (
	{{ range $struct := .Structs -}}
	{{ $struct.GetEscapedName }}Doc encoder.Doc
	{{ end -}}
)
func init() {
	{{ range $struct := .Structs -}}
	{{ $docVar := printf "%v%v" $struct.GetEscapedName "Doc" }}
	{{ $docVar }}.Type = "{{ $struct.GetName }}"
	{{ $docVar }}.Comments[encoder.LineComment] = "{{ $struct.Text.Comment }}"
	{{ $docVar }}.Description = "{{ $struct.Text.Description }}"
	{{ range $example := $struct.Text.Examples }}
	{{ if $example.Value }}
	{{ $docVar }}.AddExample("{{ $example.Name }}", {{ $example.Value }})
	{{ end -}}
	{{ end -}}
	{{ if $struct.AppearsIn -}}
	{{ $docVar }}.AppearsIn = []encoder.Appearance{
	{{ range $value := $struct.AppearsIn -}}
		{
			TypeName: "{{ $value.Struct.GetName }}",
			FieldName: "{{ $value.FieldName }}",
		},
	{{ end -}}
	}
	{{ end -}}
	{{ $docVar }}.Fields = make([]encoder.Doc,{{ len $struct.Fields }})
	{{ range $index, $field := $struct.Fields -}}
	{{ $docVar }}.Fields[{{ $index }}].Name = "{{ $field.Tag }}"
	{{ $docVar }}.Fields[{{ $index }}].Type = "{{ $field.Type }}"
	{{ $docVar }}.Fields[{{ $index }}].Note = "{{ $field.Note }}"
	{{ $docVar }}.Fields[{{ $index }}].Description = "{{ $field.Text.Description }}"
	{{ $docVar }}.Fields[{{ $index }}].Comments[encoder.LineComment] = "{{ $field.Text.Comment }}"
	{{ range $example := $field.Text.Examples }}
	{{ if $example.Value }}
	{{ $docVar }}.Fields[{{ $index }}].AddExample("{{ $example.Name }}", {{ $example.Value }})
	{{ end -}}
	{{ end -}}
	{{ if $field.Text.Values -}}
	{{ $docVar }}.Fields[{{ $index }}].Values = []string{
	{{ range $value := $field.Text.Values -}}
		"{{ $value }}",
	{{ end -}}
	}
	{{ end -}}
	{{ end -}}
	{{ end }}
}
// Get{{ .Name }}Doc returns documentation for the file {{ .File }}.
func Get{{ .Name }}Doc() *encoder.FileDoc {
	return &encoder.FileDoc{
		Name: "{{ .Name }}",
		Description: "{{ .Header }}",
		Structs: []*encoder.Doc{
			{{ range $struct := .Structs -}}
			&{{ $struct.GetEscapedName }}Doc,
			{{ end -}}
		},
	}
}
`

func render(doc *Doc, dest string) error {
	t := template.Must(template.New("docfile.tpl").Parse(tpl))
	buf := bytes.Buffer{}

	err := t.Execute(&buf, doc)
	if err != nil {
		return errors.Wrap(err, "could not execute template")
	}

	formatted, err := format.Source(buf.Bytes(), format.Options{})
	if err != nil {
		log.Printf("data: %s", buf.Bytes())
		return errors.Wrap(err, "could not format generate code")
	}

	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	out, err := os.Create(abs)
	if err != nil {
		return errors.Wrap(err, "could not create output file")
	}
	defer out.Close()
	_, err = out.Write(formatted)
	return err
}
