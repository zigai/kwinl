# kwin-layout

Declarative window placement for KWin: launch programs into predefined geometries and batch-start full layouts from YAML/JSON templates.

## Features

- Place: launch a single app and move/resize its newly created window to a specific geometry
- Launch: apply a YAML/JSON layout to start and arrange multiple apps at once
- Capture: snapshot current windows into a reusable YAML/JSON layout template
- Validate: check a layout file for errors without launching anything
- Cleanup: unload orphaned kwin-layout scripts from KWin

## Requirements

- KDE Plasma with KWin
- Session D-Bus access

## Installation

```bash
go install github.com/zigai/kwin-layout@latest
```

Or build from source:

```bash
git clone https://github.com/zigai/kwin-layout.git
cd kwin-layout
go build -o kwin-layout .
```

## Help

#### kwin-layout place

```
Loads a temporary KWin script via D-Bus that intercepts newly created
windows matching the specified application ID or title pattern and moves/resizes
them to the requested geometry. Only windows created after the script loads are affected.

Geometry values can be absolute pixels (e.g., 100) or percentages (e.g., 50%).
Percentages are relative to the target monitor's dimensions.

Usage:
  kwin-layout place [--app <app-id>] [--match <regex>] --geom <x>,<y>,<w>,<h> --cmd "<command>" [flags]

Examples:
  kwin-layout place --app org.kde.konsole --geom 50,50,900,700 --timeout 8s --cmd "konsole --separate"
  kwin-layout place --app org.kde.konsole --geom 0,0,50%,100% --anchor top-left --cmd "konsole"
  kwin-layout place --match "^Firefox.*Private" --geom 0,0,50%,100% --cmd "firefox --private-window"
  kwin-layout place --app firefox --match "YouTube" --geom 0,0,50%,100% --cmd firefox
  kwin-layout place --app org.kde.konsole --geom 0,0,800,600 --centered --cmd konsole

Flags:
      --anchor string    anchor point for positioning (default "top-left")
      --app string       application ID to match
      --centered         center window on monitor (sets x=50%, y=50%, anchor=center)
      --cmd string       command to run (quoted string)
      --desktop string   target virtual desktop (1-based index or name)
      --geom string      geometry as x,y,w,h (values can be pixels or percentages like 50%)
  -h, --help             help for place
      --keep             keep script active and re-enforce geometry
      --match string     regex pattern to match window title
      --monitor string   target monitor (index like 0, 1 or name like DP-1)
      --timeout string   timeout duration (e.g., 8s, 500ms) (default "8s")

Global Flags:
  -v, --verbose   verbose output

Note: At least one of --app or --match is required. When both are provided,
either can trigger a match (OR logic).
```

#### kwin-layout launch

```
Reads a template file containing multiple window presets and launches
all specified applications with their configured geometries.

Usage:
  kwin-layout launch <config.yaml|config.json> [--timeout <duration>] [flags]

Examples:
  kwin-layout launch layout.yaml
  kwin-layout launch workspace.json --timeout 15s

Flags:
  -h, --help             help for launch
      --timeout string   timeout override (e.g., 10s)

Global Flags:
  -v, --verbose   verbose output
```

#### kwin-layout capture

```
Captures the geometry/monitor/desktop of currently open windows and writes a YAML
or JSON template (based on output file extension) suitable for use with "kwin-layout launch".

By default, only windows with a non-empty desktopFileName are included. Use --include-unknown
to also capture windows without desktopFileName (these will be matched by window title).

Maximized and fullscreen states are captured and will be restored when launching.

If --infer-command is enabled (default), each preset uses:
  command: ["gtk-launch", "<desktopFileName>"]
This is a best-effort launcher and may not reproduce multi-window apps exactly.

Usage:
  kwin-layout capture <layout.yaml|layout.yml|layout.json|-> [flags]

Examples:
  kwin-layout capture layout.yaml
  kwin-layout capture layout.json --include-unknown
  kwin-layout capture layout.yml --current-desktop
  kwin-layout capture layout.yaml --monitor DP-1

Flags:
      --current-desktop    only capture windows on current desktop
  -h, --help               help for capture
      --include-unknown    include windows without desktopFileName (matched by title)
      --infer-command      infer a best-effort launcher command using gtk-launch (default true)
      --monitor string     only capture windows on specified monitor
      --timeout string     capture timeout (e.g., 2s, 500ms) (default "2s")

Global Flags:
  -v, --verbose   verbose output
```

#### kwin-layout validate

```
Validates a YAML/JSON layout file for syntax errors, missing fields,
and invalid values without launching any windows.

Usage:
  kwin-layout validate <layout-file> [flags]

Examples:
  kwin-layout validate layout.yaml
  kwin-layout validate workspace.json

Flags:
  -h, --help   help for validate

Global Flags:
  -v, --verbose   verbose output
```

#### kwin-layout cleanup

```
Discovers and unloads KWin scripts matching kwin-layout-* pattern.

Usage:
  kwin-layout cleanup [flags]

Examples:
  kwin-layout cleanup --dry-run
  kwin-layout cleanup

Flags:
      --dry-run   list without unloading
  -h, --help      help for cleanup

Global Flags:
  -v, --verbose   verbose output
```

### Template format (YAML/JSON)

```yaml
version: 1.0.0
timeout: 8s
presets:
  - name: konsole-1
    app: org.kde.konsole
    command: [gtk-launch, org.kde.konsole]
    geometry:
      x: 0
      y: 0
      width: 960
      height: 1080
    anchor: top-left
    monitor: DP-1
    desktop: Desktop 1
    maximized: horizontal
```

#### Preset fields

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Preset identifier |
| `app` | one of app/match | Application ID to match (e.g., `org.kde.konsole`) |
| `match` | one of app/match | Regex pattern to match window title (e.g., `^Firefox$`) |
| `command` | yes | Command to launch (array recommended, string accepted) |
| `geometry` | yes | Window geometry with `x`, `y`, `width`, `height` |
| `anchor` | no | Anchor point for positioning (default: `top-left`) |
| `monitor` | no | Target monitor (index or name like `DP-1`) |
| `desktop` | no | Target virtual desktop (1-based index or name) |
| `maximized` | no | Maximize state: `horizontal`, `vertical`, or `both` |
| `fullscreen` | no | Set to `true` to make window fullscreen |
| `centered` | no | Set to `true` to center window (overrides x, y, anchor) |

#### Command specification

The recommended format is an explicit array of strings:

```yaml
command: ["konsole", "--separate", "-e", "htop"]
```

A scalar string is also accepted for convenience:

```yaml
command: "konsole --separate -e htop"
```

Environment variables are expanded in command arguments using `${VAR}` or `$VAR` syntax:

```yaml
command: ["${HOME}/scripts/my-app.sh", "--config", "$XDG_CONFIG_HOME/app.conf"]
```

Windows can be matched by either `app` (application ID) or `match` (title regex). At least one must be specified. When both are present, either can trigger a match.

## License

AGPL-3.0
