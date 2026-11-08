# Changelog

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
