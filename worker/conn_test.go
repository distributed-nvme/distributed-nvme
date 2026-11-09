package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDialAgentCarriesTheKeepalive pins RW7's keepalive on the worker's agent
// connections: dialAgent, the production dialer, passes
// common.GrpcKeepaliveDialOptions. The worker's own tests dial through fakes,
// so only this check sees the production dial.
func TestDialAgentCarriesTheKeepalive(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "conn.go", nil, 0)
	if err != nil {
		t.Fatalf("parse conn.go: %v", err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dialAgent" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "GrpcKeepaliveDialOptions" {
				found = true
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatalf("dialAgent does not pass common.GrpcKeepaliveDialOptions")
	}
}
