package server

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gusimo/gohomeassistant/internal/codegen"
)

var (
	placeholder = regexp.MustCompile(`\$\{(\d+):([^}]*)\}`)
	tabstop     = regexp.MustCompile(`\$(\d+)`)
)

// expand fills in a snippet the way accepting all defaults would.
func expand(body string) string {
	defaults := map[string]string{}
	for _, m := range placeholder.FindAllStringSubmatch(body, -1) {
		defaults[m[1]] = m[2]
	}
	body = placeholder.ReplaceAllString(body, "$2")
	body = tabstop.ReplaceAllStringFunc(body, func(s string) string { return defaults[s[1:]] })
	return strings.ReplaceAll(body, `\$`, "$")
}

// TestBuiltinSnippetsBuild: every built-in snippet of the UI, inserted with
// its defaults and imports, must compile against generated entities.
func TestBuiltinSnippetsBuild(t *testing.T) {
	b, err := os.ReadFile("../../web/src/snippets.json")
	if err != nil {
		t.Fatal(err)
	}
	var snippets []Snippet
	if err := json.Unmarshal(b, &snippets); err != nil {
		t.Fatal(err)
	}
	var states []codegen.State
	var services codegen.Services
	json.Unmarshal([]byte(`[{"entity_id":"light.kitchen","attributes":{}},{"entity_id":"binary_sensor.motion","attributes":{}},{"entity_id":"sensor.temperature","attributes":{}}]`), &states)
	json.Unmarshal([]byte(`{"light":{"turn_on":{"fields":{"brightness_pct":{"selector":{"number":{}}}},"target":{"entity":[{"domain":["light"]}]}}}}`), &services)
	files, _, err := codegen.Generate(states, services)
	if err != nil {
		t.Fatal(err)
	}
	sdk, _ := filepath.Abs("../../sdk")
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
	write("go.work", []byte("go 1.24\n\nuse (\n\t./app\n\t./ent\n\t"+sdk+"\n)\n"))

	for i, sn := range snippets {
		body := expand(sn.Body)
		imports := map[string]bool{`ha "gohomeassistant.local/sdk"`: true}
		if strings.Contains(body, "ent.") {
			imports[`"gohomeassistant.local/ent"`] = true
		}
		for _, imp := range sn.Imports {
			if name, path, ok := strings.Cut(imp, " "); ok {
				imports[name+` "`+path+`"`] = true
			} else {
				imports[`"`+imp+`"`] = true
			}
		}
		var src strings.Builder
		src.WriteString("package main\n\nimport (\n")
		for imp := range imports {
			src.WriteString("\t" + imp + "\n")
		}
		src.WriteString(")\n\n")
		if strings.Contains(body, "func main()") {
			src.WriteString(body + "\n")
		} else {
			src.WriteString("func main() {\n\tha.MustConnect()\n" + body + "\n\tha.Default().Run()\n}\n")
		}
		write(fmt.Sprintf("app/s%02d/main.go", i), []byte(src.String()))
	}
	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = filepath.Join(dir, "app")
	cmd.Env = append(os.Environ(), "GOWORK="+filepath.Join(dir, "go.work"))
	if out, err := cmd.CombinedOutput(); err != nil {
		// Map package dirs back to snippet names.
		msg := string(out)
		for i, sn := range snippets {
			msg = strings.ReplaceAll(msg, fmt.Sprintf("s%02d/", i), fmt.Sprintf("[%s] ", sn.Name))
		}
		t.Fatalf("built-in snippets do not build: %v\n%s", err, msg)
	}
}
