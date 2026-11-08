# GoHomeAssistant

Open **GoHomeAssistant** in the sidebar (administrators only). Create a
project with **+ new project**. Every project is a small Go program that
reacts to your entities:

```go
ent.BinarySensor.FrontDoor.OnChange(func(ch ha.StateChange) {
	if ent.BinarySensor.FrontDoor.IsOn() {
		ent.Light.Hall.TurnOn(p.LightTurnOn{BrightnessPct: 80})
	}
})
```

Save (Ctrl+S / ⌘S) and the project is rebuilt and restarted within a
second. Neither Home Assistant nor the add-on restarts. If your change doesn't
compile, the previous version keeps running.

## Where your projects are

In the add-on's config folder, `addon_configs/<id>_gohomeassistant/projects/`,
which you can reach with the Samba, SSH or Studio Code Server add-ons.
Projects are included in backups; build caches are not. Every project is a
git repository (see the *Changes* section and the *History* tab).

## Features

- `gohomeassistant.local/ent`: typed entities and services, generated from
  your instance (service parameters in `gohomeassistant.local/ent/p`). It is
  regenerated whenever entities or services change.
- Code completion, hover docs, auto-imports and gofmt on save (gopls).
- **Entities** panel: search, live state, insert as code.
- **Debug** tab: breakpoints, stepping, variables (Delve).
- **Logs** tab: output of all projects.

## Notes

- The add-on requests the `SYS_PTRACE` capability for the debugger.
- The first start and the first build of a project take a bit longer,
  because Go caches are filled.
