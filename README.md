# kwin-layout

A CLI tool that launches a command and places its window at a specific geometry using KWin's scripting API.

## Features

- Place: launch a single app and move/resize its newly created window to a specific geometry
- Launch: apply a YAML/JSON layout to start and arrange multiple apps at once
- Capture: snapshot current windows into a reusable YAML/JSON layout template

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
windows matching the specified desktopFileName and moves/resizes them to the
requested geometry. Only windows created after the script loads are affected.

Geometry values can be absolute pixels (e.g., 100) or percentages (e.g., 50%).
Percentages are relative to the target monitor's dimensions.

Usage:
  kwin-layout place --df <desktopFileName> --geom <x>,<y>,<w>,<h> --cmd "<command>" [--anchor <anchor>] [--monitor <id>] [--desktop <id>] [--timeout <duration>]

Examples:
  kwin-layout place --df org.kde.konsole --geom 50,50,900,700 --timeout 8s --cmd "konsole --separate"
  kwin-layout place --df org.kde.konsole --geom 0,0,50%,100% --anchor top-left --cmd "konsole"
  kwin-layout place --df org.kde.konsole --geom 0,0,50%,100% --monitor 1 --desktop 2 --cmd "konsole"

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

#### kwin-layout launch

```
kwin-layout capture --help
Captures the geometry/monitor/desktop of currently open windows and writes a YAML
or JSON template (based on output file extension) suitable for use with "kwin-layout launch".

Only windows with a non-empty desktopFileName are included. Geometry is recorded relative
to the window's output (monitor) origin, and anchor is set to top-left.

If --infer-command is enabled (default), each preset uses:
  command: ["gtk-launch", "<desktopFileName>"]
This is a best-effort launcher and may not reproduce multi-window apps exactly.

Usage:
  kwin-layout capture <layout.yaml|layout.yml|layout.json|-> [--timeout <duration>] [--infer-command] [flags]

Examples:
  kwin-layout capture layout.yaml
  kwin-layout capture layout.json
  kwin-layout capture - --timeout 2s
  kwin-layout capture layout.yml --infer-command=false

Flags:
  -h, --help             help for capture
      --infer-command    infer a best-effort launcher command using gtk-launch (default true)
      --timeout string   capture timeout (e.g., 2s, 500ms) (default "2s")
```

### Template format (YAML/JSON)

```yaml
version: 1.0.0
timeout: 8s
presets:
  - name: terminal
    df: org.kde.konsole
    command: [konsole, --separate]
    geometry:
      x: 50
      y: 50
      width: 900
      height: 700
    anchor: top-left
    monitor: 0
    desktop: 1
```

```json
{
  "version": "1.0.0",
  "timeout": "8s",
  "presets": [
    {
      "name": "terminal",
      "df": "org.kde.konsole",
      "command": ["konsole", "--separate"],
      "geometry": {
        "x": 50,
        "y": 50,
        "width": 900,
        "height": 700
      },
      "anchor": "top-left",
      "monitor": "0",
      "desktop": "1"
    }
  ]
}
```

`command` can be either a quoted string (split into args, no shell expansion) or an explicit array of strings. Capture emits the array form.

## License

AGPL-3.0
