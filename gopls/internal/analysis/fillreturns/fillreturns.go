// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fillreturns

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/ast"
	"go/format"
	"go/types"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/gopls/internal/fuzzy"
	"golang.org/x/tools/internal/analysisinternal"
	"golang.org/x/tools/internal/typesinternal"
)

//go:embed doc.go
var doc string

var Analyzer = &analysis.Analyzer{
	Name:             "fillreturns",
	Doc:              analysisinternal.MustExtractDoc(doc, "fillreturns"),
	Run:              run,
	RunDespiteErrors: true,
	URL:              "https://pkg.go.dev/golang.org/x/tools/gopls/internal/analysis/fillreturns",
}

func run(pass *analysis.Pass) (interface{}, error) {
	info := pass.TypesInfo
	if info == nil {
		return nil, fmt.Errorf("nil TypeInfo")
	}

outer:
	for _, typeErr := range pass.TypeErrors {
		// Filter out the errors that are not relevant to this analyzer.
		if !FixesError(typeErr) {
			continue
		}
		var file *ast.File
		for _, f := range pass.Files {
			if f.FileStart <= typeErr.Pos && typeErr.Pos <= f.FileEnd {
				file = f
				break
			}
		}
		if file == nil {
			continue
		}

		// Get the end position of the error.
		// (This heuristic assumes that the buffer is formatted,
		// at least up to the end position of the error.)
		var buf bytes.Buffer
		if err := format.Node(&buf, pass.Fset, file); err != nil {
			continue
		}
		typeErrEndPos := analysisinternal.TypeErrorEndPos(pass.Fset, buf.Bytes(), typeErr.Pos)

		// TODO(rfindley): much of the error handling code below returns, when it
		// should probably continue.

		// Get the path for the relevant range.
		path, _ := astutil.PathEnclosingInterval(file, typeErr.Pos, typeErrEndPos)
		if len(path) == 0 {
			return nil, nil
		}

		// Find the enclosing return statement.
		var ret *ast.ReturnStmt
		var retIdx int
		for i, n := range path {
			if r, ok := n.(*ast.ReturnStmt); ok {
				ret = r
				retIdx = i
				break
			}
		}
		if ret == nil {
			return nil, nil
		}

		// Get the function type that encloses the ReturnStmt.
		var enclosingFunc *ast.FuncType
		for _, n := range path[retIdx+1:] {
			switch node := n.(type) {
			case *ast.FuncLit:
				enclosingFunc = node.Type
			case *ast.FuncDecl:
				enclosingFunc = node.Type
			}
			if enclosingFunc != nil {
				break
			}
		}
		if enclosingFunc == nil || enclosingFunc.Results == nil {
			continue
		}

		// Skip any generic enclosing functions, since type parameters don't
		// have 0 values.
		// TODO(rfindley): We should be able to handle this if the return
		// values are all concrete types.
		if tparams := enclosingFunc.TypeParams; tparams != nil && tparams.NumFields() > 0 {
			return nil, nil
		}

		// Find the function declaration that encloses the ReturnStmt.
		var outer *ast.FuncDecl
		for _, p := range path {
			if p, ok := p.(*ast.FuncDecl); ok {
				outer = p
				break
			}
		}
		if outer == nil {
			return nil, nil
		}

		// Skip any return statements that contain function calls with multiple
		// return values.
		for _, expr := range ret.Results {
			e, ok := expr.(*ast.CallExpr)
			if !ok {
				continue
			}
			if tup, ok := info.TypeOf(e).(*types.Tuple); ok && tup.Len() > 1 {
				continue outer
			}
		}

		// Duplicate the return values to track which values have been matched.
		remaining := make([]ast.Expr, len(ret.Results))
		copy(remaining, ret.Results)

		fixed := make([]ast.Expr, len(enclosingFunc.Results.List))

		// For each value in the return function declaration, find the leftmost element
		// in the return statement that has the desired type. If no such element exists,
		// fill in the missing value with the appropriate "zero" value.
		// Beware that type information may be incomplete.
		var retTyps []types.Type
		for _, ret := range enclosingFunc.Results.List {
			retTyp := info.TypeOf(ret.Type)
			if retTyp == nil {
				return nil, nil
			}
			retTyps = append(retTyps, retTyp)
		}
		matches := analysisinternal.MatchingIdents(retTyps, file, ret.Pos(), info, pass.Pkg)
		qual := typesinternal.FileQualifier(file, pass.Pkg)
		for i, retTyp := range retTyps {
			var match ast.Expr
			var idx int
			for j, val := range remaining {
				if t := info.TypeOf(val); t == nil || !matchingTypes(t, retTyp) {
					continue
				}
				if !typesinternal.IsZeroExpr(val) {
					match, idx = val, j
					break
				}
				// If the current match is a "zero" value, we keep searching in
				// case we find a non-"zero" value match. If we do not find a
				// non-"zero" value, we will use the "zero" value.
				match, idx = val, j
			}

			if match != nil {
				fixed[i] = match
				remaining = append(remaining[:idx], remaining[idx+1:]...)
			} else {
				names, ok := matches[retTyp]
				if !ok {
					return nil, fmt.Errorf("invalid return type: %v", retTyp)
				}
				// Find the identifier most similar to the return type.
				// If no identifier matches the pattern, generate a zero value.
				if best := fuzzy.BestMatch(retTyp.String(), names); best != "" {
					fixed[i] = ast.NewIdent(best)
				} else if zero, isValid := typesinternal.ZeroExpr(retTyp, qual); isValid {
					fixed[i] = zero
				} else {
					return nil, nil
				}
			}
		}

		// Remove any non-matching "zero values" from the leftover values.
		var nonZeroRemaining []ast.Expr
		for _, expr := range remaining {
			if !typesinternal.IsZeroExpr(expr) {
				nonZeroRemaining = append(nonZeroRemaining, expr)
			}
		}
		// Append leftover return values to end of new return statement.
		fixed = append(fixed, nonZeroRemaining...)

		newRet := &ast.ReturnStmt{
			Return:  ret.Pos(),
			Results: fixed,
		}

		// Convert the new return statement AST to text.
		var newBuf bytes.Buffer
		if err := format.Node(&newBuf, pass.Fset, newRet); err != nil {
			return nil, err
		}

		pass.Report(analysis.Diagnostic{
			Pos:     typeErr.Pos,
			End:     typeErrEndPos,
			Message: typeErr.Msg,
			SuggestedFixes: []analysis.SuggestedFix{{
				Message: "Fill in return values",
				TextEdits: []analysis.TextEdit{{
					Pos:     ret.Pos(),
					End:     ret.End(),
					NewText: newBuf.Bytes(),
				}},
			}},
		})
	}
	return nil, nil
}

func matchingTypes(want, got types.Type) bool {
	if want == got || types.Identical(want, got) {
		return true
	}
	// Code segment to help check for untyped equality from (golang/go#32146).
	if rhs, ok := want.(*types.Basic); ok && rhs.Info()&types.IsUntyped > 0 {
		if lhs, ok := got.Underlying().(*types.Basic); ok {
			return rhs.Info()&types.IsConstType == lhs.Info()&types.IsConstType
		}
	}
	return types.AssignableTo(want, got) || types.ConvertibleTo(want, got)
}

// Error messages have changed across Go versions. These regexps capture recent
// incarnations.
//
// TODO(rfindley): once error codes are exported and exposed via go/packages,
// use error codes rather than string matching here.
var wrongReturnNumRegexes = []*regexp.Regexp{
	regexp.MustCompile(`wrong number of return values \(want (\d+), got (\d+)\)`),
	regexp.MustCompile(`too many return values`),
	regexp.MustCompile(`not enough return values`),
}

func FixesError(err types.Error) bool {
	msg := strings.TrimSpace(err.Msg)
	for _, rx := range wrongReturnNumRegexes {
		if rx.MatchString(msg) {
			return true
		}
	}
	return false
}
