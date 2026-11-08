// Example using the generated, typed entities.
package main

import (
	"log"

	"github.com/gusimo/gohomeassistant/ent"
	ha "github.com/gusimo/gohomeassistant/sdk"
)

func main() {
	ha.MustConnect()

	sun := ent.Sun.Sun
	log.Printf("sun is %s, elevation %v", sun.Value(), sun.Attr("elevation"))

	sun.OnChange(func(ch ha.StateChange) {
		if ch.Old != nil && ch.New != nil && ch.Old.State != ch.New.State {
			log.Printf("sun: %s -> %s", ch.Old.State, ch.New.State)
			// e.g. ent.Light.Porch.TurnOn(ent.LightTurnOn{BrightnessPct: 60})
		}
	})

	ha.Default().Run()
}
