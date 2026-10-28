package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Static proofs over every non-test Go file the server is built from: what a
// log line may carry (IP-13 DoD 4, D6) and that nothing writes a file
// (IP-13 DoD 8, D13). The dynamic log tests prove the lines a workload
// reaches; these prove the call sites it does not.

// serverFile is one parsed non-test file of cmd/quotacore or internal/.
type serverFile struct {
	path string
	file *ast.File
}

func serverFiles(t *testing.T) []serverFile {
	t.Helper()
	var out []serverFile
	for _, root := range []string{".", filepath.Join("..", "..", "internal")} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			out = append(out, serverFile{path: filepath.ToSlash(path), file: f})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 50 {
		t.Fatalf("read %d server files; the walk is not reading the tree", len(out))
	}
	return out
}

// allowedLogKeys are the fields a log line may carry: observability.md
// section 2's, and the background and diagnostic fields in use. A new key is
// added here deliberately, after checking it carries nothing section 2.1
// forbids.
var allowedLogKeys = map[string]bool{
	// observability.md section 2.
	"ts": true, "level": true, "request_id": true, "endpoint": true, "status": true, "duration_ms": true,
	"outcome": true, "code": true, "key_prefix": true, "tenant_id": true, "feature_key": true, "amount": true,
	"balance": true, "limit": true, "cycle_index": true, "interval": true, "replayed": true,
	"rolled_over": true, "error": true, "job": true, "actor": true, "action": true, "subject_type": true,
	// What the background jobs and the startup report.
	"addr": true, "listener": true, "grace": true, "ready": true, "unsatisfied": true, "drained": true,
	"pending": true, "events": true, "dropped": true, "event_id": true, "event_type": true,
	"events_deleted": true, "audit_deleted": true, "dead_letters_replayed": true, "reversed": true,
	"referred": true, "verified": true, "findings": true, "kind": true, "retry_in": true, "detail": true,
	"refund": true, "policy": true,
}

// forbiddenLogKeys are what section 2.1 and DR-043 keep out of every line,
// named so a reviewer cannot add one to the allow-list by accident.
var forbiddenLogKeys = map[string]bool{
	"external_id": true, "tenant_external_id": true, "display_name": true, "metadata": true, "body": true,
	"authorization": true, "token": true, "secret": true, "idempotency_key": true, "password": true,
	"database_url": true, "redis_url": true, "url": true,
}

var logMethods = map[string]bool{"Debug": true, "Info": true, "Warn": true, "Error": true, "With": true}
var attrFuncs = map[string]bool{"String": true, "Int": true, "Int64": true, "Uint64": true, "Float64": true,
	"Bool": true, "Time": true, "Duration": true, "Any": true, "Group": true}

// logKeys returns every literal key a file hands a logger, by position.
func logKeys(sf serverFile) (keys map[string][]string, problems []string) {
	keys = map[string][]string{}
	ast.Inspect(sf.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// An identifier the parser did not resolve to a declaration in the
		// file is a package name.
		pkg, isPkg := sel.X.(*ast.Ident)
		isPkg = isPkg && pkg.Obj == nil
		switch {
		case isPkg && pkg.Name == "slog" && attrFuncs[sel.Sel.Name]:
			if k, ok := literal(call.Args, 0); ok {
				keys[k] = append(keys[k], sf.path)
			}
		case isPkg && pkg.Name == "slog" && (logMethods[sel.Sel.Name] || sel.Sel.Name == "Log" || sel.Sel.Name == "SetDefault"):
			problems = append(problems, sf.path+": calls slog."+sel.Sel.Name+", the package-level logger, instead of the one it was given")
		case !isPkg && logMethods[sel.Sel.Name] && len(call.Args) > 0 && isLogger(sel.X):
			first := 1
			if sel.Sel.Name == "With" {
				first = 0
			} else if _, ok := literal(call.Args, 0); !ok {
				if _, concat := call.Args[0].(*ast.BinaryExpr); !concat {
					return true // not a log call: its first argument is not a message
				}
			}
			for i := first; i < len(call.Args); i += 2 {
				if k, ok := literal(call.Args, i); ok {
					keys[k] = append(keys[k], sf.path)
				}
			}
		}
		return true
	})
	return keys, problems
}

