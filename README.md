# kwin-place

A CLI tool that launches a command and places its window at a specific geometry using KWin's scripting API.

## Features

- Loads a temporary KWin JavaScript script via session D-Bus
- Moves/resizes only newly-created windows (existing windows are not affected)
- Supports negative coordinates for multi-monitor setups
- Automatically unloads the script after timeout
- Batch launching via YAML/JSON templates

## Requirements

- KDE Plasma with KWin
- Session D-Bus access

## Installation

```bash
go install github.com/zigai/kwin-place@latest
```

Or build from source:

```bash
git clone https://github.com/zigai/kwin-place.git
cd kwin-place
go build -o kwin-place .
```

## Help

#### kwin-place place

```
Loads a temporary KWin script via D-Bus that intercepts newly created
windows matching the specified desktopFileName and moves/resizes them to the
requested geometry. Only windows created after the script loads are affected.

Geometry values can be absolute pixels (e.g., 100) or percentages (e.g., 50%).
Percentages are relative to the target monitor's dimensions.

Usage:
  kwin-place place --df <desktopFileName> --geom <x>,<y>,<w>,<h> --cmd "<command>" [--anchor <anchor>] [--monitor <id>] [--desktop <id>] [--timeout <duration>]

Examples:
  kwin-place place --df org.kde.konsole --geom 50,50,900,700 --timeout 8s --cmd "konsole --separate"
  kwin-place place --df org.kde.konsole --geom 0,0,50%,100% --anchor top-left --cmd "konsole"
  kwin-place place --df org.kde.konsole --geom 0,0,50%,100% --monitor 1 --desktop 2 --cmd "konsole"

Flags:
      --anchor string    anchor point for positioning (default "top-left")
      --cmd string       command to run (quoted string)
      --desktop string   target virtual desktop (1-based index or name)
      --df string        desktopFileName to match (required)
      --geom string      geometry as x,y,w,h (values can be pixels or percentages like 50%)
  -h, --help             help for place
      --monitor string   target monitor (index like 0, 1 or name like DP-1)
      --timeout string   timeout duration (e.g., 8s, 500ms) (default "8s")
```

#### kwin-place launch

```
Reads a template file containing multiple window presets and launches
all specified applications with their configured geometries.

Usage:
  kwin-place launch <config.yaml|config.json> [--timeout <duration>] [flags]

Examples:
  kwin-place launch layout.yaml
  kwin-place launch workspace.json --timeout 15s

Flags:
  -h, --help             help for launch
      --timeout string   timeout override (e.g., 10s)
```

### Template format (YAML/JSON)

```yaml
timeout: 8s
presets:
  - name: terminal
    df: org.kde.konsole
    command: "konsole --separate"
    geometry:
      x: 50
      y: 50
      width: 900
      height: 700
    anchor: top-left
    monitor: 0
    desktop: 1
```

`command` can be either a quoted string (split into args, no shell expansion) or an explicit array of strings.

## License

AGPL-3.0
