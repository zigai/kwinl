package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

var errIntrospectionFailed = errors.New("introspection failed")

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	oldStderr := os.Stderr

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}

	os.Stderr = w

	defer func() {
		os.Stderr = oldStderr
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("close stderr reader: %v", err)
	}

	return string(data)
}

func savePlaceFlags() func() {
	savedApp := placeAppFlag
	savedMatch := placeMatchFlag
	savedGeom := placeGeomFlag
	savedAnchor := placeAnchorFlag
	savedMonitor := placeMonitorFlag
	savedDesktop := placeDesktopFlag
	savedTimeout := placeTimeoutFlag
	savedCommand := placeCommandFlag
	savedKeep := placeKeepFlag
	savedCentered := placeCenteredFlag
	savedPinned := placePinnedFlag
	savedMinimized := placeMinimizedFlag
	savedKeepAbove := placeKeepAboveFlag
	savedKeepBelow := placeKeepBelowFlag

	return func() {
		placeAppFlag = savedApp
		placeMatchFlag = savedMatch
		placeGeomFlag = savedGeom
		placeAnchorFlag = savedAnchor
		placeMonitorFlag = savedMonitor
		placeDesktopFlag = savedDesktop
		placeTimeoutFlag = savedTimeout
		placeCommandFlag = savedCommand
		placeKeepFlag = savedKeep
		placeCenteredFlag = savedCentered
		placePinnedFlag = savedPinned
		placeMinimizedFlag = savedMinimized
		placeKeepAboveFlag = savedKeepAbove
		placeKeepBelowFlag = savedKeepBelow
	}
}

func saveWindowFlags() func() {
	savedID := windowIDFlag
	savedApp := windowAppFlag
	savedMatch := windowMatchFlag
	savedClass := windowClassFlag
	savedDesktop := windowDesktopFlag
	savedMonitor := windowMonitorFlag
	savedTimeout := windowTimeoutFlag
	savedStates := slices.Clone(windowStateFlags)
	savedLimit := windowLimitFlag
	savedAll := windowAllFlag
	savedJSON := windowJSONFlag
	savedAny := windowAnyFlag
	savedPos := windowPosFlag
	savedSize := windowSizeFlag
	savedGeom := windowGeomFlag

	return func() {
		windowIDFlag = savedID
		windowAppFlag = savedApp
		windowMatchFlag = savedMatch
		windowClassFlag = savedClass
		windowDesktopFlag = savedDesktop
		windowMonitorFlag = savedMonitor
		windowTimeoutFlag = savedTimeout
		windowStateFlags = savedStates
		windowLimitFlag = savedLimit
		windowAllFlag = savedAll
		windowJSONFlag = savedJSON
		windowAnyFlag = savedAny
		windowPosFlag = savedPos
		windowSizeFlag = savedSize
		windowGeomFlag = savedGeom
	}
}

