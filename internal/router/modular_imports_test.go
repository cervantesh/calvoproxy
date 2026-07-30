package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouterDoesNotImportCervoRulesRootFacade(t *testing.T) {
	rootImport := `"github.com/cervantesh/` + `cervo-rules/v3"`
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(source), rootImport) {
			t.Fatalf("%s imports the CervoRules root facade; use v3 modular core/runtime packages", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
