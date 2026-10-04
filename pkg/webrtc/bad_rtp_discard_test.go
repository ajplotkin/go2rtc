package webrtc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A single malformed RTP packet must be discarded, not treated as a dead session.
// -16 changed the Unmarshal error branch in OnTrack from close-and-return to
// count-and-continue; -18 was written on top of an older copy of conn.go and
// silently put the close back, and nothing noticed for two releases. Exercising
// the read loop needs a live remote track, so this checks the branch's shape in
// the source instead: it must reach a `continue` and must not call Close().
func TestBadRTPIsDiscardedNotFatal(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "conn.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var branch *ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Init == nil {
			return true
		}
		if callsMethod(ifs.Init, "Unmarshal") {
			if branch != nil {
				t.Fatal("more than one Unmarshal error branch in conn.go; update this test")
			}
			branch = ifs.Body
		}
		return true
	})
	if branch == nil {
		t.Fatal("no `if err := packet.Unmarshal(...); err != nil` branch found in conn.go")
	}

	if callsMethod(branch, "Close") {
		t.Error("Unmarshal error branch calls Close(): one bad packet would tear down a healthy session")
	}
	hasContinue := false
	ast.Inspect(branch, func(n ast.Node) bool {
		if b, ok := n.(*ast.BranchStmt); ok && b.Tok == token.CONTINUE {
			hasContinue = true
		}
		return true
	})
	if !hasContinue {
		t.Error("Unmarshal error branch never continues: a bad packet would end the read loop")
	}
}

// callsMethod reports whether n contains a call of the form x.name(...).
func callsMethod(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}
