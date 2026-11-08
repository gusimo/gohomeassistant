package main

import (
	"log"

	ha "gohomeassistant.local/sdk"
)

func main() {
	c := ha.MustConnect()
	log.Printf("hello: %d entities known", len(c.States()))

	c.OnStateChange("", func(ch ha.StateChange) {
		if ch.Old != nil && ch.New != nil && ch.Old.State != ch.New.State {
			log.Printf("%s: %s -> %s", ch.EntityID, ch.Old.State, ch.New.State)
		}
	})

	c.Run()
}
