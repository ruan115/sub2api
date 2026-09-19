package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

var allowedCreate = regexp.MustCompile(`^CREATE TABLE IF NOT EXISTS (runtime_outbox_commit_lock|runtime_outbox|runtime_outbox_consumers)\s*\(`)

const commitLockInsert = "INSERT IGNORE INTO runtime_outbox_commit_lock (singleton, lock_epoch) VALUES (1, 0)"

// extractCCMAXSchema copies only four literal statements from the named source
// function, preserving the repository's DDL rather than maintaining a second
// simplified schema. No source is executed and no business migrations are run.
func extractCCMAXSchema(source []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "execution_outbox.go", source, 0)
	if err != nil {
		return nil, fmt.Errorf("parse CCMAX schema source: %w", err)
	}
	var function *ast.FuncDecl
	for _, declaration := range file.Decls {
		candidate, ok := declaration.(*ast.FuncDecl)
		if ok && candidate.Recv == nil && candidate.Name.Name == "mysqlExecutionSchema" {
			if function != nil {
				return nil, fmt.Errorf("duplicate mysqlExecutionSchema function")
			}
			function = candidate
		}
	}
	if function == nil || function.Body == nil || len(function.Body.List) != 1 {
		return nil, fmt.Errorf("mysqlExecutionSchema must contain one literal return")
	}
	returned, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return nil, fmt.Errorf("mysqlExecutionSchema must return one literal slice")
	}
	literal, ok := returned.Results[0].(*ast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("mysqlExecutionSchema must return a literal slice")
	}
	arrayType, ok := literal.Type.(*ast.ArrayType)
	if !ok || arrayType.Len != nil {
		return nil, fmt.Errorf("mysqlExecutionSchema must return []string")
	}
	elementType, ok := arrayType.Elt.(*ast.Ident)
	if !ok || elementType.Name != "string" {
		return nil, fmt.Errorf("mysqlExecutionSchema must return []string")
	}
	wanted := map[string]bool{
		"runtime_outbox_commit_lock": false,
		"runtime_outbox":             false,
		"runtime_outbox_consumers":   false,
		"commit_lock_seed":           false,
	}
	var result []string
	for _, element := range literal.Elts {
		value, ok := element.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return nil, fmt.Errorf("CCMAX schema contains a non-literal statement")
		}
		statement, err := strconv.Unquote(value.Value)
		if err != nil {
			return nil, fmt.Errorf("decode CCMAX statement: %w", err)
		}
		statement = strings.TrimSpace(statement)
		var key string
		if match := allowedCreate.FindStringSubmatch(statement); match != nil {
			key = match[1]
		} else if strings.Join(strings.Fields(statement), " ") == commitLockInsert {
			key = "commit_lock_seed"
		} else {
			continue
		}
		// The selected repository literals contain no statement separators or
		// comments. Reject appended commands instead of executing a SQL batch.
		if strings.Contains(statement, ";") || strings.Contains(statement, "--") || strings.Contains(statement, "/*") {
			return nil, fmt.Errorf("selected CCMAX schema statement contains a separator or comment")
		}
		if wanted[key] {
			return nil, fmt.Errorf("duplicate selected CCMAX schema statement: %s", key)
		}
		wanted[key] = true
		result = append(result, statement+";")
	}
	for _, key := range []string{"runtime_outbox_commit_lock", "commit_lock_seed", "runtime_outbox", "runtime_outbox_consumers"} {
		if !wanted[key] {
			return nil, fmt.Errorf("required CCMAX schema statement missing: %s", key)
		}
	}
	return result, nil
}
