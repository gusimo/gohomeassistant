# Third-party notices

GoHomeAssistant uses or ships the following software. Each keeps its own
license; the full texts are in the linked repositories.

| Component | Used as | License |
|---|---|---|
| [Go](https://go.dev) toolchain and standard library | compiler in the image, runtime | BSD-3-Clause |
| [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys) | Go dependency | BSD-3-Clause |
| [coder/websocket](https://github.com/coder/websocket) | Go dependency | ISC |
| [fsnotify](https://github.com/fsnotify/fsnotify) | Go dependency | BSD-3-Clause |
| [gopls](https://github.com/golang/tools) | binary in the image | BSD-3-Clause |
| [Delve](https://github.com/go-delve/delve) | binary in the image | MIT |
| [Monaco Editor](https://github.com/microsoft/monaco-editor) | bundled in the web UI | MIT |
| [@monaco-editor/react](https://github.com/suren-atoyan/monaco-react) | bundled in the web UI | MIT |
| [React, React DOM](https://github.com/facebook/react) | bundled in the web UI | MIT |
| [Vite](https://github.com/vitejs/vite), [TypeScript](https://github.com/microsoft/TypeScript) | build tools (not shipped) | MIT, Apache-2.0 |
| [git](https://git-scm.com) | separate program in the image (Alpine package) | GPL-2.0 |
| [tini](https://github.com/krallin/tini) | separate program in the image (Alpine package) | MIT |
| [Alpine Linux](https://alpinelinux.org) | base image | various, per package |
