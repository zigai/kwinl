package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	savedTimeout := windowTimeoutFlag
	savedAll := windowAllFlag
	savedJSON := windowJSONFlag

	return func() {
		windowIDFlag = savedID
		windowAppFlag = savedApp
		windowMatchFlag = savedMatch
		windowTimeoutFlag = savedTimeout
		windowAllFlag = savedAll
		windowJSONFlag = savedJSON
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

	if cfg.Selector != (windowSelector{}) {
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

	if !strings.Contains(err.Error(), "at least one of --id, --app, or --match is required") {
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

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("expected child process to be reaped, process state is still nil for pid %d", cmd.Process.Pid)
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("expected cleanup to terminate started child, process state is still nil for pid %d", cmd.Process.Pid)
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

	if !strings.Contains(js, `return idOK && appOK && matchOK;`) {
		t.Fatalf("expected generated JS to require all provided selectors, got:\n%s", js)
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