func TestParseTemplateAcceptsNumericGeometryValuesInJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")

	data := `{
  "version": "1.0.0",
  "presets": [
    {
      "name": "demo",
      "app": "org.kde.konsole",
      "command": ["echo", "hi"],
      "geometry": {
        "x": 0,
        "y": 0,
        "width": 960,
        "height": 1080
      }
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write layout: %v", err)
	}

	template, err := parseTemplate(path)
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	if err := validateTemplate(template); err != nil {
		t.Fatalf("validate template: %v", err)
	}

	got := template.Presets[0].Geometry
	if got.X != "0" || got.Y != "0" || got.Width != "960" || got.Height != "1080" {
		t.Fatalf("unexpected geometry: %+v", got)
	}
}

func TestParseTemplateAcceptsStringGeometryValuesInJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")

	data := `{
  "version": "1.0.0",
  "presets": [
    {
      "name": "demo",
      "app": "org.kde.konsole",
      "command": ["echo", "hi"],
      "geometry": {
        "x": "0",
        "y": "0",
        "width": "50%",
        "height": "1080"
      }
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write layout: %v", err)
	}

	template, err := parseTemplate(path)
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	if err := validateTemplate(template); err != nil {
		t.Fatalf("validate template: %v", err)
	}

	got := template.Presets[0].Geometry
	if got.X != "0" || got.Y != "0" || got.Width != "50%" || got.Height != "1080" {
		t.Fatalf("unexpected geometry: %+v", got)
	}
}

func TestValidateTemplateAllowsCenteredPresetWithoutXY(t *testing.T) {
	t.Parallel()

	template := Template{
		Presets: []Preset{
			{
				Name:     "centered",
				App:      "org.kde.konsole",
				Command:  CommandSpec{"echo", "hi"},
				Centered: true,
				Geometry: PresetGeometry{Width: "800", Height: "600"},
			},
		},
	}

	if err := validateTemplate(template); err != nil {
		t.Fatalf("validate template: %v", err)
	}

	geom, err := resolveLaunchPresetGeometry(template.Presets[0], "")
	if err != nil {
		t.Fatalf("resolve geometry: %v", err)
	}

	if geom.W.Value != 800 || geom.H.Value != 600 {
		t.Fatalf("unexpected geometry: %+v", geom)
	}
}

func TestValidateTemplateRejectsCenteredPresetWithoutHeight(t *testing.T) {
	t.Parallel()

	template := Template{
		Presets: []Preset{
			{
				Name:     "centered",
				App:      "org.kde.konsole",
				Command:  CommandSpec{"echo", "hi"},
				Centered: true,
				Geometry: PresetGeometry{Width: "800"},
			},
		},
	}

	err := validateTemplate(template)
	if err == nil {
		t.Fatal("expected validation error")
	}

	if !strings.Contains(err.Error(), "width and height must both be set when centered is true") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTemplateStillRejectsNonCenteredPresetWithoutXY(t *testing.T) {
	t.Parallel()

	template := Template{
		Presets: []Preset{
			{
				Name:    "not-centered",
				App:     "org.kde.konsole",
				Command: CommandSpec{"echo", "hi"},
				Geometry: PresetGeometry{
					Width:  "800",
					Height: "600",
				},
			},
		},
	}

	err := validateTemplate(template)
	if err == nil {
		t.Fatal("expected validation error")
	}

	if !strings.Contains(err.Error(), "x, y, width, height must all be set together") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSplitCommandPreservesQuotedEmptyArgs(t *testing.T) {
	t.Parallel()

	got, err := splitCommand(`echo --flag "" value`)
	if err != nil {
		t.Fatalf("split command: %v", err)
	}

	want := []string{"echo", "--flag", "", "value"}
	if len(got) != len(want) {
		t.Fatalf("unexpected arg count: got %v want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected args: got %v want %v", got, want)
		}
	}
}

func TestCommandSpecUnmarshalJSONPreservesQuotedEmptyArgs(t *testing.T) {
	t.Parallel()

	var spec CommandSpec
	if err := json.Unmarshal([]byte(`"echo --flag \"\" value"`), &spec); err != nil {
		t.Fatalf("unmarshal command spec: %v", err)
	}

	want := []string{"echo", "--flag", "", "value"}
	if len(spec) != len(want) {
		t.Fatalf("unexpected arg count: got %v want %v", spec, want)
	}

	for i := range want {
		if spec[i] != want[i] {
			t.Fatalf("unexpected args: got %v want %v", spec, want)
		}
	}
}

func TestValidateTemplateRejectsEmptyCommandExecutable(t *testing.T) {
	t.Parallel()

	template := Template{
		Presets: []Preset{
			{
				Name:    "empty-executable",
				App:     "org.kde.konsole",
				Command: CommandSpec{""},
				Geometry: PresetGeometry{
					X:      "0",
					Y:      "0",
					Width:  "800",
					Height: "600",
				},
			},
		},
	}

	err := validateTemplate(template)
	if err == nil {
		t.Fatal("expected validation error")
	}

	if !strings.Contains(err.Error(), "executable must not be empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTemplateRejectsWhitespaceOnlyAppAndMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		preset Preset
	}{
		{
			name: "app",
			preset: Preset{
				Name:    "blank-app",
				App:     "   ",
				Command: CommandSpec{"echo", "hi"},
				Geometry: PresetGeometry{
					X:      "0",
					Y:      "0",
					Width:  "800",
					Height: "600",
				},
			},
		},
		{
			name: "match",
			preset: Preset{
				Name:    "blank-match",
				Match:   "   ",
				Command: CommandSpec{"echo", "hi"},
				Geometry: PresetGeometry{
					X:      "0",
					Y:      "0",
					Width:  "800",
					Height: "600",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateTemplate(Template{Presets: []Preset{tt.preset}})
			if err == nil {
				t.Fatal("expected validation error")
			}

			if !strings.Contains(err.Error(), "either app or match is required") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestFormatValidationFailureWritesToStderrAndSuppressesRootLogging(t *testing.T) {
	expected := &ValidationError{Field: "match", Value: "(", Message: "invalid regex: missing closing )"}

	var err error

	output := captureStderr(t, func() {
		err = formatValidationFailure("/tmp/demo.yaml", expected)
	})

	if !strings.Contains(output, "✗ demo.yaml validation failed:") {
		t.Fatalf("expected formatted validation header on stderr, got %q", output)
	}

	if !strings.Contains(output, expected.Error()) {
		t.Fatalf("expected validation error on stderr, got %q", output)
	}

	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T", err)
	}

	if !exitErr.Reported {
		t.Fatalf("expected validation failure to be marked as reported: %+v", exitErr)
	}

	if shouldLogError(err) {
		t.Fatalf("expected reported validation failure to skip root logging")
	}
}

func TestParseAndValidatePlaceAllowsCenteredGeomWithoutXY(t *testing.T) {
	restore := savePlaceFlags()
	defer restore()

	placeAppFlag = "org.kde.konsole"
	placeMatchFlag = ""
	placeGeomFlag = "800,600"
	placeAnchorFlag = "top-left"
	placeMonitorFlag = ""
	placeDesktopFlag = ""
	placeTimeoutFlag = "8s"
	placeCommandFlag = "echo hi"
	placeKeepFlag = false
	placeCenteredFlag = true
	placePinnedFlag = false
	placeMinimizedFlag = false
	placeKeepAboveFlag = false
	placeKeepBelowFlag = false

	cfg, err := parseAndValidatePlace()
	if err != nil {
		t.Fatalf("parseAndValidatePlace: %v", err)
	}
	defer os.RemoveAll(cfg.TempDir)

	if cfg.Anchor != "center" {
		t.Fatalf("unexpected anchor: %q", cfg.Anchor)
	}

	if !cfg.Geom.X.Percent || cfg.Geom.X.Value != 50 {
		t.Fatalf("unexpected x geometry: %+v", cfg.Geom.X)
	}

	if !cfg.Geom.Y.Percent || cfg.Geom.Y.Value != 50 {
		t.Fatalf("unexpected y geometry: %+v", cfg.Geom.Y)
	}

	if cfg.Geom.W.Value != 800 || cfg.Geom.H.Value != 600 {
		t.Fatalf("unexpected size geometry: %+v", cfg.Geom)
	}
}

func TestParseAndValidatePlaceRejectsConflictingKeepStackingFlags(t *testing.T) {
	restore := savePlaceFlags()
	defer restore()

	placeAppFlag = "org.kde.konsole"
	placeGeomFlag = "0,0,800,600"
	placeAnchorFlag = "top-left"
	placeTimeoutFlag = "8s"
	placeCommandFlag = "echo hi"
	placeKeepAboveFlag = true
	placeKeepBelowFlag = true

	_, err := parseAndValidatePlace()
	if err == nil {
		t.Fatal("expected validation error")
	}

	if !strings.Contains(err.Error(), "--keep-above and --keep-below cannot both be set") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseWindowSearchConfigAllowsEmptySelector(t *testing.T) {
	restore := saveWindowFlags()
	defer restore()

	windowIDFlag = ""
	windowAppFlag = ""
	windowMatchFlag = ""
	windowTimeoutFlag = "2s"
	windowJSONFlag = true

	cfg, err := parseWindowSearchConfig()
	if err != nil {
		t.Fatalf("parseWindowSearchConfig: %v", err)
	}
	defer os.RemoveAll(cfg.TempDir)

	if cfg.Selector.HasSelectors() {
		t.Fatalf("unexpected selector: %+v", cfg.Selector)
	}

	if !cfg.JSONOutput {
		t.Fatal("expected JSON output flag to be preserved")
	}
}

func TestParseWindowActionConfigRequiresSelector(t *testing.T) {
	restore := saveWindowFlags()
	defer restore()

	windowIDFlag = ""
	windowAppFlag = ""
	windowMatchFlag = ""
	windowTimeoutFlag = "2s"

	_, err := parseWindowActionConfig(windowActionRaise)
	if err == nil {
		t.Fatal("expected validation error")
	}

	if !strings.Contains(err.Error(), "at least one selector is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseWindowActionConfigAcceptsIDSelector(t *testing.T) {
	restore := saveWindowFlags()
	defer restore()

	windowIDFlag = "123"
	windowAppFlag = ""
	windowMatchFlag = ""
	windowTimeoutFlag = "2s"
	windowAllFlag = true

	cfg, err := parseWindowActionConfig(windowActionLower)
	if err != nil {
		t.Fatalf("parseWindowActionConfig: %v", err)
	}
	defer os.RemoveAll(cfg.TempDir)

	if cfg.Selector.ID != "123" || cfg.Action != windowActionLower || !cfg.All {
		t.Fatalf("unexpected action config: %+v", cfg)
	}
}

func TestParseWindowSearchConfigAcceptsRichSelectors(t *testing.T) {
	restore := saveWindowFlags()
	defer restore()

	windowClassFlag = "Navigator"
	windowDesktopFlag = "current"
	windowMonitorFlag = "DP-1"
	windowStateFlags = []string{"minimized", "keep-above", "minimized"}
	windowAnyFlag = true
	windowLimitFlag = 3
	windowTimeoutFlag = "2s"

	cfg, err := parseWindowSearchConfig()
	if err != nil {
		t.Fatalf("parseWindowSearchConfig: %v", err)
	}
	defer os.RemoveAll(cfg.TempDir)

	if cfg.Selector.Class != "Navigator" || cfg.Selector.Desktop != "current" || cfg.Selector.Monitor != "DP-1" {
		t.Fatalf("unexpected selector: %+v", cfg.Selector)
	}

	if !cfg.Selector.Any || cfg.Selector.Limit != 3 {
		t.Fatalf("unexpected any/limit selector: %+v", cfg.Selector)
	}

	if got := strings.Join(cfg.Selector.States, ","); got != "minimized,keep-above" {
		t.Fatalf("unexpected states: %q", got)
	}
}

func TestParseWindowActionConfigRejectsLimitWithoutAll(t *testing.T) {
	restore := saveWindowFlags()
	defer restore()

	windowIDFlag = "123"
	windowLimitFlag = 2
	windowTimeoutFlag = "2s"

	_, err := parseWindowActionConfig(windowActionRaise)
	if err == nil {
		t.Fatal("expected validation error")
	}

	if !strings.Contains(err.Error(), "requires --all") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseWindowGeometryConfigParsesMoveResizeAndSet(t *testing.T) {
	tests := []struct {
		name string
		mode windowGeometryMode
		set  func()
		want func(t *testing.T, cfg windowGeometryConfig)
	}{
		{
			name: "move",
			mode: windowGeometryModeMove,
			set: func() {
				windowPosFlag = "10%,20"
			},
			want: func(t *testing.T, cfg windowGeometryConfig) {
				t.Helper()

				if !cfg.Point.X.Percent || cfg.Point.X.Value != 10 || cfg.Point.Y.Value != 20 {
					t.Fatalf("unexpected point: %+v", cfg.Point)
				}
			},
		},
		{
			name: "resize",
			mode: windowGeometryModeResize,
			set: func() {
				windowSizeFlag = "50%,600"
			},
			want: func(t *testing.T, cfg windowGeometryConfig) {
				t.Helper()

				if !cfg.Size.W.Percent || cfg.Size.W.Value != 50 || cfg.Size.H.Value != 600 {
					t.Fatalf("unexpected size: %+v", cfg.Size)
				}
			},
		},
		{
			name: "set",
			mode: windowGeometryModeSet,
			set: func() {
				windowGeomFlag = "0,0,800,600"
			},
			want: func(t *testing.T, cfg windowGeometryConfig) {
				t.Helper()

				if cfg.Geom.W.Value != 800 || cfg.Geom.H.Value != 600 {
					t.Fatalf("unexpected geometry: %+v", cfg.Geom)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := saveWindowFlags()
			defer restore()

			windowIDFlag = "123"
			windowAllFlag = true
			windowLimitFlag = 1
			windowTimeoutFlag = "2s"

			tt.set()

			cfg, err := parseWindowGeometryConfig(tt.mode)
			if err != nil {
				t.Fatalf("parseWindowGeometryConfig: %v", err)
			}
			defer os.RemoveAll(cfg.TempDir)

			tt.want(t, cfg)
		})
	}
}

func TestValidateTemplateAllowsEmptyNonExecutableCommandArg(t *testing.T) {
	t.Parallel()

	template := Template{
		Presets: []Preset{
			{
				Name:    "empty-arg",
				App:     "org.kde.konsole",
				Command: CommandSpec{"echo", "", "--foo"},
				Geometry: PresetGeometry{
					X:      "0",
					Y:      "0",
					Width:  "800",
					Height: "600",
				},
			},
		},
	}

	if err := validateTemplate(template); err != nil {
		t.Fatalf("validate template: %v", err)
	}
}

func TestValidateTemplateRejectsConflictingKeepAboveAndKeepBelow(t *testing.T) {
	t.Parallel()

	template := Template{
		Presets: []Preset{
			{
				Name:      "stacking-conflict",
				App:       "org.kde.konsole",
				Command:   CommandSpec{"echo", "hi"},
				KeepAbove: true,
				KeepBelow: true,
				Geometry: PresetGeometry{
					X:      "0",
					Y:      "0",
					Width:  "800",
					Height: "600",
				},
			},
		},
	}

	err := validateTemplate(template)
	if err == nil {
		t.Fatal("expected validation error")
	}

	if !strings.Contains(err.Error(), "keepAbove and keepBelow cannot both be true") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTemplateRejectsFullscreenAndMaximized(t *testing.T) {
	t.Parallel()

	template := Template{
		Presets: []Preset{
			{
				Name:       "state-conflict",
				App:        "org.kde.konsole",
				Command:    CommandSpec{"echo", "hi"},
				FullScreen: true,
				Maximized:  "both",
				Geometry: PresetGeometry{
					X:      "0",
					Y:      "0",
					Width:  "800",
					Height: "600",
				},
			},
		},
	}

	err := validateTemplate(template)
	if err == nil {
		t.Fatal("expected validation error")
	}

	if !strings.Contains(err.Error(), "fullscreen cannot be combined with maximized") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNextLaunchPresetCallbackSkipsMismatchedTokens(t *testing.T) {
	t.Parallel()

	ch := make(chan placeResult, 2)
	ch <- placeResult{CallbackToken: "preset-1", Success: true, WindowID: "1"}

	ch <- placeResult{CallbackToken: "preset-2", Success: true, WindowID: "2"}

	result, ok := nextLaunchPresetCallback(ch, "preset-2", 50*time.Millisecond)
	if !ok {
		t.Fatal("expected matching callback")
	}

	if result.CallbackToken != "preset-2" || result.WindowID != "2" {
		t.Fatalf("unexpected callback: %+v", result)
	}
}

func TestNextLaunchPresetCallbackTimesOutOnOnlyStaleCallbacks(t *testing.T) {
	t.Parallel()

	ch := make(chan placeResult, 1)
	ch <- placeResult{CallbackToken: "preset-1", Success: true, WindowID: "1"}

	if _, ok := nextLaunchPresetCallback(ch, "preset-2", 20*time.Millisecond); ok {
		t.Fatal("expected timeout when only stale callbacks are available")
	}
}

func TestBuildLaunchPresetRunUsesScriptNameAsCallbackToken(t *testing.T) {
	t.Parallel()

	preset := Preset{
		Name:    "demo",
		App:     "org.kde.konsole",
		Command: CommandSpec{"echo", "hi"},
		Geometry: PresetGeometry{
			X:      "0",
			Y:      "0",
			Width:  "800",
			Height: "600",
		},
	}

	run, err := buildLaunchPresetRun(0, preset, t.TempDir(), "io.github.kwinl.Place.test")
	if err != nil {
		t.Fatalf("build launch preset run: %v", err)
	}

	if run.JSConfig.CallbackToken != run.ScriptName {
		t.Fatalf("unexpected callback token: got %q want %q", run.JSConfig.CallbackToken, run.ScriptName)
	}
}

func TestLaunchCommandReapsFastExitingChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process lifecycle behavior is only exercised on linux in this project")
	}

	cmd, err := launchCommand([]string{"sh", "-c", "exit 0"})
	if err != nil {
		t.Fatalf("launch command: %v", err)
	}

	waitForLinuxProcessReaped(t, cmd.Process.Pid, 500*time.Millisecond)
}

func TestCleanupStartedCommandsTerminatesStartedProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process lifecycle behavior is only exercised on linux in this project")
	}

	cmd, err := launchCommand([]string{"sh", "-c", "sleep 10"})
	if err != nil {
		t.Fatalf("launch command: %v", err)
	}

	cleanupStartedCommands([]*exec.Cmd{cmd})

	waitForLinuxProcessReaped(t, cmd.Process.Pid, 2*time.Second)
}

func waitForLinuxProcessReaped(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()

	procPath := "/proc/" + strconv.Itoa(pid)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(procPath); errors.Is(err, os.ErrNotExist) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("expected child process to be reaped, process still exists for pid %d", pid)
}

func TestValidateCleanupDiscoveryReturnsDBusErrorOnDiscoveryFailure(t *testing.T) {
	t.Parallel()

	scripts, err := validateCleanupDiscovery(nil, errIntrospectionFailed)
	if err == nil {
		t.Fatal("expected discovery error")
	}

	if scripts != nil {
		t.Fatalf("expected no scripts on error, got %v", scripts)
	}

	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T", err)
	}

	if exitErr.Code != exitCodeDBusFailure {
		t.Fatalf("unexpected exit code: got %d want %d", exitErr.Code, exitCodeDBusFailure)
	}
}

func TestValidateCleanupDiscoveryAllowsVerifiedEmptyResult(t *testing.T) {
	t.Parallel()

	scripts, err := validateCleanupDiscovery(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if scripts != nil {
		t.Fatalf("expected nil scripts, got %v", scripts)
	}
}

func TestMarshalTemplateYAMLPreservesNumericLookingMonitorAndDesktopAsStrings(t *testing.T) {
	t.Parallel()

	template := Template{
		Presets: []Preset{
			{
				Name:    "captured",
				App:     "org.kde.konsole",
				Command: CommandSpec{"echo", "hi"},
				Monitor: "1",
				Desktop: "1",
				Geometry: PresetGeometry{
					X:      "0",
					Y:      "0",
					Width:  "800",
					Height: "600",
				},
			},
		},
	}

	data, err := marshalTemplateYAML(template)
	if err != nil {
		t.Fatalf("marshal template YAML: %v", err)
	}

	out := string(data)
	if !strings.Contains(out, "monitor: \"1\"") {
		t.Fatalf("expected monitor to remain a quoted string, got:\n%s", out)
	}

	if !strings.Contains(out, "desktop: \"1\"") {
		t.Fatalf("expected desktop to remain a quoted string, got:\n%s", out)
	}

	if !strings.Contains(out, "x: 0") || !strings.Contains(out, "width: 800") {
		t.Fatalf("expected geometry scalars to remain numeric-looking YAML ints, got:\n%s", out)
	}
}

func TestWaitForPlacementReturnsUsageErrorOnFailedCallback(t *testing.T) {
	t.Parallel()

	ch := make(chan placeResult, 1)
	ch <- placeResult{Success: false, Message: "invalid monitor target: typo"}

	err := waitForPlacement(50*time.Millisecond, ch, nil)
	if err == nil {
		t.Fatal("expected placement error")
	}

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T", err)
	}

	if !strings.Contains(err.Error(), "invalid monitor target: typo") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForPlacementReturnsExitErrorOnTimeout(t *testing.T) {
	t.Parallel()

	err := waitForPlacement(20*time.Millisecond, make(chan placeResult), nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T", err)
	}

	if exitErr.Code != exitCodeLoadFailed {
		t.Fatalf("unexpected exit code: got %d want %d", exitErr.Code, exitCodeLoadFailed)
	}

	if !strings.Contains(err.Error(), "placement timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForLaunchPresetCallbackReturnsErrorOnTimeout(t *testing.T) {
	t.Parallel()

	err := waitForLaunchPresetCallback(make(chan placeResult), "demo", "token", 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}

	var presetErr *PresetError
	if !errors.As(err, &presetErr) {
		t.Fatalf("expected PresetError, got %T", err)
	}

	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected wrapped ExitError, got %T", err)
	}

	if exitErr.Code != exitCodeLoadFailed {
		t.Fatalf("unexpected exit code: got %d want %d", exitErr.Code, exitCodeLoadFailed)
	}

	if !strings.Contains(err.Error(), "placement timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForWindowActionReturnsReportedNoMatchExit(t *testing.T) {
	t.Parallel()

	ch := make(chan placeResult, 1)
	ch <- placeResult{Success: false, Message: "no matching window found"}

	err := waitForWindowAction(50*time.Millisecond, ch)
	if err == nil {
		t.Fatal("expected no-match error")
	}

	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T", err)
	}

	if exitErr.Code != exitCodeNoMatch {
		t.Fatalf("unexpected exit code: got %d want %d", exitErr.Code, exitCodeNoMatch)
	}

	if !exitErr.Reported {
		t.Fatal("expected no-match error to be marked as reported")
	}
}

func TestLaunchPresetWaitUsesProvidedTimeout(t *testing.T) {
	t.Parallel()

	want := 15 * time.Second
	if got := launchPresetWait(want); got != want {
		t.Fatalf("unexpected wait duration: got %s want %s", got, want)
	}
}

func TestWriteCaptureOutputReturnsStdoutWriteErrors(t *testing.T) {
	t.Parallel()

	tmp := filepath.Join(t.TempDir(), "stdout.txt")
	if err := os.WriteFile(tmp, []byte(""), 0o644); err != nil {
		t.Fatalf("seed stdout file: %v", err)
	}

	readOnly, err := os.Open(tmp)
	if err != nil {
		t.Fatalf("open read-only stdout file: %v", err)
	}
	defer readOnly.Close()

	oldStdout := os.Stdout
	os.Stdout = readOnly

	defer func() {
		os.Stdout = oldStdout
	}()

	err = writeCaptureOutput("-", []byte("payload"))
	if err == nil {
		t.Fatal("expected stdout write error")
	}

	if !strings.Contains(err.Error(), "failed to write stdout") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateJSAbortsOnInvalidTargetInsteadOfFallingBack(t *testing.T) {
	t.Parallel()

	js := generateJS(jsPlacementConfig{
		ScriptName:      "script",
		App:             "org.kde.konsole",
		Anchor:          "top-left",
		Geom:            ParsedGeometry{X: GeomValue{Value: 0}, Y: GeomValue{Value: 0}, W: GeomValue{Value: 800}, H: GeomValue{Value: 600}},
		Verbose:         false,
		CallbackService: "io.github.kwinl.Place.test",
		CallbackToken:   "script",
	})

	if !strings.Contains(js, `abortPlacement(targetResolutionError);`) {
		t.Fatalf("expected generated JS to abort on invalid target, got:\n%s", js)
	}

	if !strings.Contains(js, `invalid monitor target: `) || !strings.Contains(js, `invalid desktop target: `) {
		t.Fatalf("expected generated JS to include explicit invalid target errors, got:\n%s", js)
	}

	if strings.Contains(js, `falling back to first output index 0`) {
		t.Fatalf("expected generated JS to stop silent monitor fallback, got:\n%s", js)
	}
}

func TestGenerateWindowSearchJSSendsWindowPayload(t *testing.T) {
	t.Parallel()

	js := generateWindowSearchJS(jsWindowSearchConfig{
		ScriptName: "script",
		Selector: windowSelector{
			App:   "code",
			Match: `^/home/me/project - Visual Studio Code$`,
		},
		Verbose: false,
		Service: "io.github.kwinl.Capture.test",
	})

	if !strings.Contains(js, `JSON.stringify({ windows: found })`) {
		t.Fatalf("expected generated JS to send a window payload, got:\n%s", js)
	}

	if !strings.Contains(js, `appIds: ids`) || !strings.Contains(js, `caption: "" + (w.caption || "")`) {
		t.Fatalf("expected generated JS to include searchable window metadata, got:\n%s", js)
	}

	if !strings.Contains(js, `return selectorChecksMatch(checks);`) || !strings.Contains(js, `if (TARGET_ANY)`) {
		t.Fatalf("expected generated JS to require all provided selectors, got:\n%s", js)
	}
}

func TestGenerateWindowSearchJSSupportsRichSelectorsAndLimit(t *testing.T) {
	t.Parallel()

	js := generateWindowSearchJS(jsWindowSearchConfig{
		ScriptName: "script",
		Selector: windowSelector{
			Class:   "Navigator",
			Desktop: "current",
			Monitor: "DP-1",
			States:  []string{"minimized"},
			Any:     true,
			Limit:   2,
		},
		Verbose: false,
		Service: "io.github.kwinl.Capture.test",
	})

	for _, want := range []string{
		`var TARGET_CLASS = "Navigator";`,
		`var TARGET_DESKTOP = "current";`,
		`var TARGET_MONITOR = "DP-1";`,
		`var TARGET_STATES = ["minimized"];`,
		`var TARGET_ANY = true;`,
		`var TARGET_LIMIT = 2;`,
		`if (TARGET_LIMIT > 0 && found.length >= TARGET_LIMIT) break;`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("expected generated JS to contain %q, got:\n%s", want, js)
		}
	}
}

func TestGenerateWindowActionJSSupportsRaiseAndLower(t *testing.T) {
	t.Parallel()

	js := generateWindowActionJS(jsWindowActionConfig{
		ScriptName: "script",
		Selector:   windowSelector{ID: "123"},
		Action:     windowActionRaise,
		All:        false,
		Verbose:    false,
	})

	if !strings.Contains(js, `workspace.raiseWindow(w);`) {
		t.Fatalf("expected generated JS to raise windows, got:\n%s", js)
	}

	if !strings.Contains(js, `workspace.slotWindowLower();`) {
		t.Fatalf("expected generated JS to lower windows, got:\n%s", js)
	}

	if !strings.Contains(js, `case "keep-above":`) || !strings.Contains(js, `case "keep-below":`) {
		t.Fatalf("expected generated JS to support stacking state actions, got:\n%s", js)
	}

	if !strings.Contains(js, `if (!APPLY_ALL) break;`) {
		t.Fatalf("expected generated JS to default to one topmost match, got:\n%s", js)
	}
}

func TestGenerateWindowGeometryJSSetsFrameGeometry(t *testing.T) {
	t.Parallel()

	js := generateWindowGeometryJS(jsWindowGeometryConfig{
		ScriptName: "script",
		Selector:   windowSelector{ID: "123"},
		Mode:       windowGeometryModeSet,
		All:        true,
		Geom: ParsedGeometry{
			X: GeomValue{Value: 0},
			Y: GeomValue{Value: 0},
			W: GeomValue{Value: 800},
			H: GeomValue{Value: 600},
		},
		CallbackService: "io.github.kwinl.Place.test",
		CallbackToken:   "script",
	})

	if !strings.Contains(js, `w.frameGeometry = next;`) {
		t.Fatalf("expected generated JS to set frameGeometry, got:\n%s", js)
	}

	if !strings.Contains(js, `case "set-geometry":`) {
		t.Fatalf("expected generated JS to support set-geometry, got:\n%s", js)
	}
}

func TestGenerateDesktopAndMouseJSScriptsUseKWinAPIs(t *testing.T) {
	t.Parallel()

	desktopJS := generateDesktopQueryJS("script", "io.github.kwinl.Capture.test", "current", false)
	if !strings.Contains(desktopJS, `workspace.currentDesktop`) || !strings.Contains(desktopJS, `workspace.desktops`) {
		t.Fatalf("expected desktop query JS to inspect KWin desktops, got:\n%s", desktopJS)
	}

	setJS := generateDesktopSetJS("script", "io.github.kwinl.Place.test", "2", false)
	if !strings.Contains(setJS, `workspace.currentDesktop = desktop;`) {
		t.Fatalf("expected desktop set JS to assign currentDesktop, got:\n%s", setJS)
	}

	locationJS := generateMouseLocationJS("script", "io.github.kwinl.Capture.test", false)
	if !strings.Contains(locationJS, `workspace.cursorPos`) {
		t.Fatalf("expected mouse location JS to inspect cursorPos, got:\n%s", locationJS)
	}

	hoverJS := generateMouseHoveredWindowJS("script", "io.github.kwinl.Capture.test", true, false)
	if !strings.Contains(hoverJS, `workspace.windowAt(pos, count)`) {
		t.Fatalf("expected hovered-window JS to call windowAt, got:\n%s", hoverJS)
	}
}

func TestMouseInputCommandBuilders(t *testing.T) {
	t.Parallel()

	point := &mousePoint{X: 10, Y: 20}

	click := buildMouseClickCommands("ydotool", "left", 2, point)

	wantClick := []externalCommand{
		{Name: "ydotool", Args: []string{"mousemove", "--absolute", "10", "20"}},
		{Name: "ydotool", Args: []string{"click", "--repeat", "2", "0xC0"}},
	}
	if !reflect.DeepEqual(click, wantClick) {
		t.Fatalf("unexpected ydotool click commands: %+v", click)
	}

	scroll := buildMouseScrollCommands("xdotool", -3, point)

	wantScroll := []externalCommand{
		{Name: "xdotool", Args: []string{"mousemove", "10", "20"}},
		{Name: "xdotool", Args: []string{"click", "--repeat", "3", "4"}},
	}
	if !reflect.DeepEqual(scroll, wantScroll) {
		t.Fatalf("unexpected xdotool scroll commands: %+v", scroll)
	}
}

func TestWindowStateStringFormatsCommonStates(t *testing.T) {
	t.Parallel()

	got := windowStateString(windowInfo{Minimized: true, KeepAbove: true, FullScreen: true})
	if got != "minimized,keep-above,fullscreen" {
		t.Fatalf("unexpected state string: %q", got)
	}

	emptyState := windowStateString(windowInfo{})
	if emptyState != "-" {
		t.Fatalf("unexpected empty state string: %q", emptyState)
	}
}
