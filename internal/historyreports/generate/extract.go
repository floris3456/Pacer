//go:build ignore

// Reproducible declaration-level extraction; run from the repository root.
// go run internal/historyreports/generate/extract.go /path/to/analytics
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const base = "github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"

type declaration struct {
	node  ast.Decl
	file  string
	names []string
}

func main() {
	root := os.Args[1]
	out := "internal/historyreports/internal/cpamp"
	must(os.MkdirAll(out, 0755))
	must(filepath.WalkDir(out, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			if bytes.HasPrefix(b, []byte("// Code extracted")) {
				return os.Remove(p)
			}
		}
		return nil
	}))
	manifest := map[string]string{}
	extract(root, "service/usagehistory", out, "cpamp", []string{"buildProfileWorkspace", "QuotaEvidence", "PinHistoricalValuation", "comparisonPriceBook", "ProfileConfig", "profileMemberships", "pinProfileFilter", "WorkspaceCacheStatus"}, map[string]bool{"Service": true, "acquireHistory": true, "releaseHistory": true, "wakeWorkspace": true}, manifest)
	extract(root, "model", out+"/model", "model", []string{"ModelPrice", "ModelPriceForContext", "SelectModelPriceServiceTier", "APIKeyAlias"}, nil, manifest)
	extract(root, "usageidentity", out+"/usageidentity", "usageidentity", []string{"AnalyticsModel"}, nil, manifest)
	extract(root, "repository/modelprice", out+"/modelprice", "modelprice", []string{"LoadAll", "LoadAllTx", "repository"}, nil, manifest)
	extract(root, "repository/apikeyalias", out+"/apikeyalias", "apikeyalias", []string{"LoadAll", "repository"}, nil, manifest)
	// Existing controller exports are copied as pure writers, with no Handler.
	extract(root, "http/controller/usagehistory", out+"/exports", "exports", []string{"writeComparisons", "writeQuotaEvidence", "safe"}, map[string]bool{"profileFilter": true}, manifest)
	b, e := os.ReadFile(filepath.Join(root, "LICENSE"))
	must(e)
	must(os.WriteFile(out+"/LICENSE", b, 0644))
	b, e = json.MarshalIndent(manifest, "", "  ")
	must(e)
	must(os.WriteFile(out+"/PROVENANCE.json", append(b, '\n'), 0644))
}
func must(e error) {
	if e != nil {
		panic(e)
	}
}
func extract(root, dir, out, pkg string, roots []string, skip map[string]bool, manifest map[string]string) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	decls := []*declaration{}
	byName := map[string][]*declaration{}
	methods := map[string][]*declaration{}
	paths, e := filepath.Glob(filepath.Join(root, "apps/manager-server/internal", dir, "*.go"))
	must(e)
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, e := os.ReadFile(path)
		must(e)
		f, e := parser.ParseFile(fset, path, raw, parser.ParseComments)
		must(e)
		files[path] = f
		for _, node := range f.Decls {
			parts := []ast.Decl{node}
			if g, ok := node.(*ast.GenDecl); ok {
				if g.Tok == token.IMPORT {
					continue
				}
				if len(g.Specs) > 1 && g.Tok != token.CONST {
					parts = nil
					for _, spec := range g.Specs {
						parts = append(parts, &ast.GenDecl{Doc: g.Doc, Tok: g.Tok, Specs: []ast.Spec{spec}})
					}
				}
			}
			for _, part := range parts {
				d := &declaration{node: part, file: path}
				switch n := part.(type) {
				case *ast.FuncDecl:
					d.names = []string{n.Name.Name}
				case *ast.GenDecl:
					for _, spec := range n.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							d.names = append(d.names, s.Name.Name)
						case *ast.ValueSpec:
							for _, name := range s.Names {
								d.names = append(d.names, name.Name)
							}
						}
					}
				}
				decls = append(decls, d)
				for _, name := range d.names {
					if fn, ok := d.node.(*ast.FuncDecl); ok && fn.Recv != nil {
						methods[name] = append(methods[name], d)
					} else {
						byName[name] = append(byName[name], d)
					}
				}
			}
		}
	}
	selected := map[*declaration]bool{}
	var visit func(string, bool)
	var scan func(ast.Node)
	visit = func(name string, method bool) {
		if skip[name] {
			return
		}
		ds := byName[name]
		if method {
			ds = methods[name]
		}
		for _, d := range ds {
			if selected[d] {
				continue
			}
			selected[d] = true
			scan(d.node)
		}
	}
	scan = func(node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				scan(v.X)
				return false
			case *ast.CallExpr:
				if s, ok := v.Fun.(*ast.SelectorExpr); ok {
					visit(s.Sel.Name, true)
				}
			case *ast.Ident:
				visit(v.Name, false)
			}
			return true
		})
	}
	for _, name := range roots {
		if len(byName[name])+len(methods[name]) == 0 {
			fmt.Printf("root absent %s %s\n", dir, name)
		}
		visit(name, false)
		visit(name, true)
	}
	// Deterministic source-order files. Imported package selectors are not local
	// dependencies; unused imports are pruned from each generated unit.
	must(os.MkdirAll(out, 0755))
	for _, path := range paths {
		f := files[path]
		if f == nil {
			continue
		}
		chosen := []ast.Decl{}
		used := map[string]bool{}
		for _, d := range decls {
			if d.file == path && selected[d] {
				chosen = append(chosen, d.node)
				ast.Inspect(d.node, func(n ast.Node) bool {
					if s, ok := n.(*ast.SelectorExpr); ok {
						if id, ok := s.X.(*ast.Ident); ok {
							used[id.Name] = true
						}
					}
					return true
				})
			}
		}
		if len(chosen) == 0 {
			continue
		}
		imports := []ast.Spec{}
		for _, imp := range f.Imports {
			p, e := strconv.Unquote(imp.Path.Value)
			must(e)
			name := filepath.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if !used[name] {
				continue
			}
			p = strings.ReplaceAll(p, "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/usagehistory", base)
			p = strings.ReplaceAll(p, "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/", base+"/")
			imp.Path.Value = strconv.Quote(p)
			imports = append(imports, imp)
		}
		if len(imports) > 0 {
			chosen = append([]ast.Decl{&ast.GenDecl{Tok: token.IMPORT, Specs: imports}}, chosen...)
		}
		var b bytes.Buffer
		b.WriteString("// Code extracted from CPAMP; Copyright (c) 2026 Seakee, MIT (see LICENSE).\n// Generated by generate/extract.go; calculation declarations retain source semantics.\n")
		must(format.Node(&b, fset, &ast.File{Name: ast.NewIdent(pkg), Decls: chosen}))
		text := b.String()
		// Frozen preparation timestamps are supplied by the Pacer archive boundary.
		if pkg == "cpamp" {
			text = strings.ReplaceAll(text, "time.Now().UnixMilli()", "s.at")
		}
		if pkg == "exports" {
			text = strings.ReplaceAll(text, "func writeComparisons(", "func WriteComparisons(")
			text = strings.ReplaceAll(text, "func writeQuotaEvidence(", "func WriteQuotaEvidence(")
		}
		// Re-prune imports after the deterministic timestamp substitution.
		ff, e := parser.ParseFile(token.NewFileSet(), "", text, parser.ParseComments)
		must(e)
		u := map[string]bool{}
		ast.Inspect(ff, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := s.X.(*ast.Ident); ok {
					u[id.Name] = true
				}
			}
			return true
		})
		for _, imp := range ff.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			name := filepath.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if !u[name] {
				text = strings.ReplaceAll(text, "\t"+imp.Path.Value+"\n", "")
			}
		}
		formatted, e := format.Source([]byte(text))
		must(e)
		must(os.WriteFile(filepath.Join(out, filepath.Base(path)), formatted, 0644))
		raw, e := os.ReadFile(path)
		must(e)
		sum := sha256.Sum256(raw)
		rel, e := filepath.Rel(root, path)
		must(e)
		manifest[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
	}
	list := []string{}
	for d := range selected {
		list = append(list, d.names...)
	}
	sort.Strings(list)
	fmt.Printf("%s: %d declarations\n", dir, len(selected))
}
