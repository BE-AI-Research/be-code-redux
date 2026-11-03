# Editor extensions

Prebuilt extension packages, committed here so they can be downloaded without
reading anything else. The same files are attached to every
[GitHub release](https://github.com/BE-AI-Research/be-code-redux/releases/latest).

| File | Editor | Install |
|---|---|---|
| `be-code-vscode-<version>.vsix` | VS Code | Extensions view → `...` menu → **Install from VSIX...** → pick the file |

The VS Code package is rebuilt from [`../vscode`](../vscode) by
`make -f build.mk vscode` (also part of `make -f build.mk release`), which refreshes the
copy here. Its version is the extension's own (`vscode/package.json`), not BE-Code's.
The Visual Studio extension (`../visualstudio`) is built on Windows only and is not
packaged here yet.
