package codegen

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const statesJSON = `[
 {"entity_id": "light.kitchen", "attributes": {"friendly_name": "Kitchen Ceiling"}},
 {"entity_id": "light.0x00158d00086f44ec", "attributes": {}},
 {"entity_id": "sensor.power", "attributes": {"unit_of_measurement": "W"}},
 {"entity_id": "sensor.power_", "attributes": {}},
 {"entity_id": "media_player.living_room", "attributes": {}}
]`

const servicesJSON = `{
 "light": {
  "turn_on": {
   "fields": {
    "brightness_pct": {"selector": {"number": {"min": 0, "max": 100, "unit_of_measurement": "%"}}},
    "rgb_color": {"selector": {"color_rgb": {}}},
    "flash": {"selector": {"select": {"options": ["short", "long"]}}},
    "additional_fields": {"collapsed": true, "fields": {
      "rgbw_color": {"selector": {"object": {}}},
      "kelvin": {"selector": {"color_temp": {"unit": "kelvin"}}}
    }}
   },
   "target": {"entity": [{"domain": ["light"]}]}
  },
  "state": {"fields": {}, "target": {"entity": [{"domain": ["light"]}]}}
 },
 "sonos": {
  "join": {"fields": {"master": {"required": true, "selector": {"entity": {}}}},
           "target": {"entity": [{"integration": "sonos", "domain": ["media_player"]}]}}
 },
 "script": {"recalibrate": {"fields": {}}},
 "notify": {"mobile_app_phone": {"fields": {
   "message": {"required": true, "selector": {"text": {}}},
   "data": {"selector": {"object": {}}}
 }}}
}`

func generateFixture(t *testing.T) string {
	t.Helper()
	var states []State
	var services Services
	if err := json.Unmarshal([]byte(statesJSON), &states); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(servicesJSON), &services); err != nil {
		t.Fatal(err)
	}
	src, _, err := Generate(states, services)
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

func TestRefs(t *testing.T) {
	var states []State
	json.Unmarshal([]byte(statesJSON), &states)
	_, refs, err := Generate(states, Services{})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"light.kitchen":            "ent.Light.Kitchen",
		"sensor.power_":            "ent.Sensor.Power_2",
		"light.0x00158d00086f44ec": "ent.Light.X0x00158d00086f44ec",
		"media_player.living_room": "ent.MediaPlayer.LivingRoom",
	} {
		if refs[id] != want {
			t.Errorf("%s: got %q, want %q", id, refs[id], want)
		}
	}
}

func TestGenerateAPI(t *testing.T) {
	src := generateFixture(t)
	for _, want := range []string{
		"Kitchen LightEntity",             // entity field
		"X0x00158d00086f44ec LightEntity", // leading digit
		"Power_2 SensorEntity",            // name collision power / power_
		"func (e LightEntity) TurnOn(p ...LightTurnOn) error",
		"func (e LightEntity) StateService(",                         // clashes with ha.Entity.State
		"func (e MediaPlayerEntity) SonosJoin(p ...SonosJoin) error", // cross-domain target
		"BrightnessPct float64",
		"RgbColor []int",
		"RgbwColor any", // from collapsed section
		"Kelvin float64",
		"Flash string",
		`one of: "short", "long"`,
		"func (svcScript) Recalibrate(p ...ScriptRecalibrate) error", // no target
		"func (svcLight) TurnOn(target ha.Target, p ...LightTurnOn) error",
		"unit W",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
}

// TestGeneratedCodeCompiles builds a program using the generated package
// against the real SDK.
func TestGeneratedCodeCompiles(t *testing.T) {
	src := generateFixture(t)
	dir, err := filepath.EvalSymlinks(t.TempDir()) // go.work needs real paths
	if err != nil {
		t.Fatal(err)
	}
	sdk, _ := filepath.Abs("../../sdk")
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ent/go.mod", "module "+ModulePath+"\n\ngo 1.24\n")
	write("ent/ent.go", src)
	write("app/go.mod", "module app\n\ngo 1.24\n")
	write("app/main.go", `package main

import (
	ha "gohomeassistant.local/sdk"
	"gohomeassistant.local/ent"
)

func main() {
	_ = ent.Light.Kitchen.TurnOn(ent.LightTurnOn{BrightnessPct: 50, RgbColor: []int{255, 0, 0}})
	_ = ent.Light.Kitchen.TurnOn()
	_ = ent.MediaPlayer.LivingRoom.SonosJoin(ent.SonosJoin{Master: "media_player.kitchen"})
	_ = ent.Svc.Light.TurnOn(ha.Target{AreaID: []string{"kitchen"}})
	_ = ent.Svc.Notify.MobileAppPhone(ent.NotifyMobileAppPhone{Message: "hi", Extra: map[string]any{"x": 1}})
	_, _ = ent.Sensor.Power.Float()
	_ = ent.Light.Kitchen.IsOn()
}
`)
	write("go.work", "go 1.24\n\nuse (\n\t./app\n\t./ent\n\t"+sdk+"\n)\n")
	cmd := exec.Command("go", "vet", ".")
	cmd.Dir = filepath.Join(dir, "app")
	cmd.Env = append(os.Environ(), "GOWORK="+filepath.Join(dir, "go.work"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated code does not compile: %v\n%s", err, out)
	}
}

func TestDataOmitsZeroValues(t *testing.T) {
	src := generateFixture(t)
	if !strings.Contains(src, `if p.BrightnessPct != 0 {`) || !strings.Contains(src, `if len(p.RgbColor) > 0 {`) {
		t.Error("zero checks missing")
	}
}
