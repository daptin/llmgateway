package llmgateway

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestModuleHasNoHostOrMeteringPolicyOwnership(t *testing.T) {
	forbiddenImports := []string{
		"github.com/daptin/daptin", "github.com/gin-gonic/gin", "github.com/jmoiron/sqlx",
		"github.com/doug-martin/goqu", "github.com/buraksezer/olric", "database/sql",
	}
	forbiddenSymbols := map[string]bool{
		"Limit": true, "Policy": true, "PolicyBinding": true, "PolicyBindings": true, "LimitReservations": true,
		"ParsePolicy": true, "BuildReservations": true, "metricAmount": true, "windowBounds": true,
	}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			for _, forbidden := range forbiddenImports {
				if value == forbidden || strings.HasPrefix(value, forbidden+"/") {
					t.Errorf("%s imports forbidden host dependency %q", path, value)
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.TypeSpec:
				if forbiddenSymbols[typed.Name.Name] {
					t.Errorf("%s declares host-owned metering type %s", path, typed.Name.Name)
				}
			case *ast.FuncDecl:
				if forbiddenSymbols[typed.Name.Name] {
					t.Errorf("%s declares host-owned metering function %s", path, typed.Name.Name)
				}
			case *ast.Field:
				for _, name := range typed.Names {
					if forbiddenSymbols[name.Name] {
						t.Errorf("%s declares host-owned metering field %s", path, name.Name)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
