# BE-Code Redux

## Overview
BE-Code Redux is an offline-first "polymorphic" runtime and coding CLI for local LLMs. It combines the BE-CLI Go architecture, the Claude Code tool-loop model, and the Codex headless/pipeline style, rebuilt around the premise that the model is small, local, and fallible, so the harness must supply the quality.

## Build
```bash
make build
```

## Test
```bash
make test
```

## Run
```bash
./be-code
```

## Directory Layout
- `cmd/`: Entry points and CLI commands
- `docs/`: Documentation and guides
- `internal/`: Core functionality and modules
- `scripts/`: Scripts for various tasks
- `test/`: Unit and integration tests
- `visualstudio/`: Visual Studio project files
- `vscode/`: VS Code project files

## Key Files
- `LICENSE`: License information
- `README.md`: Project documentation
- `build.mk`: Makefile for build targets
- `go.mod`: Go module configuration

## Conventions
- Uses Go as the primary language
- Follows Go module conventions
- Uses Make targets for build, test, and clean
- Has a clear directory structure with `internal/` for core logic
- Uses `cmd/` for CLI entry points

## Gotchas
- The project uses a mix of Go, .dll, .pdb, and other file types
- Some directories like `visualstudio/` and `vscode/` contain IDE-specific files
- The project has a large number of test files, many in `internal/`
- The `internal/` directory contains a lot of subdirectories with specific functionality
- The project has a complex build and test setup with multiple targets
- The `README.md` contains installation instructions and project overview
- The project has a large number of source files and lines of code
