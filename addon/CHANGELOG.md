# Changelog

## 0.2.0

- Traces tab: every handler run with its trigger, log lines, service calls
  (data, duration, error) and panics; click a source position to open the
  line.
- Logs are kept on disk and survive restarts: filter by level, search, load
  older entries, see what is new since your last visit.
- Handler panics and failed service calls are logged as errors / warnings.
- Importing a new third-party package adds its module automatically.
- Code completion reconnects after the connection was lost (phone asleep,
  network change) and learns about new entities and files changed by git or
  other editors without a reload.
- Go to definition (F12, ⌘/Ctrl+click), also into the SDK, the generated
  entities and Go itself.
- The debugger reconnects after the connection was lost; a paused program
  stays paused.
- Projects reload all states after Home Assistant was restarted, so they
  don't act on old states.
- Fix: a handler that calls services while many events arrive no longer
  stalls for 30 seconds.

## 0.1.3

- Phone layout: projects and files in a ☰ drawer, all editor buttons
  reachable, Code / Logs / Debug / History as tabs at the bottom. While
  debugging, the code is shown above the debug panel.
- Fix: no browser error when a file is opened while code completion is still
  connecting, or when debug mode is switched quickly.

## 0.1.2

- Fix: required service parameters are always sent, even when zero
  (`SetCoverPosition(p.CoverSetCoverPosition{Position: 0})` closed nothing).

## 0.1.1

- Snippets: built-in and your own, in a panel and in editor completion.
- Login sessions survive restarts; auto save in the editor.
- Uses the `app_config` mapping (no more legacy warning in HA 2026.2+).

## 0.1.0

First add-on release: projects with hot reload, typed entities (`ent`),
web IDE with gopls, entity browser, debugger (Delve), local git.
