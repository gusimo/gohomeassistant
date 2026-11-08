package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gusimo/gohomeassistant/internal/codegen"
)

// TestTemplateBuilds: a new project must build right away, both before the
// first entity generation (stub) and with generated entities.
func TestTemplateBuilds(t *testing.T) {
	var states []codegen.State
	json.Unmarshal([]byte(`[{"entity_id":"light.kitchen","attributes":{}}]`), &states)
	generated, _, err := codegen.Generate(states, codegen.Services{})
	if err != nil {
		t.Fatal(err)
	}
	sdk, _ := filepath.Abs("../../sdk")
	for name, files := range map[string]codegen.Files{
		"stub":      {Ent: codegen.Stub(), Params: codegen.StubParams()},
		"generated": generated,
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := filepath.EvalSymlinks(t.TempDir())
			write := func(p string, b []byte) {
				os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
				if err := os.WriteFile(filepath.Join(dir, p), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("ent/go.mod", []byte("module "+codegen.ModulePath+"\n\ngo 1.24\n"))
			write("ent/ent.go", files.Ent)
			write("ent/p/p.go", files.Params)
			write("app/go.mod", []byte("module app\n\ngo 1.24\n"))
			write("app/main.go", []byte(MainTemplate))
			write("go.work", []byte("go 1.24\n\nuse (\n\t./app\n\t./ent\n\t"+sdk+"\n)\n"))
			cmd := exec.Command("go", "vet", ".")
			cmd.Dir = filepath.Join(dir, "app")
			cmd.Env = append(os.Environ(), "GOWORK="+filepath.Join(dir, "go.work"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("template does not build: %v\n%s", err, out)
			}
		})
	}
}
