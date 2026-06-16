# kwinl

Declarative window placement for KWin: launch programs into predefined geometries and batch-start full layouts from YAML/JSON templates.

## Features

- Place: launch a single app and move/resize its newly created window to a specific geometry
- Launch: apply a YAML/JSON layout to start and arrange multiple apps at once
- Windows: search existing windows and apply actions like activate/raise/lower/minimize
- Layouts: manage named layouts stored in `~/.config/kwinl`
- Capture: snapshot current windows into a reusable YAML/JSON layout template
- Validate: check a layout file for errors without launching anything
- Cleanup: unload orphaned kwinl scripts from KWin
- Completion: generate shell completions for bash/fish/powershell/zsh

## Requirements

- KDE Plasma with KWin
- Session D-Bus access

## Installation

### Go install

```bash
go install github.com/zigai/kwinl@latest
```

### Prebuilt binaries

Download release archives and Linux `.deb`/`.rpm` packages from the [GitHub Releases page](https://github.com/zigai/kwinl/releases/latest).

### Build from source

```bash
git clone https://github.com/zigai/kwinl.git
cd kwinl
go build -o kwinl .
```

## Help

#### kwinl place

```
Loads a temporary KWin script via D-Bus that intercepts newly created
windows matching the specified application ID or title pattern and moves/resizes
them to the requested geometry. Only windows created after the script loads are affected.

Geometry values can be absolute pixels (e.g., 100) or percentages (e.g., 50%).
Percentages are relative to the target monitor's dimensions.

Usage:
  kwinl place [--app <app-id>] [--match <regex>] --geom <x>,<y>,<w>,<h> --cmd "<command>" [flags]

Examples:
  kwinl place --app org.kde.konsole --geom 50,50,900,700 --timeout 8s --cmd "konsole --separate"
  kwinl place --app org.kde.konsole --geom 0,0,50%,100% --anchor top-left --cmd "konsole"
  kwinl place --match "^Firefox.*Private" --geom 0,0,50%,100% --cmd "firefox --private-window"
  kwinl place --app firefox --match "YouTube" --geom 0,0,50%,100% --cmd firefox
  kwinl place --app org.kde.konsole --geom 800,600 --centered --cmd konsole

Flags:
      --anchor string    anchor point for positioning (default "top-left")
  -a, --app string       application ID to match
      --centered         center window on monitor (sets x=50%, y=50%, anchor=center)
  -c, --cmd string       command to run (quoted string)
      --desktop string   target virtual desktop (1-based index or name)
  -g, --geom string      geometry as x,y,w,h (or w,h with --centered; values can be pixels or percentages like 50%)
  -h, --help             help for place
      --keep             keep script active and re-enforce geometry
      --keep-above       keep window above others
      --keep-below       keep window below others
  -m, --match string     regex pattern to match window title
      --minimized        start window minimized
      --monitor string   target monitor (index like 0, 1 or name like DP-1)
      --pinned           show window on all virtual desktops
  -t, --timeout string   timeout duration (e.g., 8s, 500ms) (default "8s")

Global Flags:
  -v, --verbose   verbose output

Note: At least one of --app or --match is required. When both are provided,
either can trigger a match (OR logic).
```

#### kwinl launch

```
Reads a template file containing multiple window presets and launches
all specified applications with their configured geometries.

Usage:
  kwinl launch [config.yaml|config.yml|config.json|-] [--timeout <duration>] [flags]

Examples:
  kwinl launch layout.yaml
  kwinl launch -
  cat layout.yaml | kwinl launch
  kwinl launch workspace.json --timeout 15s

Flags:
  -h, --help             help for launch
  -t, --timeout string   timeout override (e.g., 10s)

Global Flags:
  -v, --verbose   verbose output

Notes:
  - Use `-` to read a template from stdin.
  - With no positional argument, `launch` reads stdin when input is piped.
```

#### kwinl windows

```
Search and control existing KWin windows. Selectors are combined with AND logic:
when --id, --app, and --match are provided, all provided selectors must match
the same window.

Search examples:
  kwinl windows list
  kwinl windows list --app code
  kwinl windows list --match "Firefox"
  kwinl windows list --json

Action examples:
  kwinl windows activate --app code
  kwinl windows raise --id 123
  kwinl windows lower --match "Firefox"
  kwinl windows keep-above --app org.kde.konsole --all
  kwinl windows unset-keep-above --id 123
  kwinl windows minimize --match "Slack" --all

Available actions:
  activate, raise, lower, minimize, unminimize, toggle-minimize,
  keep-above, keep-below, unset-keep-above, unset-keep-below,
  toggle-keep-above, toggle-keep-below, clear-stacking, close

Search output columns:
  id, app, title, geometry, monitor, desktop, states

Action behavior:
  - Actions target the topmost matching window by default.
  - Use --all to apply an action to every matching window.
  - If no matching window exists, action commands exit with code 30.

The old "search" spelling remains available as an alias for "list".
```

#### kwinl layouts list

```
Lists saved layouts from ~/.config/kwinl.

Usage:
  kwinl layouts list

Behavior:
  - Creates ~/.config/kwinl if it does not exist
  - Prints one layout name per line
  - Names are shown without extension
  - If multiple files share the same basename, entries are disambiguated as:
      work (work.yaml)
      work (work.json)
```

#### kwinl layouts launch

```
Launches a saved layout by name from ~/.config/kwinl.

Usage:
  kwinl layouts launch <name> [--timeout <duration>]

Examples:
  kwinl layouts launch work
  kwinl layouts launch work.yaml --timeout 12s

Name resolution:
  - Basename: "work" resolves to work.yaml, work.yml, or work.json
  - Exact filename: "work.yaml" resolves that file directly
  - If basename is ambiguous (e.g., both work.yaml and work.json exist), command fails and asks for exact filename
```

#### kwinl layouts remove

```
Removes a saved layout by name from ~/.config/kwinl.

Usage:
  kwinl layouts remove <name>

Examples:
  kwinl layouts remove work
  kwinl layouts remove work.yaml

Name resolution rules are the same as "kwinl layouts launch".
```

#### kwinl capture

```
Captures the geometry/monitor/desktop of currently open windows and writes a YAML
or JSON template (based on output file extension) suitable for use with "kwinl launch".

By default, only windows with a known application ID are included. App IDs are read from
`desktopFileName` and fall back to `appId` when needed. Use --include-unknown
to also capture windows without an application ID (these will be matched by window title).

Maximized and fullscreen states are captured and will be restored when launching.

If --infer-command is enabled (default), each preset uses:
  command: ["gtk-launch", "<app-id>"]
This is a best-effort launcher and may not reproduce multi-window apps exactly.

If capture includes presets without `command`, it prints a warning. These presets must
be edited before `kwinl launch` can use the file.

Usage:
  kwinl capture [layout.yaml|layout.yml|layout.json|-] [flags]

Examples:
  kwinl capture
  kwinl capture -
  kwinl capture layout.yaml
  kwinl capture layout.json --include-unknown
  kwinl capture layout.yml --current-desktop
  kwinl capture layout.yaml --monitor DP-1

Notes:
  - If no output path is provided, output is written to stdout.
  - Use `-` explicitly to force stdout output.

Flags:
  -d, --current-desktop    only capture windows on current desktop
  -h, --help               help for capture
  -u, --include-unknown    include windows without desktopFileName/appId (matched by title; may require manual command)
      --infer-command      infer a best-effort launcher command using gtk-launch (default true)
  -M, --monitor string     only capture windows on specified monitor
  -t, --timeout string     capture timeout (e.g., 2s, 500ms) (default "2s")

Global Flags:
  -v, --verbose   verbose output
```

#### kwinl completion

```
Generate the autocompletion script for kwinl for the specified shell.
See each sub-command's help for details on how to use the generated script.

Usage:
  kwinl completion [command]

Available Commands:
  bash        Generate the autocompletion script for bash
  fish        Generate the autocompletion script for fish
  powershell  Generate the autocompletion script for powershell
  zsh         Generate the autocompletion script for zsh

Flags:
  -h, --help   help for completion

Global Flags:
  -v, --verbose   verbose output
```

#### kwinl validate

```
Validates a YAML/JSON layout file for syntax errors, missing fields,
and invalid values without launching any windows.

Usage:
  kwinl validate <layout-file> [flags]

Examples:
  kwinl validate layout.yaml
  kwinl validate workspace.json

Flags:
  -h, --help   help for validate

Global Flags:
  -v, --verbose   verbose output
```

#### kwinl cleanup

```
Discovers and unloads KWin scripts matching kwinl-* pattern.

Usage:
  kwinl cleanup [flags]

Examples:
  kwinl cleanup --dry-run
  kwinl cleanup

Flags:
  -n, --dry-run   list without unloading
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
| `geometry` | yes* | Window geometry with `x`, `y`, `width`, `height` |
| `tile` | no | Quick-tile mode: `left`, `right`, `top`, `bottom`, `top-left`, `top-right`, `bottom-left`, `bottom-right` |
| `anchor` | no | Anchor point for positioning (default: `top-left`) |
| `monitor` | no | Target monitor (index or name like `DP-1`) |
| `desktop` | no | Target virtual desktop (1-based index or name) |
| `maximized` | no | Maximize state: `horizontal`, `vertical`, or `both` |
| `fullscreen` | no | Set to `true` to make window fullscreen |
| `centered` | no | Set to `true` to center window (overrides x, y, anchor) |
| `pinned` | no | Set to `true` to show window on all virtual desktops |
| `minimized` | no | Set to `true` to start window minimized |
| `keepAbove` | no | Set to `true` to keep window above others (always on top) |
| `keepBelow` | no | Set to `true` to keep window below others |

\* `geometry` is required unless `tile` is set.
`tile` cannot be combined with `centered`, `maximized`, or `fullscreen`.

Quick-tiling example (supports shared split resizing in KWin):

```yaml
- name: left-pane
  app: com.mitchellh.ghostty
  command: [ghostty]
  tile: left
```

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