// isLogger reports a receiver named as a logger is named throughout the
// code (log, s.log, o.Logger, readyLog), which is what tells a logger's With
// from an error envelope's (envelope.Error(...).With), whose details the
// contract defines.
func isLogger(x ast.Expr) bool {
	switch v := x.(type) {
	case *ast.Ident:
		return strings.Contains(strings.ToLower(v.Name), "log")
	case *ast.SelectorExpr:
		return strings.Contains(strings.ToLower(v.Sel.Name), "log")
	case *ast.ParenExpr:
		return isLogger(v.X)
	case *ast.StarExpr:
		return isLogger(v.X)
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "With" {
			return isLogger(sel.X)
		}
	}
	return false
}

func literal(args []ast.Expr, i int) (string, bool) {
	if i >= len(args) {
		return "", false
	}
	lit, ok := args[i].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// TestEveryLogCallUsesAnAllowedKey (IP-13 DoD 4, D6): every key any server
// call site hands a logger is on the allow-list and none is forbidden; no
// code uses slog's package-level logger; and only main and the local
// commands write to stdout or stderr directly.
func TestEveryLogCallUsesAnAllowedKey(t *testing.T) {
	used := map[string][]string{}
	for _, sf := range serverFiles(t) {
		keys, problems := logKeys(sf)
		for _, p := range problems {
			t.Error(p)
		}
		for k, at := range keys {
			used[k] = append(used[k], at...)
		}
		base := filepath.Base(sf.path)
		inMain := !strings.Contains(sf.path, "internal/") && (base == "main.go" || base == "commands.go")
		ast.Inspect(sf.file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" && (sel.Sel.Name == "Stdout" || sel.Sel.Name == "Stderr") && !inMain {
				t.Errorf("%s writes to os.%s; a line belongs in the logger it was given", sf.path, sel.Sel.Name)
			}
			return true
		})
	}
	var names []string
	for k := range used {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch {
		case forbiddenLogKeys[k]:
			t.Errorf("log key %q is forbidden (observability.md section 2.1, DR-043), at %v", k, used[k])
		case !allowedLogKeys[k]:
			t.Errorf("log key %q is not on the allow-list, at %v", k, used[k])
		}
	}
	for k := range allowedLogKeys {
		if used[k] == nil {
			t.Errorf("log key %q is allowed and no call site uses it; take it off the list", k)
		}
	}
	if len(used) < 15 {
		t.Errorf("found %d log keys; the walk is not reading the call sites", len(used))
	}
}

// fileWrites are the os functions that create, change or remove a file.
var fileWrites = map[string]bool{
	"Create": true, "CreateTemp": true, "WriteFile": true, "OpenFile": true, "Mkdir": true, "MkdirAll": true,
	"MkdirTemp": true, "Remove": true, "RemoveAll": true, "Rename": true, "Chmod": true, "Chown": true,
	"Lchown": true, "Chtimes": true, "Symlink": true, "Link": true, "Truncate": true,
}

// TestTheServerWritesNoFile (IP-13 DoD 8, D13): no server code path
// creates, writes, renames or removes a file, so the process runs on a
// read-only filesystem. The local commands' --report is the one writer, and
// it is not the server.
func TestTheServerWritesNoFile(t *testing.T) {
	for _, sf := range serverFiles(t) {
		if strings.HasSuffix(sf.path, "/commands.go") || sf.path == "commands.go" {
			continue
		}
		ast.Inspect(sf.file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "os" || id.Name == "ioutil") && fileWrites[sel.Sel.Name] {
				t.Errorf("%s calls %s.%s; the server writes no file (NFR-S6)", sf.path, id.Name, sel.Sel.Name)
			}
			return true
		})
	}
}
