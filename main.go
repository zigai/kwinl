package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	dbusDestination    = "org.kde.KWin"
	dbusScriptingPath  = "/Scripting"
	dbusScriptingIface = "org.kde.kwin.Scripting"
	dbusScriptIface    = "org.kde.kwin.Script"
	scriptObjectPrefix = "/Scripting/Script"
	defaultTimeoutSec  = 8

	exitCodeInternal     = 1
	exitCodeUsage        = 2
	exitCodeDBusFailure  = 10
	exitCodeLoadFailed   = 11
	exitCodeLaunchFailed = 20
	exitCodeInterrupted  = 130
	exitCodeTerminated   = 143

	captureObjectPath = "/io/github/kwinlayout/Capture"
	captureIface      = "io.github.kwinlayout.Capture"

	placeObjectPath = "/io/github/kwinlayout/Place"
	placeIface      = "io.github.kwinlayout.Place"
)

var validAnchors = []string{
	"top-left", "top-center", "top-right",
	"center-left", "center", "center-right",
	"bottom-left", "bottom-center", "bottom-right",
}

type GeomValue struct {
	Value   int
	Percent bool
}

type ParsedGeometry struct {
	X GeomValue
	Y GeomValue
	W GeomValue
	H GeomValue
}

type Config struct {
	App        string
	Match      string
	Geom       ParsedGeometry
	Anchor     string
	Monitor    string
	Desktop    string
	Pinned     bool
	Minimized  bool
	KeepAbove  bool
	KeepBelow  bool
	Timeout    time.Duration
	Cmd        []string
	ScriptName string
	ScriptPath string
	TempDir    string
	JSFile     string
}

type PresetGeometry struct {
	X      string `json:"x" yaml:"x"`
	Y      string `json:"y" yaml:"y"`
	Width  string `json:"width" yaml:"width"`
	Height string `json:"height" yaml:"height"`
}

type Preset struct {
	Name       string         `json:"name" yaml:"name"`
	App        string         `json:"app,omitempty" yaml:"app,omitempty"`
	Match      string         `json:"match,omitempty" yaml:"match,omitempty"`
	Command    CommandSpec    `json:"command" yaml:"command"`
	Geometry   PresetGeometry `json:"geometry" yaml:"geometry"`
	Tile       string         `json:"tile,omitempty" yaml:"tile,omitempty"`
	Anchor     string         `json:"anchor,omitempty" yaml:"anchor,omitempty"`
	Monitor    string         `json:"monitor,omitempty" yaml:"monitor,omitempty"`
	Desktop    string         `json:"desktop,omitempty" yaml:"desktop,omitempty"`
	Maximized  string         `json:"maximized,omitempty" yaml:"maximized,omitempty"`
	FullScreen bool           `json:"fullscreen,omitempty" yaml:"fullscreen,omitempty"`
	Centered   bool           `json:"centered,omitempty" yaml:"centered,omitempty"`
	Pinned     bool           `json:"pinned,omitempty" yaml:"pinned,omitempty"`
	Minimized  bool           `json:"minimized,omitempty" yaml:"minimized,omitempty"`
	KeepAbove  bool           `json:"keepAbove,omitempty" yaml:"keepAbove,omitempty"`
	KeepBelow  bool           `json:"keepBelow,omitempty" yaml:"keepBelow,omitempty"`
}

type Template struct {
	Version string   `json:"version,omitempty" yaml:"version,omitempty"`
	Timeout string   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Presets []Preset `json:"presets" yaml:"presets"`
}

type CommandSpec []string

func (c *CommandSpec) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*c = nil
		return nil
	}

	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		parsed, err := splitCommand(asString)
		if err != nil {
			return fmt.Errorf("invalid command: %w", err)
		}
		*c = CommandSpec(parsed)
		return nil
	}

	var asSlice []string
	if err := json.Unmarshal(data, &asSlice); err != nil {
		return fmt.Errorf("invalid command: expected string or string array")
	}
	*c = CommandSpec(asSlice)
	return nil
}

func (c *CommandSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		parsed, err := splitCommand(value.Value)
		if err != nil {
			return fmt.Errorf("invalid command: %w", err)
		}
		*c = CommandSpec(parsed)
		return nil
	case yaml.SequenceNode:
		var asSlice []string
		if err := value.Decode(&asSlice); err != nil {
			return fmt.Errorf("invalid command: expected string array")
		}
		*c = CommandSpec(asSlice)
		return nil
	case yaml.DocumentNode:
		if len(value.Content) != 1 {
			return fmt.Errorf("invalid command: expected string or string array")
		}
		return c.UnmarshalYAML(value.Content[0])
	case yaml.MappingNode, yaml.AliasNode:
		return fmt.Errorf("invalid command: expected string or string array")
	case 0:
		*c = nil
		return nil
	default:
		return fmt.Errorf("invalid command: expected string or string array")
	}
}

type ValidationError struct {
	Field   string
	Value   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Value != "" {
		return fmt.Sprintf("invalid %s %q: %s", e.Field, e.Value, e.Message)
	}
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Message)
}

type GeometryError struct {
	Component string
	Value     string
	Reason    string
}

func (e *GeometryError) Error() string {
	if e.Value != "" {
		return fmt.Sprintf("geometry %s: invalid value %q: %s", e.Component, e.Value, e.Reason)
	}
	return fmt.Sprintf("geometry %s: %s", e.Component, e.Reason)
}

type TemplateError struct {
	Path   string
	Reason string
	Err    error
}

func (e *TemplateError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("template %q: %s: %v", e.Path, e.Reason, e.Err)
	}
	return fmt.Sprintf("template %q: %s", e.Path, e.Reason)
}

func (e *TemplateError) Unwrap() error {
	return e.Err
}

type PresetError struct {
	Preset string
	Field  string
	Err    error
}

func (e *PresetError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("preset %s: %s: %v", e.Preset, e.Field, e.Err)
	}
	if e.Err != nil {
		return fmt.Sprintf("preset %s: %v", e.Preset, e.Err)
	}
	return fmt.Sprintf("preset %s: unknown error", e.Preset)
}

func (e *PresetError) Unwrap() error {
	return e.Err
}

type CommandError struct {
	Reason string
}

func (e *CommandError) Error() string {
	return e.Reason
}

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit code %d", e.Code)
}

func (e *ExitError) Unwrap() error {
	return e.Err
}

type ScriptPathError struct {
	Reason string
}

func (e *ScriptPathError) Error() string {
	return fmt.Sprintf("unexpected loadScript return: %s", e.Reason)
}

var (
	version = "1.0.0"

	verboseFlag bool

	placeAppFlag       string
	placeMatchFlag     string
	placeGeomFlag      string
	placeAnchorFlag    string
	placeMonitorFlag   string
	placeDesktopFlag   string
	placeTimeoutFlag   string
	placeCommandFlag   string
	placeKeepFlag      bool
	placeCenteredFlag  bool
	placePinnedFlag    bool
	placeMinimizedFlag bool
	placeKeepAboveFlag bool
	placeKeepBelowFlag bool
	launchTimeoutFlag  string

	captureTimeoutFlag      string
	captureInferCommandFlag bool
	captureIncludeUnknown   bool
	captureCurrentDesktop   bool
	captureMonitorFilter    string
)

type captureOptions struct {
	InferCommand   bool
	IncludeUnknown bool
	CurrentDesktop bool
	MonitorFilter  string
}

var rootCmd = &cobra.Command{
	Use:   "kwin-layout",
	Short: "KWin window placement tool",
	Long: `kwin-layout loads temporary KWin scripts via D-Bus that intercept newly created
windows and move/resize them to the requested geometry.`,
	Version: version,
}

var placeCmd = &cobra.Command{
	Use:   "place [--app <app-id>] [--match <regex>] --geom <x>,<y>,<w>,<h> --cmd \"<command>\" [flags]",
	Short: "Launch a command and place its window at a specific geometry",
	Long: `Loads a temporary KWin script via D-Bus that intercepts newly created
windows matching the specified application ID or title pattern and moves/resizes
them to the requested geometry. Only windows created after the script loads are affected.

Geometry values can be absolute pixels (e.g., 100) or percentages (e.g., 50%).
Percentages are relative to the target monitor's dimensions.

At least one of --app or --match is required. When both are provided, either can
trigger a match (OR logic).`,
	Example: `  kwin-layout place --app org.kde.konsole --geom 50,50,900,700 --timeout 8s --cmd "konsole --separate"
  kwin-layout place --app org.kde.konsole --geom 0,0,50%,100% --anchor top-left --cmd "konsole"
  kwin-layout place --match "^Firefox.*Private" --geom 0,0,50%,100% --cmd "firefox --private-window"
  kwin-layout place --app firefox --match "YouTube" --geom 0,0,50%,100% --cmd firefox`,
	DisableFlagsInUseLine: true,
	SilenceUsage:          true,
	SilenceErrors:         true,
	Args:                  cobra.NoArgs,
	RunE:                  runPlace,
}

var launchCmd = &cobra.Command{
	Use:   "launch <config.yaml|config.json> [--timeout <duration>]",
	Short: "Batch launch windows from a YAML/JSON template",
	Long: `Reads a template file containing multiple window presets and launches
all specified applications with their configured geometries.`,
	Example: `  kwin-layout launch layout.yaml
  kwin-layout launch workspace.json --timeout 15s`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLaunch,
}

var captureCmd = &cobra.Command{
	Use:   "capture <layout.yaml|layout.yml|layout.json|-> [flags]",
	Short: "Capture currently open windows into a layout template",
	Long: `Captures the geometry/monitor/desktop of currently open windows and writes a YAML
or JSON template (based on output file extension) suitable for use with "kwin-layout launch".

By default, only windows with a known application ID are included. Use --include-unknown
to also capture windows without an application ID (these will be matched by window title).

Maximized and fullscreen states are captured and will be restored when launching.

If --infer-command is enabled (default), each preset uses:
  command: ["gtk-launch", "<app-id>"]
This is a best-effort launcher and may not reproduce multi-window apps exactly.`,
	Example: `  kwin-layout capture layout.yaml
  kwin-layout capture layout.json --include-unknown
  kwin-layout capture layout.yml --current-desktop
  kwin-layout capture layout.yaml --monitor DP-1`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCapture,
}

var validateCmd = &cobra.Command{
	Use:   "validate <layout-file>",
	Short: "Validate a layout template without launching",
	Long:  `Validates a YAML/JSON layout file for syntax errors, missing fields, and invalid values without launching any windows.`,
	Example: `  kwin-layout validate layout.yaml
  kwin-layout validate workspace.json`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runValidate,
}

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Unload orphaned kwin-layout scripts",
	Long:  `Discovers and unloads KWin scripts matching kwin-layout-* pattern.`,
	Example: `  kwin-layout cleanup --dry-run
  kwin-layout cleanup`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCleanup,
}

var cleanupDryRunFlag bool

//nolint:gochecknoinits // cobra setup uses init for flag wiring.
func init() {
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.PersistentFlags().BoolVarP(&verboseFlag, "verbose", "v", false, "verbose output")

	placeCmd.Flags().StringVarP(&placeAppFlag, "app", "a", "", "application ID to match")
	placeCmd.Flags().StringVarP(&placeMatchFlag, "match", "m", "", "regex pattern to match window title")
	placeCmd.Flags().StringVarP(&placeGeomFlag, "geom", "g", "", "geometry as x,y,w,h (values can be pixels or percentages like 50%)")
	placeCmd.Flags().StringVar(&placeAnchorFlag, "anchor", "top-left", "anchor point for positioning")
	placeCmd.Flags().StringVar(&placeMonitorFlag, "monitor", "", "target monitor (index like 0, 1 or name like DP-1)")
	placeCmd.Flags().StringVar(&placeDesktopFlag, "desktop", "", "target virtual desktop (1-based index or name)")
	placeCmd.Flags().StringVarP(&placeTimeoutFlag, "timeout", "t", "8s", "timeout duration (e.g., 8s, 500ms)")
	placeCmd.Flags().StringVarP(&placeCommandFlag, "cmd", "c", "", "command to run (quoted string)")
	placeCmd.Flags().BoolVar(&placeKeepFlag, "keep", false, "keep script active and re-enforce geometry")
	placeCmd.Flags().BoolVar(&placeCenteredFlag, "centered", false, "center window on monitor (sets x=50%, y=50%, anchor=center)")
	placeCmd.Flags().BoolVar(&placePinnedFlag, "pinned", false, "show window on all virtual desktops")
	placeCmd.Flags().BoolVar(&placeMinimizedFlag, "minimized", false, "start window minimized")
	placeCmd.Flags().BoolVar(&placeKeepAboveFlag, "keep-above", false, "keep window above others")
	placeCmd.Flags().BoolVar(&placeKeepBelowFlag, "keep-below", false, "keep window below others")
	must(placeCmd.MarkFlagRequired("geom"))
	must(placeCmd.MarkFlagRequired("cmd"))

	launchCmd.Flags().StringVarP(&launchTimeoutFlag, "timeout", "t", "", "timeout override (e.g., 10s)")

	captureCmd.Flags().StringVarP(&captureTimeoutFlag, "timeout", "t", "2s", "capture timeout (e.g., 2s, 500ms)")
	captureCmd.Flags().BoolVar(&captureInferCommandFlag, "infer-command", true, "infer a best-effort launcher command using gtk-launch")
	captureCmd.Flags().BoolVarP(&captureIncludeUnknown, "include-unknown", "u", false, "include windows without desktopFileName (matched by title)")
	captureCmd.Flags().BoolVarP(&captureCurrentDesktop, "current-desktop", "d", false, "only capture windows on current desktop")
	captureCmd.Flags().StringVarP(&captureMonitorFilter, "monitor", "M", "", "only capture windows on specified monitor")

	cleanupCmd.Flags().BoolVarP(&cleanupDryRunFlag, "dry-run", "n", false, "list without unloading")

	rootCmd.AddCommand(placeCmd)
	rootCmd.AddCommand(launchCmd)
	rootCmd.AddCommand(captureCmd)
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(cleanupCmd)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func verbose(format string, args ...any) {
	if verboseFlag {
		fmt.Fprintf(os.Stderr, "[verbose] "+format+"\n", args...)
	}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("kwin-layout: ")
	if err := rootCmd.Execute(); err != nil {
		log.Println(err)
		os.Exit(exitCodeFor(err))
	}
}

func exitCodeFor(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if isUsageError(err) {
		return exitCodeUsage
	}
	return exitCodeInternal
}

func isUsageError(err error) bool {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return true
	}
	var ge *GeometryError
	if errors.As(err, &ge) {
		return true
	}
	var te *TemplateError
	if errors.As(err, &te) {
		return true
	}
	var pe *PresetError
	if errors.As(err, &pe) {
		return true
	}
	var ce *CommandError
	return errors.As(err, &ce)
}

func isValidAnchor(anchor string) bool {
	return slices.Contains(validAnchors, anchor)
}

func runPlace(cmd *cobra.Command, args []string) error {
	cfg, err := parseAndValidatePlace()
	if err != nil {
		return err
	}

	defer func() {
		if err := os.RemoveAll(cfg.TempDir); err != nil {
			log.Printf("warning: failed to remove temp dir %s: %v", cfg.TempDir, err)
		}
	}()

	verbose("connecting to session D-Bus")
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot connect to session D-Bus: %w", err),
		}
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("warning: failed to close D-Bus connection: %v", err)
		}
	}()

	recv := &placeReceiver{ch: make(chan placeResult, 1)}
	callbackService := fmt.Sprintf("io.github.kwinlayout.Place.p%d.r%s", os.Getpid(), generateRandomSuffix())

	verbose("registering D-Bus service %s", callbackService)
	reply, err := conn.RequestName(callbackService, dbus.NameFlagDoNotQueue)
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot acquire place D-Bus name %q: %w", callbackService, err),
		}
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot acquire place D-Bus name %q: already owned", callbackService),
		}
	}

	if err := conn.Export(recv, dbus.ObjectPath(placeObjectPath), placeIface); err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot export place D-Bus object: %w", err),
		}
	}

	verbose("writing script to %s", cfg.JSFile)
	if err := writeJSFile(cfg, callbackService, placeKeepFlag); err != nil {
		return fmt.Errorf("failed to write JS file: %w", err)
	}

	verbose("loading script %s", cfg.ScriptName)
	scriptPath, err := loadScript(conn, cfg.JSFile, cfg.ScriptName)
	if err != nil {
		var warning *ScriptPathError
		if errors.As(err, &warning) {
			log.Printf("warning: %v", warning)
		} else {
			return &ExitError{
				Code: exitCodeLoadFailed,
				Err:  fmt.Errorf("loadScript failed: %w", err),
			}
		}
	}
	verbose("script loaded at path %s", scriptPath)
	defer unloadScript(conn, cfg.ScriptName)

	verbose("running script")
	if err := runScript(conn, cfg.ScriptName, scriptPath); err != nil {
		return &ExitError{
			Code: exitCodeLoadFailed,
			Err:  err,
		}
	}

	verbose("launching command: %v", cfg.Cmd)
	cmdProc, err := launchCommand(cfg.Cmd)
	if err != nil {
		return &ExitError{
			Code: exitCodeLaunchFailed,
			Err:  fmt.Errorf("failed to launch command: %w", err),
		}
	}

	if placeKeepFlag {
		verbose("keep mode: waiting indefinitely for SIGINT/SIGTERM")
		return waitForSignal([]*exec.Cmd{cmdProc})
	}

	verbose("waiting for placement callback (timeout: %s)", cfg.Timeout)
	return waitForPlacement(cfg.Timeout, recv.ch, []*exec.Cmd{cmdProc})
}

func parseAndValidatePlace() (Config, error) {
	cmdStr := strings.TrimSpace(placeCommandFlag)
	if cmdStr == "" {
		return Config{}, &CommandError{Reason: "missing --cmd command string"}
	}

	cmdSlice, err := splitCommand(cmdStr)
	if err != nil {
		return Config{}, &CommandError{Reason: fmt.Sprintf("invalid --cmd value: %v", err)}
	}
	if len(cmdSlice) == 0 {
		return Config{}, &CommandError{Reason: "command is empty after parsing --cmd"}
	}

	if placeAppFlag == "" && placeMatchFlag == "" {
		return Config{}, &ValidationError{
			Field:   "app/match",
			Message: "at least one of --app or --match is required",
		}
	}

	if placeMatchFlag != "" {
		if _, err := regexp.Compile(placeMatchFlag); err != nil {
			return Config{}, &ValidationError{
				Field:   "match",
				Value:   placeMatchFlag,
				Message: "invalid regex: " + err.Error(),
			}
		}
	}

	if !isValidAnchor(placeAnchorFlag) {
		return Config{}, &ValidationError{
			Field:   "anchor",
			Value:   placeAnchorFlag,
			Message: "valid anchors: " + strings.Join(validAnchors, ", "),
		}
	}

	geom, err := parseGeom(placeGeomFlag)
	if err != nil {
		return Config{}, err
	}

	anchor := placeAnchorFlag
	if placeCenteredFlag {
		anchor = "center"
		geom.X = GeomValue{Value: 50, Percent: true}
		geom.Y = GeomValue{Value: 50, Percent: true}
	}

	timeout, err := parseTimeout(placeTimeoutFlag)
	if err != nil {
		return Config{}, err
	}

	scriptName := generateScriptName()

	tempDir, err := os.MkdirTemp("", "kwin-layout-*")
	if err != nil {
		return Config{}, fmt.Errorf("failed to create temp dir: %w", err)
	}

	jsFile := filepath.Join(tempDir, scriptName+".js")

	return Config{
		App:        placeAppFlag,
		Match:      placeMatchFlag,
		Geom:       geom,
		Anchor:     anchor,
		Monitor:    placeMonitorFlag,
		Desktop:    placeDesktopFlag,
		Pinned:     placePinnedFlag,
		Minimized:  placeMinimizedFlag,
		KeepAbove:  placeKeepAboveFlag,
		KeepBelow:  placeKeepBelowFlag,
		Timeout:    timeout,
		Cmd:        cmdSlice,
		ScriptName: scriptName,
		TempDir:    tempDir,
		JSFile:     jsFile,
	}, nil
}

func runLaunch(cmd *cobra.Command, args []string) error {
	templatePath := args[0]

	template, err := parseTemplate(templatePath)
	if err != nil {
		return err
	}

	if err := validateTemplate(template); err != nil {
		return err
	}

	timeout, err := determineLaunchTimeout(template)
	if err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp("", "kwin-layout-launch-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("warning: failed to remove temp dir %s: %v", tempDir, err)
		}
	}()

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot connect to session D-Bus: %w", err),
		}
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("warning: failed to close D-Bus connection: %v", err)
		}
	}()

	recv := &placeReceiver{ch: make(chan placeResult, len(template.Presets)+2)}
	callbackService := fmt.Sprintf("io.github.kwinlayout.Place.p%d.r%s", os.Getpid(), generateRandomSuffix())

	reply, err := conn.RequestName(callbackService, dbus.NameFlagDoNotQueue)
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot acquire launch place D-Bus name %q: %w", callbackService, err),
		}
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot acquire launch place D-Bus name %q: already owned", callbackService),
		}
	}

	if err := conn.Export(recv, dbus.ObjectPath(placeObjectPath), placeIface); err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot export launch place D-Bus object: %w", err),
		}
	}

	var scriptNames []string
	var cmdProcs []*exec.Cmd
	defer func() {
		unloadAllScripts(conn, scriptNames)
	}()

	for i, preset := range template.Presets {
		label := preset.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}

		tile := normalizeTileValue(preset.Tile)
		anchor := preset.Anchor
		if anchor == "" {
			anchor = "top-left"
		}

		var geom ParsedGeometry
		geomProvided := hasAllPresetGeometry(preset.Geometry)
		if geomProvided || tile == "" {
			geom, err = parsePresetGeometry(preset.Geometry, preset.Centered)
			if err != nil {
				return &PresetError{Preset: label, Field: "geometry", Err: err}
			}
		} else {
			// Geometry is optional for quick-tiling presets.
			geom = ParsedGeometry{
				X: GeomValue{Value: 0, Percent: false},
				Y: GeomValue{Value: 0, Percent: false},
				W: GeomValue{Value: 100, Percent: true},
				H: GeomValue{Value: 100, Percent: true},
			}
		}

		if preset.Centered {
			anchor = "center"
			geom.X = GeomValue{Value: 50, Percent: true}
			geom.Y = GeomValue{Value: 50, Percent: true}
		}

		scriptName := fmt.Sprintf("kwin-layout-%d-%d-%s", os.Getpid(), i, generateRandomSuffix())
		jsFile := filepath.Join(tempDir, scriptName+".js")

		jsCfg := jsPlacementConfig{
			ScriptName:      scriptName,
			App:             preset.App,
			Match:           preset.Match,
			Anchor:          anchor,
			Monitor:         preset.Monitor,
			Desktop:         preset.Desktop,
			Pinned:          preset.Pinned,
			Minimized:       preset.Minimized,
			KeepAbove:       preset.KeepAbove,
			KeepBelow:       preset.KeepBelow,
			Maximized:       preset.Maximized,
			FullScreen:      preset.FullScreen,
			Tile:            tile,
			Geom:            geom,
			Verbose:         verboseFlag,
			CallbackService: callbackService,
		}
		js := generateJS(jsCfg)
		if err := os.WriteFile(jsFile, []byte(js), 0600); err != nil {
			return &PresetError{Preset: label, Err: fmt.Errorf("failed to write script: %w", err)}
		}

		scriptPath, err := loadScript(conn, jsFile, scriptName)
		if err != nil {
			var warning *ScriptPathError
			if errors.As(err, &warning) {
				log.Printf("warning: preset %s: %v", label, warning)
			} else {
				return &ExitError{
					Code: exitCodeLoadFailed,
					Err:  fmt.Errorf("loadScript failed for preset %s: %w", label, err),
				}
			}
		}

		scriptNames = append(scriptNames, scriptName)
		if err := runScript(conn, scriptName, scriptPath); err != nil {
			return &ExitError{
				Code: exitCodeLoadFailed,
				Err:  err,
			}
		}

		cmdProc, err := launchCommand([]string(preset.Command))
		if err != nil {
			return &ExitError{
				Code: exitCodeLaunchFailed,
				Err:  fmt.Errorf("failed to launch command for preset %s: %w", label, err),
			}
		}
		cmdProcs = append(cmdProcs, cmdProc)

		perPresetWait := timeout
		const maxPerPresetWait = 2 * time.Second
		if perPresetWait <= 0 || perPresetWait > maxPerPresetWait {
			perPresetWait = maxPerPresetWait
		}
		timer := time.NewTimer(perPresetWait)
		select {
		case result := <-recv.ch:
			verbose("launch preset %s placement callback: success=%v windowID=%s caption=%s geometry=%s",
				label, result.Success, result.WindowID, result.Caption, result.Geometry)
		case <-timer.C:
			verbose("launch preset %s placement callback timeout after %s; continuing",
				label, perPresetWait)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}

	return waitAndCleanup(timeout, cmdProcs)
}

type captureReceiver struct {
	ch chan string
}

//nolint:unparam // D-Bus method signature requires *dbus.Error return; always nil.
func (r *captureReceiver) Send(payload string) (bool, *dbus.Error) {
	select {
	case r.ch <- payload:
	default:
	}
	return true, nil
}

type placeReceiver struct {
	ch chan placeResult
}

type placeResult struct {
	Success  bool
	WindowID string
	Caption  string
	Geometry string
}

//nolint:unparam // D-Bus method signature requires *dbus.Error return; always nil.
func (r *placeReceiver) Placed(success bool, windowID, caption, geom string) (bool, *dbus.Error) {
	select {
	case r.ch <- placeResult{success, windowID, caption, geom}:
	default:
	}
	return true, nil
}

func runCapture(cmd *cobra.Command, args []string) error {
	outPath := strings.TrimSpace(args[0])
	if outPath == "" {
		return &ValidationError{Field: "output", Message: "output path is required (use '-' for stdout)"}
	}
	format := "yaml"
	if outPath != "-" {
		ext := strings.ToLower(filepath.Ext(outPath))
		switch ext {
		case ".yaml", ".yml":
			format = "yaml"
		case ".json": //nolint:goconst // extension strings are clearer inline for output validation.
			format = "json"
		default:
			return &ValidationError{Field: "output", Value: outPath, Message: "expected .yaml/.yml/.json output file (or '-' for stdout)"}
		}
	}

	timeout, err := parseTimeout(captureTimeoutFlag)
	if err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp("", "kwin-layout-capture-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("warning: failed to remove temp dir %s: %v", tempDir, err)
		}
	}()

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot connect to session D-Bus: %w", err),
		}
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("warning: failed to close D-Bus connection: %v", err)
		}
	}()

	recv := &captureReceiver{ch: make(chan string, 1)}
	serviceName := fmt.Sprintf("io.github.kwinlayout.Capture.p%d.r%s", os.Getpid(), generateRandomSuffix())

	reply, err := conn.RequestName(serviceName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot acquire capture D-Bus name %q: %w", serviceName, err),
		}
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot acquire capture D-Bus name %q: already owned", serviceName),
		}
	}

	if err := conn.Export(recv, dbus.ObjectPath(captureObjectPath), captureIface); err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot export capture D-Bus object: %w", err),
		}
	}

	scriptName := fmt.Sprintf("kwin-layout-capture-%d-%s", os.Getpid(), generateRandomSuffix())
	jsFile := filepath.Join(tempDir, scriptName+".js")
	opts := captureOptions{
		InferCommand:   captureInferCommandFlag,
		IncludeUnknown: captureIncludeUnknown,
		CurrentDesktop: captureCurrentDesktop,
		MonitorFilter:  captureMonitorFilter,
	}
	js := generateCaptureJS(scriptName, serviceName, opts)

	if err := os.WriteFile(jsFile, []byte(js), 0600); err != nil {
		return fmt.Errorf("failed to write capture JS file: %w", err)
	}

	scriptPath, err := loadScript(conn, jsFile, scriptName)
	if err != nil {
		var warning *ScriptPathError
		if errors.As(err, &warning) {
			log.Printf("warning: %v", warning)
		} else {
			return &ExitError{
				Code: exitCodeLoadFailed,
				Err:  fmt.Errorf("loadScript failed: %w", err),
			}
		}
	}
	defer unloadScript(conn, scriptName)

	if err := runScript(conn, scriptName, scriptPath); err != nil {
		return &ExitError{
			Code: exitCodeLoadFailed,
			Err:  err,
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var payload string
	select {
	case payload = <-recv.ch:
	case <-timer.C:
		return &ExitError{
			Code: exitCodeLoadFailed,
			Err:  fmt.Errorf("capture timed out after %s (no payload received from KWin script)", timeout),
		}
	}

	template, err := buildTemplateFromCapturePayload(payload)
	if err != nil {
		return &ExitError{
			Code: exitCodeLoadFailed,
			Err:  fmt.Errorf("invalid capture payload: %w", err),
		}
	}

	var data []byte
	if format == "json" {
		data, err = json.MarshalIndent(template, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		data = append(data, '\n')
	} else {
		data, err = marshalTemplateYAML(template)
		if err != nil {
			return err
		}
	}

	if outPath == "-" {
		_, _ = os.Stdout.Write(data)
		return nil
	}

	if err := os.WriteFile(outPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write %q: %w", outPath, err)
	}

	return nil
}

func runValidate(cmd *cobra.Command, args []string) error {
	templatePath := args[0]

	template, err := parseTemplate(templatePath)
	if err != nil {
		return formatValidationFailure(templatePath, err)
	}

	if template.Timeout != "" {
		if _, err := parseTimeout(template.Timeout); err != nil {
			return formatValidationFailure(templatePath, err)
		}
	}

	if err := validateTemplate(template); err != nil {
		return formatValidationFailure(templatePath, err)
	}

	fmt.Printf("✓ %s is valid (%d presets)\n", filepath.Base(templatePath), len(template.Presets))
	return nil
}

func formatValidationFailure(path string, err error) error {
	fmt.Printf("✗ %s validation failed:\n", filepath.Base(path))
	fmt.Printf("  %v\n", err)
	return &ExitError{Code: exitCodeUsage, Err: err}
}

func runCleanup(cmd *cobra.Command, args []string) error {
	verbose("connecting to session D-Bus")
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot connect to session D-Bus: %w", err),
		}
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("warning: failed to close D-Bus connection: %v", err)
		}
	}()

	scripts, err := discoverKwinLayoutScripts(conn)
	if err != nil {
		verbose("introspection failed: %v", err)
	}

	if len(scripts) == 0 {
		fmt.Println("no orphaned kwin-layout scripts found")
		return nil
	}

	count := 0
	for _, scriptName := range scripts {
		if cleanupDryRunFlag {
			fmt.Printf("would unload: %s\n", scriptName)
		} else {
			verbose("unloading script %s", scriptName)
			obj := conn.Object(dbusDestination, dbus.ObjectPath(dbusScriptingPath))
			call := obj.Call(dbusScriptingIface+".unloadScript", 0, scriptName)
			if call.Err != nil {
				verbose("unload failed for %s: %v", scriptName, call.Err)
				continue
			}
			fmt.Printf("unloaded: %s\n", scriptName)
		}
		count++
	}

	fmt.Printf("total: %d script(s)\n", count)
	return nil
}

func discoverKwinLayoutScripts(conn *dbus.Conn) ([]string, error) {
	obj := conn.Object(dbusDestination, dbus.ObjectPath(dbusScriptingPath))
	var xmlData string
	err := obj.Call("org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&xmlData)
	if err != nil {
		return nil, fmt.Errorf("introspection failed: %w", err)
	}

	verbose("introspection result: %d bytes", len(xmlData))

	var scripts []string
	scriptPathRe := regexp.MustCompile(`<node name="(Script\d+)"`)
	matches := scriptPathRe.FindAllStringSubmatch(xmlData, -1)

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		nodeName := match[1]
		scriptPath := fmt.Sprintf("%s/%s", dbusScriptingPath, nodeName)

		verbose("checking script at %s", scriptPath)
		scriptObj := conn.Object(dbusDestination, dbus.ObjectPath(scriptPath))

		var pluginName dbus.Variant
		err := scriptObj.Call("org.freedesktop.DBus.Properties.Get", 0, dbusScriptIface, "pluginName").Store(&pluginName)
		if err != nil {
			verbose("could not get pluginName for %s: %v", scriptPath, err)
			continue
		}

		name, ok := pluginName.Value().(string)
		if !ok {
			verbose("pluginName not a string for %s", scriptPath)
			continue
		}

		verbose("found script: %s", name)
		if strings.HasPrefix(name, "kwin-layout-") {
			scripts = append(scripts, name)
		}
	}

	return scripts, nil
}

func marshalTemplateYAML(template Template) ([]byte, error) {
	var node yaml.Node
	if err := node.Encode(template); err != nil {
		return nil, fmt.Errorf("failed to encode YAML: %w", err)
	}
	setCommandFlowStyle(&node)
	data, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal YAML: %w", err)
	}
	return unquoteYAMLKeyY(data), nil
}

func setCommandFlowStyle(node *yaml.Node) {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, c := range node.Content {
			setCommandFlowStyle(c)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			k := node.Content[i]
			v := node.Content[i+1]
			if k.Kind == yaml.ScalarNode && k.Value == "command" && v.Kind == yaml.SequenceNode {
				v.Style = yaml.FlowStyle
			}
			setCommandFlowStyle(v)
		}
	case yaml.SequenceNode:
		for _, c := range node.Content {
			setCommandFlowStyle(c)
		}
	case yaml.ScalarNode:
		return
	case yaml.AliasNode:
		if node.Alias != nil {
			setCommandFlowStyle(node.Alias)
		}
	}
}

var yamlQuotedYKeyRE = regexp.MustCompile(`(?m)^(\s*)"y":`)

func unquoteYAMLKeyY(data []byte) []byte {
	// yaml.v3 quotes "y" due to YAML 1.1 boolean rules; drop quotes for readability.
	return yamlQuotedYKeyRE.ReplaceAll(data, []byte("${1}y:"))
}

type capturePayload struct {
	Presets []struct {
		Name       string `json:"name"`
		App        string `json:"app"`
		Match      string `json:"match"`
		Monitor    string `json:"monitor"`
		Desktop    string `json:"desktop"`
		Anchor     string `json:"anchor"`
		Maximized  string `json:"maximized"`
		FullScreen bool   `json:"fullscreen"`
		Geom       struct {
			X      string `json:"x"`
			Y      string `json:"y"`
			Width  string `json:"width"`
			Height string `json:"height"`
		} `json:"geometry"`
		Command []string `json:"command"`
	} `json:"presets"`
}

func buildTemplateFromCapturePayload(payload string) (Template, error) {
	var cp capturePayload
	if err := json.Unmarshal([]byte(payload), &cp); err != nil {
		return Template{}, err
	}

	var presets []Preset
	for _, p := range cp.Presets {
		app := strings.TrimSpace(p.App)
		match := strings.TrimSpace(p.Match)
		if app == "" && match == "" {
			continue
		}

		pr := Preset{
			Name:       p.Name,
			App:        app,
			Match:      match,
			Anchor:     p.Anchor,
			Monitor:    p.Monitor,
			Desktop:    p.Desktop,
			Maximized:  p.Maximized,
			FullScreen: p.FullScreen,
			Geometry: PresetGeometry{
				X:      p.Geom.X,
				Y:      p.Geom.Y,
				Width:  p.Geom.Width,
				Height: p.Geom.Height,
			},
		}

		if len(p.Command) > 0 {
			pr.Command = CommandSpec(p.Command)
		} else if captureInferCommandFlag && app != "" {
			pr.Command = CommandSpec([]string{"gtk-launch", app})
		}

		presets = append(presets, pr)
	}

	if len(presets) == 0 {
		return Template{}, &ValidationError{Field: "capture", Message: "no capturable windows found"}
	}

	return Template{Version: version, Presets: presets}, nil
}

func parseTemplate(path string) (Template, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" && ext != ".json" {
		return Template{}, &TemplateError{
			Path:   path,
			Reason: "unsupported file extension (expected .yaml, .yml, or .json)",
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Template{}, &TemplateError{Path: path, Reason: "failed to read file", Err: err}
	}

	var template Template
	if ext == ".json" {
		if err := json.Unmarshal(data, &template); err != nil {
			return Template{}, &TemplateError{Path: path, Reason: "invalid JSON", Err: err}
		}
	} else {
		if err := yaml.Unmarshal(data, &template); err != nil {
			return Template{}, &TemplateError{Path: path, Reason: "invalid YAML", Err: err}
		}
	}

	return template, nil
}

var validMaximizedValues = []string{"", "horizontal", "vertical", "both"}
var validTileValues = []string{
	"",
	"left", "right", "top", "bottom",
	"top-left", "top-right", "bottom-left", "bottom-right",
}

func validateTemplate(t Template) error {
	if len(t.Presets) == 0 {
		return &ValidationError{Field: "presets", Message: "at least one preset is required"}
	}

	for i, p := range t.Presets {
		label := p.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}

		if p.App == "" && p.Match == "" {
			return &PresetError{
				Preset: label,
				Err:    &ValidationError{Field: "app/match", Message: "either app or match is required"},
			}
		}

		if p.Match != "" {
			if _, err := regexp.Compile(p.Match); err != nil {
				return &PresetError{
					Preset: label,
					Err:    &ValidationError{Field: "match", Value: p.Match, Message: "invalid regex: " + err.Error()},
				}
			}
		}

		if len(p.Command) == 0 {
			return &PresetError{
				Preset: label,
				Err:    &ValidationError{Field: "command", Message: "required field is missing"},
			}
		}

		tile := normalizeTileValue(p.Tile)
		if !isValidTile(tile) {
			return &PresetError{
				Preset: label,
				Err: &ValidationError{
					Field:   "tile",
					Value:   p.Tile,
					Message: "valid values: left, right, top, bottom, top-left, top-right, bottom-left, bottom-right (or empty)",
				},
			}
		}

		if tile != "" {
			if p.Centered {
				return &PresetError{
					Preset: label,
					Err:    &ValidationError{Field: "centered", Message: "cannot be combined with tile"},
				}
			}
			if p.Maximized != "" {
				return &PresetError{
					Preset: label,
					Err:    &ValidationError{Field: "maximized", Message: "cannot be combined with tile"},
				}
			}
			if p.FullScreen {
				return &PresetError{
					Preset: label,
					Err:    &ValidationError{Field: "fullscreen", Message: "cannot be combined with tile"},
				}
			}
		}

		geomProvided := hasAnyPresetGeometry(p.Geometry)
		geomComplete := hasAllPresetGeometry(p.Geometry)
		if geomProvided && !geomComplete {
			return &PresetError{
				Preset: label,
				Err: &ValidationError{
					Field:   "geometry",
					Message: "x, y, width, height must all be set together",
				},
			}
		}
		if !geomComplete && tile == "" {
			return &PresetError{
				Preset: label,
				Err:    &ValidationError{Field: "geometry", Message: "required unless tile is set"},
			}
		}

		if geomComplete {
			geom, err := parsePresetGeometry(p.Geometry, p.Centered)
			if err != nil {
				return &PresetError{Preset: label, Field: "geometry", Err: err}
			}

			if geom.W.Value <= 0 {
				return &PresetError{
					Preset: label,
					Err:    &GeometryError{Component: "width", Reason: "must be > 0"},
				}
			}

			if geom.H.Value <= 0 {
				return &PresetError{
					Preset: label,
					Err:    &GeometryError{Component: "height", Reason: "must be > 0"},
				}
			}
		}

		if p.Anchor != "" && !isValidAnchor(p.Anchor) {
			return &PresetError{
				Preset: label,
				Err: &ValidationError{
					Field:   "anchor",
					Value:   p.Anchor,
					Message: "valid anchors: " + strings.Join(validAnchors, ", "),
				},
			}
		}

		if !isValidMaximized(p.Maximized) {
			return &PresetError{
				Preset: label,
				Err: &ValidationError{
					Field:   "maximized",
					Value:   p.Maximized,
					Message: "valid values: horizontal, vertical, both (or empty)",
				},
			}
		}
	}

	return nil
}

func isValidMaximized(v string) bool {
	return slices.Contains(validMaximizedValues, v)
}

func isValidTile(v string) bool {
	return slices.Contains(validTileValues, v)
}

func normalizeTileValue(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

func hasAnyPresetGeometry(pg PresetGeometry) bool {
	return strings.TrimSpace(pg.X) != "" ||
		strings.TrimSpace(pg.Y) != "" ||
		strings.TrimSpace(pg.Width) != "" ||
		strings.TrimSpace(pg.Height) != ""
}

func hasAllPresetGeometry(pg PresetGeometry) bool {
	return strings.TrimSpace(pg.X) != "" &&
		strings.TrimSpace(pg.Y) != "" &&
		strings.TrimSpace(pg.Width) != "" &&
		strings.TrimSpace(pg.Height) != ""
}

func determineLaunchTimeout(t Template) (time.Duration, error) {
	if launchTimeoutFlag != "" {
		return parseTimeout(launchTimeoutFlag)
	}
	if t.Timeout != "" {
		return parseTimeout(t.Timeout)
	}
	return time.Duration(defaultTimeoutSec) * time.Second, nil
}

func unloadAllScripts(conn *dbus.Conn, scriptNames []string) {
	for _, name := range scriptNames {
		unloadScript(conn, name)
	}
}

func parseTimeout(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, &ValidationError{
				Field:   "timeout",
				Value:   s,
				Message: "must be greater than 0",
			}
		}
		return d, nil
	}
	if secs, err := strconv.Atoi(s); err == nil {
		if secs <= 0 {
			return 0, &ValidationError{
				Field:   "timeout",
				Value:   s,
				Message: "must be greater than 0",
			}
		}
		return time.Duration(secs) * time.Second, nil
	}
	return 0, &ValidationError{
		Field:   "timeout",
		Value:   s,
		Message: "expected duration (e.g., 8s, 500ms) or integer seconds",
	}
}

func generateScriptName() string {
	return fmt.Sprintf("kwin-layout-%d-%s", os.Getpid(), generateRandomSuffix())
}

func generateRandomSuffix() string {
	randomBytes := make([]byte, 6)
	if _, err := rand.Read(randomBytes); err == nil {
		return hex.EncodeToString(randomBytes)
	}
	log.Printf("warning: crypto/rand.Read failed; falling back to time-based suffix")
	now := max(time.Now().UnixNano(), 0)
	pid := os.Getpid()
	// #nosec G115 -- now is clamped to non-negative above.
	return fmt.Sprintf("%x%x", uint64(now), uint32(pid))
}

func parseGeomValue(s, component string, allowEmpty bool) (GeomValue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		if allowEmpty {
			return GeomValue{Value: 0, Percent: false}, nil
		}
		return GeomValue{}, &GeometryError{
			Component: component,
			Reason:    "required",
		}
	}
	if before, ok := strings.CutSuffix(s, "%"); ok {
		pct, err := strconv.Atoi(before)
		if err != nil {
			return GeomValue{}, &GeometryError{
				Component: component,
				Value:     s,
				Reason:    "invalid percentage format",
			}
		}
		if pct < 0 || pct > 100 {
			return GeomValue{}, &GeometryError{
				Component: component,
				Value:     s,
				Reason:    "percentage must be 0-100",
			}
		}
		return GeomValue{Value: pct, Percent: true}, nil
	}
	val, err := strconv.Atoi(s)
	if err != nil {
		return GeomValue{}, &GeometryError{
			Component: component,
			Value:     s,
			Reason:    "must be an integer or percentage",
		}
	}
	return GeomValue{Value: val, Percent: false}, nil
}

func parseGeom(s string) (ParsedGeometry, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return ParsedGeometry{}, &ValidationError{
			Field:   "geom",
			Value:   s,
			Message: "expected x,y,w,h (4 comma-separated values)",
		}
	}

	x, err := parseGeomValue(parts[0], "x", false)
	if err != nil {
		return ParsedGeometry{}, err
	}

	y, err := parseGeomValue(parts[1], "y", false)
	if err != nil {
		return ParsedGeometry{}, err
	}

	w, err := parseGeomValue(parts[2], "width", false)
	if err != nil {
		return ParsedGeometry{}, err
	}

	h, err := parseGeomValue(parts[3], "height", false)
	if err != nil {
		return ParsedGeometry{}, err
	}

	if w.Value <= 0 {
		return ParsedGeometry{}, &GeometryError{Component: "width", Reason: "must be > 0"}
	}
	if h.Value <= 0 {
		return ParsedGeometry{}, &GeometryError{Component: "height", Reason: "must be > 0"}
	}

	return ParsedGeometry{X: x, Y: y, W: w, H: h}, nil
}

func parsePresetGeometry(pg PresetGeometry, centered bool) (ParsedGeometry, error) {
	x, err := parseGeomValue(pg.X, "x", centered)
	if err != nil {
		return ParsedGeometry{}, err
	}

	y, err := parseGeomValue(pg.Y, "y", centered)
	if err != nil {
		return ParsedGeometry{}, err
	}

	w, err := parseGeomValue(pg.Width, "width", false)
	if err != nil {
		return ParsedGeometry{}, err
	}

	h, err := parseGeomValue(pg.Height, "height", false)
	if err != nil {
		return ParsedGeometry{}, err
	}

	if w.Value <= 0 {
		return ParsedGeometry{}, &GeometryError{Component: "width", Reason: "must be > 0"}
	}
	if h.Value <= 0 {
		return ParsedGeometry{}, &GeometryError{Component: "height", Reason: "must be > 0"}
	}

	return ParsedGeometry{X: x, Y: y, W: w, H: h}, nil
}

func writeJSFile(cfg Config, callbackService string, keepMode bool) error {
	jsCfg := jsPlacementConfig{
		ScriptName:      cfg.ScriptName,
		App:             cfg.App,
		Match:           cfg.Match,
		Anchor:          cfg.Anchor,
		Monitor:         cfg.Monitor,
		Desktop:         cfg.Desktop,
		Pinned:          cfg.Pinned,
		Minimized:       cfg.Minimized,
		KeepAbove:       cfg.KeepAbove,
		KeepBelow:       cfg.KeepBelow,
		Geom:            cfg.Geom,
		Verbose:         verboseFlag,
		CallbackService: callbackService,
		KeepMode:        keepMode,
	}
	js := generateJS(jsCfg)
	return os.WriteFile(cfg.JSFile, []byte(js), 0600)
}

func splitCommand(input string) ([]string, error) {
	var args []string
	var buf strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		args = append(args, buf.String())
		buf.Reset()
	}

	for _, r := range input {
		switch {
		case escaped:
			buf.WriteRune(r)
			escaped = false
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t' || r == '\n') && !inSingle && !inDouble:
			flush()
		default:
			buf.WriteRune(r)
		}
	}

	if escaped {
		return nil, fmt.Errorf("unfinished escape at end of command")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote in command")
	}

	flush()
	return args, nil
}

type jsPlacementConfig struct {
	ScriptName      string
	App             string
	Match           string
	Anchor          string
	Monitor         string
	Desktop         string
	Tile            string
	Pinned          bool
	Minimized       bool
	KeepAbove       bool
	KeepBelow       bool
	Maximized       string
	FullScreen      bool
	Geom            ParsedGeometry
	Verbose         bool
	CallbackService string
	KeepMode        bool
}

func generateJS(cfg jsPlacementConfig) string {
	scriptNameJSON, _ := json.Marshal(cfg.ScriptName)
	appJSON, _ := json.Marshal(cfg.App)
	matchJSON, _ := json.Marshal(cfg.Match)
	anchorJSON, _ := json.Marshal(cfg.Anchor)
	monitorJSON, _ := json.Marshal(cfg.Monitor)
	desktopJSON, _ := json.Marshal(cfg.Desktop)
	tileJSON, _ := json.Marshal(cfg.Tile)
	maximizedJSON, _ := json.Marshal(cfg.Maximized)
	callbackServiceJSON, _ := json.Marshal(cfg.CallbackService)

	return fmt.Sprintf(`// Auto-generated: %s
var SCRIPT_NAME = %s;
var TARGET_APP = %s;
var TARGET_MATCH = %s;
var ANCHOR = %s;
var MONITOR = %s;
var DESKTOP = %s;
var TILE = %s;
var PINNED = %v;
var MINIMIZED = %v;
var KEEP_ABOVE = %v;
var KEEP_BELOW = %v;
var MAXIMIZED = %s;
var FULLSCREEN = %v;
var GEOM_X = {value: %d, percent: %v};
var GEOM_Y = {value: %d, percent: %v};
var GEOM_W = {value: %d, percent: %v};
var GEOM_H = {value: %d, percent: %v};
var VERBOSE = %v;
var CALLBACK_SERVICE = %s;
var CALLBACK_PATH = "/io/github/kwinlayout/Place";
var CALLBACK_IFACE = "io.github.kwinlayout.Place";
var KEEP_MODE = %v;

function vlog() {
  if (!VERBOSE) return;
  var msg = "[kwin-layout] ";
  for (var i = 0; i < arguments.length; i++) msg += arguments[i] + " ";
  print(msg);
}

function idOf(w) { return "" + w.internalId; }

function windowIds(w) {
  var ids = [];
  function add(v) {
    if (v === undefined || v === null) return;
    var s = ("" + v).trim();
    if (s === "") return;
    for (var i = 0; i < ids.length; i++) {
      if (ids[i] === s) return;
    }
    ids.push(s);
  }

  try { add(w.desktopFileName); } catch (e) {}
  try { add(w.resourceClass); } catch (e) {}
  try { add(w.resourceName); } catch (e) {}
  try { add(w.appId); } catch (e) {}
  return ids;
}

function appTargetMatches(id, target) {
  if (!id || !target) return false;
  if (id === target) return true;
  if (id === (target + ".desktop")) return true;
  if (id.endsWith("/" + target + ".desktop")) return true;
  if (id.endsWith("/" + target)) return true;

  var idLower = id.toLowerCase();
  var targetLower = target.toLowerCase();
  if (idLower === targetLower) return true;
  if (idLower === (targetLower + ".desktop")) return true;
  if (idLower.endsWith("/" + targetLower + ".desktop")) return true;
  if (idLower.endsWith("/" + targetLower)) return true;

  function stripPathAndDesktop(s) {
    var out = s;
    var slash = out.lastIndexOf("/");
    if (slash >= 0) out = out.slice(slash + 1);
    if (out.endsWith(".desktop")) out = out.slice(0, -8);
    return out;
  }

  function lastSegment(s) {
    var dot = s.lastIndexOf(".");
    if (dot >= 0 && dot + 1 < s.length) return s.slice(dot + 1);
    return s;
  }

  function stripInstanceSuffix(s) {
    // Treat ghostty-like class suffixes (e.g. "-1") as the same base app.
    return s.replace(/-[0-9]+$/, "");
  }

  var idNorm = stripPathAndDesktop(idLower);
  var targetNorm = stripPathAndDesktop(targetLower);
  if (idNorm === targetNorm) return true;

  var idTail = stripInstanceSuffix(lastSegment(idNorm));
  var targetTail = stripInstanceSuffix(lastSegment(targetNorm));
  if (idTail !== "" && idTail === targetTail) return true;

  if (idNorm.startsWith(targetNorm + "-")) return true;
  if (targetNorm.startsWith(idNorm + "-")) return true;
  return false;
}

function appMatches(w) {
  if (TARGET_APP === "" && TARGET_MATCH === "") return false;
  var ids = windowIds(w);
  if (TARGET_APP !== "") {
    for (var i = 0; i < ids.length; i++) {
      if (appTargetMatches(ids[i], TARGET_APP)) {
        vlog("appMatches: id match id=", ids[i], "target=", TARGET_APP);
        return true;
      }
    }
  }
  if (TARGET_MATCH !== "") {
    try {
      var re = new RegExp(TARGET_MATCH);
      var caption = w.caption || "";
      if (re.test(caption)) { vlog("appMatches: regex match caption=", caption); return true; }
    } catch (e) { vlog("appMatches: regex error", e); }
  }
  vlog("appMatches: no match ids=", ids.join("|"), "caption=", w.caption || "");
  return false;
}

function isManageable(w) {
  if (!w) { vlog("isManageable: null window"); return false; }
  if (w.deleted) { vlog("isManageable: deleted"); return false; }
  if (w.specialWindow) { vlog("isManageable: specialWindow"); return false; }
  if (w.popupWindow) { vlog("isManageable: popupWindow"); return false; }
  if (w.dock) { vlog("isManageable: dock"); return false; }
  if (w.desktopWindow) { vlog("isManageable: desktopWindow"); return false; }
  return true;
}

function findMonitor(id) {
  if (!id || id === "") return workspace.activeScreen;
  var screens = workspace.screens || [];
  var idStr = ("" + id).trim();

  function monitorName(mon) {
    try { if (mon.name) return "" + mon.name; } catch (e) {}
    try { if (mon.connector) return "" + mon.connector; } catch (e) {}
    try { if (mon.connectorName) return "" + mon.connectorName; } catch (e) {}
    return "";
  }

  function norm(s) {
    return ("" + s).toLowerCase().replace(/[^a-z0-9]/g, "");
  }

  if (/^\d+$/.test(idStr)) {
    var idx = parseInt(idStr, 10);
    if (!isNaN(idx) && idx >= 0 && idx < screens.length) {
      return screens[idx];
    }
  }

  var names = [];
  for (var i = 0; i < screens.length; i++) {
    var nm = monitorName(screens[i]);
    if (nm !== "") names.push(nm);
    if (nm === idStr) return screens[i];
  }

  var wanted = norm(idStr);
  for (var j = 0; j < screens.length; j++) {
    var n = monitorName(screens[j]);
    if (n === "") continue;
    if (n.toLowerCase() === idStr.toLowerCase()) return screens[j];
    var m = norm(n);
    if (wanted !== "" && m !== "" && (m === wanted || m.endsWith(wanted) || wanted.endsWith(m))) {
      return screens[j];
    }
  }

  if (names.length > 0) {
    vlog("findMonitor: no match for", idStr, "available=", names.join(","));
  }
  if (screens.length > 0) {
    vlog("findMonitor: falling back to first output index 0");
    return screens[0];
  }
  return workspace.activeScreen;
}

function findDesktop(id) {
  if (!id || id === "") return null;
  var desktops = workspace.desktops;
  var idx = parseInt(id);
  if (!isNaN(idx) && idx >= 1 && idx <= desktops.length) {
    return desktops[idx - 1];
  }
  for (var i = 0; i < desktops.length; i++) {
    if (desktops[i].name === id) return desktops[i];
  }
  return null;
}

function windowOnMonitor(w, mon) {
  if (!w || !mon) return false;
  var g = w.frameGeometry;
  var mg = mon.geometry;
  var cx = g.x + g.width / 2;
  var cy = g.y + g.height / 2;
  return cx >= mg.x && cx < (mg.x + mg.width) && cy >= mg.y && cy < (mg.y + mg.height);
}

function resolveGeom(mon) {
  var mg = mon.geometry;
  var w = GEOM_W.percent ? Math.round(mg.width * GEOM_W.value / 100) : GEOM_W.value;
  var h = GEOM_H.percent ? Math.round(mg.height * GEOM_H.value / 100) : GEOM_H.value;
  var x = GEOM_X.percent ? Math.round(mg.width * GEOM_X.value / 100) : GEOM_X.value;
  var y = GEOM_Y.percent ? Math.round(mg.height * GEOM_Y.value / 100) : GEOM_Y.value;

  x += mg.x;
  y += mg.y;

  switch (ANCHOR) {
    case "top-center":
      x -= Math.round(w / 2);
      break;
    case "top-right":
      x -= w;
      break;
    case "center-left":
      y -= Math.round(h / 2);
      break;
    case "center":
      x -= Math.round(w / 2);
      y -= Math.round(h / 2);
      break;
    case "center-right":
      x -= w;
      y -= Math.round(h / 2);
      break;
    case "bottom-left":
      y -= h;
      break;
    case "bottom-center":
      x -= Math.round(w / 2);
      y -= h;
      break;
    case "bottom-right":
      x -= w;
      y -= h;
      break;
  }

  return {x: x, y: y, width: w, height: h};
}

function notifySuccess(w) {
  if (!CALLBACK_SERVICE) return;
  try {
    var g = w.frameGeometry;
    callDBus(CALLBACK_SERVICE, CALLBACK_PATH, CALLBACK_IFACE, "Placed",
             true, "" + w.internalId, "" + w.caption,
             g.x + "," + g.y + "," + g.width + "," + g.height);
    vlog("notifySuccess: sent callback");
  } catch (e) { vlog("notifySuccess: error", e); }
}

var baseline = {};
for (var i = 0; i < workspace.stackingOrder.length; i++) {
  var w0 = workspace.stackingOrder[i];
  if (isManageable(w0) && appMatches(w0)) {
    baseline[idOf(w0)] = true;
    vlog("baseline: excluding pre-existing window id=", idOf(w0), "caption=", w0.caption);
  }
}

var handled = false;
var targetMon = findMonitor(MONITOR);
var target = resolveGeom(targetMon);
vlog("target geometry:", target.x, target.y, target.width, target.height);

function resolveTileFallbackGeom(mon, tile) {
  var mg = mon.geometry;
  var halfW = Math.round(mg.width / 2);
  var halfH = Math.round(mg.height / 2);
  var leftX = mg.x;
  var rightX = mg.x + mg.width - halfW;
  var topY = mg.y;
  var bottomY = mg.y + mg.height - halfH;

  switch (tile) {
    case "left":
      return {x: leftX, y: topY, width: halfW, height: mg.height};
    case "right":
      return {x: rightX, y: topY, width: halfW, height: mg.height};
    case "top":
      return {x: leftX, y: topY, width: mg.width, height: halfH};
    case "bottom":
      return {x: leftX, y: bottomY, width: mg.width, height: halfH};
    case "top-left":
      return {x: leftX, y: topY, width: halfW, height: halfH};
    case "top-right":
      return {x: rightX, y: topY, width: halfW, height: halfH};
    case "bottom-left":
      return {x: leftX, y: bottomY, width: halfW, height: halfH};
    case "bottom-right":
      return {x: rightX, y: bottomY, width: halfW, height: halfH};
    default:
      return target;
  }
}

function quickTileModeFor(tile) {
  switch (tile) {
    case "left": return 1;
    case "right": return 2;
    case "top": return 4;
    case "bottom": return 8;
    case "top-left": return 5;
    case "top-right": return 6;
    case "bottom-left": return 9;
    case "bottom-right": return 10;
    default: return 0;
  }
}

var targetTileMode = quickTileModeFor(TILE);
if (targetTileMode !== 0) {
  vlog("target tile mode:", TILE, targetTileMode);
  target = resolveTileFallbackGeom(targetMon, TILE);
  vlog("tile fallback geometry:", target.x, target.y, target.width, target.height);
}

function tileShortcutSequence(tile) {
  switch (tile) {
    case "left":
      return ["Window Quick Tile Left"];
    case "right":
      return ["Window Quick Tile Right"];
    case "top":
      return ["Window Quick Tile Top"];
    case "bottom":
      return ["Window Quick Tile Bottom"];
    case "top-left":
      return ["Window Quick Tile Left", "Window Quick Tile Top"];
    case "top-right":
      return ["Window Quick Tile Right", "Window Quick Tile Top"];
    case "bottom-left":
      return ["Window Quick Tile Left", "Window Quick Tile Bottom"];
    case "bottom-right":
      return ["Window Quick Tile Right", "Window Quick Tile Bottom"];
    default:
      return [];
  }
}

function invokeQuickTileShortcut(w, tile) {
  var seq = tileShortcutSequence(tile);
  if (seq.length === 0) return false;

  var want = idOf(w);
  try {
    workspace.activeWindow = w;
  } catch (e) { vlog("invokeQuickTileShortcut: failed to activate window", e); }

  var active = "";
  try {
    if (workspace.activeWindow) active = idOf(workspace.activeWindow);
  } catch (e) {}
  if (active !== want) {
    vlog("invokeQuickTileShortcut: target window is not active (want=", want, "active=", active, "), skipping");
    return false;
  }

  try {
    for (var i = 0; i < seq.length; i++) {
      callDBus("org.kde.kglobalaccel", "/component/kwin",
               "org.kde.kglobalaccel.Component", "invokeShortcut", seq[i]);
      vlog("invokeQuickTileShortcut: invoked", seq[i]);
    }
    return true;
  } catch (e) {
    vlog("invokeQuickTileShortcut: DBus invoke failed", e);
    return false;
  }
}

function tileLooksApplied(w, mon, tile) {
  if (!w || !mon) return false;
  if (!windowOnMonitor(w, mon)) return false;
  var g = w.frameGeometry;
  var mg = mon.geometry;
  var cx = g.x + g.width / 2;
  var cy = g.y + g.height / 2;
  var midX = mg.x + mg.width / 2;
  var midY = mg.y + mg.height / 2;

  switch (tile) {
    case "left":
      return cx < midX;
    case "right":
      return cx > midX;
    case "top":
      return cy < midY;
    case "bottom":
      return cy > midY;
    case "top-left":
      return cx < midX && cy < midY;
    case "top-right":
      return cx > midX && cy < midY;
    case "bottom-left":
      return cx < midX && cy > midY;
    case "bottom-right":
      return cx > midX && cy > midY;
    default:
      return false;
  }
}

function finish() {
  if (KEEP_MODE) {
    vlog("finish: keep mode active, not unloading");
    return;
  }
  vlog("finish: unloading script");
  try { workspace.windowAdded.disconnect(onAdded); } catch (e) {}
  try {
    if (typeof callDBus === "function") {
      callDBus("org.kde.KWin", "/Scripting", "org.kde.kwin.Scripting", "unloadScript", SCRIPT_NAME);
    }
  } catch (e) {}
}

function applyAndStick(w) {
  if (handled && !KEEP_MODE) return;
  handled = true;

  vlog("applyAndStick: window caption=", w.caption, "app=", w.desktopFileName, "id=", idOf(w));

  if (MONITOR !== "") {
    try { workspace.sendWindowToOutput(w, targetMon); } catch (e) {}
  }

  if (FULLSCREEN) {
    w.fullScreen = true;
    vlog("applyAndStick: set fullscreen");
    notifySuccess(w);
  } else if (targetTileMode !== 0) {
    if (MONITOR !== "" && !windowOnMonitor(w, targetMon)) {
      vlog("applyAndStick: window not yet on target monitor; tiling with fallback enabled");
    }
    var tileApplied = false;
    try {
      w.quickTileMode = targetTileMode;
      tileApplied = tileLooksApplied(w, targetMon, TILE);
      vlog("applyAndStick: set quickTileMode to", targetTileMode, "applied=", tileApplied);
    } catch (e) {
      vlog("applyAndStick: set quickTileMode failed", e);
    }

    if (!tileApplied) {
      _ = invokeQuickTileShortcut(w, TILE);
      tileApplied = tileLooksApplied(w, targetMon, TILE);
    }

    if (!tileApplied) {
      w.frameGeometry = target;
      vlog("applyAndStick: immediate fallback geometry to", target.x, target.y, target.width, target.height);
    }
  } else {
    w.frameGeometry = target;
    vlog("applyAndStick: set geometry to", target.x, target.y, target.width, target.height);
    if (MAXIMIZED === "both") {
      try { w.setMaximize(true, true); } catch (e) {}
    } else if (MAXIMIZED === "horizontal") {
      try { w.setMaximize(false, true); } catch (e) {}
    } else if (MAXIMIZED === "vertical") {
      try { w.setMaximize(true, false); } catch (e) {}
    }
  }

  if (PINNED) {
    try { w.desktops = workspace.desktops; } catch (e) {}
  } else {
    var desk = findDesktop(DESKTOP);
    if (desk) {
      try { w.desktops = [desk]; } catch (e) {}
    }
  }

  if (MINIMIZED) {
    try { w.minimized = true; } catch (e) {}
  }
  if (KEEP_ABOVE) {
    try { w.keepAbove = true; } catch (e) {}
  }
  if (KEEP_BELOW) {
    try { w.keepBelow = true; } catch (e) {}
  }

  var triesLeft = 200;
  var retryLimitLogged = false;
  var notified = false;
  var tileShortcutAttempts = 0;

  function ensure() {
    if (!KEEP_MODE) {
      if (triesLeft > 0) {
        triesLeft--;
      } else if (!retryLimitLogged) {
        // Keep script loaded until launcher cleanup; some apps can re-center very late.
        vlog("ensure: retry budget exhausted; continuing without auto-unload");
        retryLimitLogged = true;
      }
    }

    if (FULLSCREEN) return;

    if (targetTileMode !== 0) {
      if (MONITOR !== "" && !windowOnMonitor(w, targetMon)) {
        vlog("ensure(tile): window not on target monitor, retrying move and continuing");
        try { workspace.sendWindowToOutput(w, targetMon); } catch (e) {}
      }

      var tileOk = tileLooksApplied(w, targetMon, TILE);
      var currentTileMode = 0;
      try { currentTileMode = (w.quickTileMode || 0); } catch (e) {}
      vlog("ensure(tile): current=", currentTileMode, "target=", targetTileMode, "ok=", tileOk, "triesLeft=", triesLeft);

      if (tileOk) {
        if (!notified) {
          notified = true;
          notifySuccess(w);
        }
        return;
      }

      if (tileShortcutAttempts < 6) {
        if (invokeQuickTileShortcut(w, TILE)) {
          vlog("ensure(tile): shortcut invoke attempt", tileShortcutAttempts + 1, "sent");
        }
        tileShortcutAttempts++;
      }

      try { w.quickTileMode = targetTileMode; } catch (e) {}
      w.frameGeometry = target;
      return;
    }

    if (MONITOR !== "" && !windowOnMonitor(w, targetMon)) {
      vlog("ensure: window not on target monitor, retrying move");
      try { workspace.sendWindowToOutput(w, targetMon); } catch (e) {}
    }

    var g = w.frameGeometry;
    var ok =
      (Math.round(g.x) === target.x) &&
      (Math.round(g.y) === target.y) &&
      (Math.round(g.width) === target.width) &&
      (Math.round(g.height) === target.height);

    vlog("ensure: current=", g.x, g.y, g.width, g.height, "target=", target.x, target.y, target.width, target.height, "ok=", ok, "triesLeft=", triesLeft);

    if (ok) {
      if (!notified) {
        notified = true;
        notifySuccess(w);
      }
      // Some apps (notably terminals) can apply a late resize after map.
      // Keep listening for a bit instead of unloading immediately.
      return;
    }

    w.frameGeometry = target;
  }

  try { w.frameGeometryChanged.connect(ensure); } catch (e) {}
  try { w.windowShown.connect(ensure); } catch (e) {}
  try { w.activeChanged.connect(ensure); } catch (e) {}

  ensure();
}

function isNewMatch(w) {
  if (!isManageable(w)) return false;
  if (!appMatches(w)) return false;

  var id = idOf(w);
  if (baseline[id]) {
    vlog("isNewMatch: window in baseline, skipping id=", id);
    return false;
  }
  return true;
}

function onAdded(w) {
  vlog("onAdded: window added caption=", w.caption || "", "id=", idOf(w));
  if ((!handled || KEEP_MODE) && isNewMatch(w)) applyAndStick(w);
}

workspace.windowAdded.connect(onAdded);

for (var i = 0; i < workspace.stackingOrder.length; i++) {
  var w1 = workspace.stackingOrder[i];
  if (!handled && isNewMatch(w1)) {
    vlog("initial scan: found matching window");
    applyAndStick(w1);
    break;
  }
}
`, cfg.ScriptName, string(scriptNameJSON), string(appJSON), string(matchJSON),
		string(anchorJSON), string(monitorJSON), string(desktopJSON), string(tileJSON), cfg.Pinned,
		cfg.Minimized, cfg.KeepAbove, cfg.KeepBelow,
		string(maximizedJSON), cfg.FullScreen,
		cfg.Geom.X.Value, cfg.Geom.X.Percent,
		cfg.Geom.Y.Value, cfg.Geom.Y.Percent,
		cfg.Geom.W.Value, cfg.Geom.W.Percent,
		cfg.Geom.H.Value, cfg.Geom.H.Percent,
		cfg.Verbose, string(callbackServiceJSON), cfg.KeepMode)
}

func generateCaptureJS(scriptName, serviceName string, opts captureOptions) string {
	scriptNameJSON, _ := json.Marshal(scriptName)
	serviceJSON, _ := json.Marshal(serviceName)
	pathJSON, _ := json.Marshal(captureObjectPath)
	ifaceJSON, _ := json.Marshal(captureIface)
	monitorFilterJSON, _ := json.Marshal(opts.MonitorFilter)

	return fmt.Sprintf(`// Auto-generated capture script: %s
var SCRIPT_NAME = %s;
var CAP_SERVICE = %s;
var CAP_PATH = %s;
var CAP_IFACE = %s;
var INFER_COMMAND = %v;
var INCLUDE_UNKNOWN = %v;
var CURRENT_DESKTOP_ONLY = %v;
var MONITOR_FILTER = %s;

function isManageable(w) {
  if (!w) return false;
  if (w.deleted) return false;
  if (w.specialWindow) return false;
  if (w.popupWindow) return false;
  if (w.dock) return false;
  if (w.desktopWindow) return false;
  return true;
}

function normalizeApp(app) {
  app = app ? ("" + app) : "";
  if (app.endsWith(".desktop")) app = app.slice(0, -8);
  var slash = app.lastIndexOf("/");
  if (slash >= 0) app = app.slice(slash + 1);
  return app;
}

function findOutputForRect(r) {
  var cx = Math.round(r.x + r.width / 2);
  var cy = Math.round(r.y + r.height / 2);
  var screens = workspace.screens;
  for (var i = 0; i < screens.length; i++) {
    var g = screens[i].geometry;
    if (cx >= g.x && cx < g.x + g.width && cy >= g.y && cy < g.y + g.height) {
      return screens[i];
    }
  }
  return workspace.activeScreen;
}

function desktopIdForWindow(w) {
  try {
    if (w.desktops && w.desktops.length > 0) {
      var d = w.desktops[0];
      if (d && d.name) return "" + d.name;
    }
  } catch (e) {}
  return "";
}

function isOnCurrentDesktop(w) {
  if (!CURRENT_DESKTOP_ONLY) return true;
  var curDesk = workspace.currentDesktop;
  var desks = w.desktops;
  if (!desks || desks.length === 0) return true;
  for (var i = 0; i < desks.length; i++) {
    if (desks[i] === curDesk) return true;
  }
  return false;
}

function isOnMonitor(w, fg) {
  if (!MONITOR_FILTER) return true;
  var out = findOutputForRect(fg);
  return out && out.name === MONITOR_FILTER;
}

function getMaximizedState(w) {
  var h = w.maximizedHorizontally || false;
  var v = w.maximizedVertically || false;
  if (h && v) return "both";
  if (h) return "horizontal";
  if (v) return "vertical";
  return "";
}

function sanitizeTitle(title) {
  return (title || "").replace(/[^a-zA-Z0-9_-]/g, "-").replace(/-+/g, "-").replace(/^-|-$/g, "").slice(0, 40).toLowerCase() || "unknown";
}

function escapeRegex(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function capture() {
  var wins = null;
  try {
    if (typeof workspace.windowList === "function") wins = workspace.windowList();
  } catch (e) {}
  if (!wins) wins = workspace.stackingOrder;

  var presets = [];
  var seq = 0;

  for (var i = 0; i < wins.length; i++) {
    var w = wins[i];
    if (!isManageable(w)) continue;

    var app0 = w.desktopFileName ? ("" + w.desktopFileName) : "";
    var app = normalizeApp(app0);
    var caption = w.caption ? ("" + w.caption) : "";

    if (!app && !INCLUDE_UNKNOWN) continue;

    var fg = w.frameGeometry;
    if (!isOnCurrentDesktop(w)) continue;
    if (!isOnMonitor(w, fg)) continue;

    var out = findOutputForRect(fg);
    var og = out.geometry;

    var rx = Math.round(fg.x - og.x);
    var ry = Math.round(fg.y - og.y);

    seq++;
    var baseName = app || sanitizeTitle(caption);
    var name = baseName + "-" + seq;

    var cmd = [];
    if (INFER_COMMAND && app) cmd = ["gtk-launch", app];

    var match = "";
    if (!app && caption) {
      match = "^" + escapeRegex(caption) + "$";
    }

    var p = {
      name: name,
      app: app,
      match: match,
      monitor: out && out.name ? ("" + out.name) : "",
      desktop: desktopIdForWindow(w),
      anchor: "top-left",
      maximized: getMaximizedState(w),
      fullscreen: w.fullScreen || false,
      geometry: {
        x: "" + rx,
        y: "" + ry,
        width: "" + Math.round(fg.width),
        height: "" + Math.round(fg.height)
      },
      command: cmd
    };

    presets.push(p);
  }

  var payload = JSON.stringify({ presets: presets });

  try {
    callDBus(CAP_SERVICE, CAP_PATH, CAP_IFACE, "Send", payload);
  } catch (e) {}

  try {
    callDBus("org.kde.KWin", "/Scripting", "org.kde.kwin.Scripting", "unloadScript", SCRIPT_NAME);
  } catch (e) {}
}

capture();
`, scriptName, string(scriptNameJSON), string(serviceJSON), string(pathJSON), string(ifaceJSON),
		opts.InferCommand, opts.IncludeUnknown, opts.CurrentDesktop, string(monitorFilterJSON))
}

func loadScript(conn *dbus.Conn, jsPath, scriptName string) (string, error) {
	obj := conn.Object(dbusDestination, dbus.ObjectPath(dbusScriptingPath))
	call := obj.Call(dbusScriptingIface+".loadScript", 0, jsPath, scriptName)
	if call.Err != nil {
		return "", call.Err
	}

	return normalizeScriptPath(call.Body)
}

func normalizeScriptPath(body []any) (string, error) {
	if len(body) == 0 {
		return "", &ScriptPathError{Reason: "empty response"}
	}

	val := body[0]

	switch v := val.(type) {
	case string:
		if strings.HasPrefix(v, scriptObjectPrefix) {
			return v, nil
		}
		if v == "" {
			return "", &ScriptPathError{Reason: "empty string response"}
		}
		return "", &ScriptPathError{Reason: fmt.Sprintf("unexpected string %q", v)}
	case int32:
		return fmt.Sprintf("%s%d", scriptObjectPrefix, v), nil
	case int64:
		return fmt.Sprintf("%s%d", scriptObjectPrefix, v), nil
	case uint32:
		return fmt.Sprintf("%s%d", scriptObjectPrefix, v), nil
	case uint64:
		return fmt.Sprintf("%s%d", scriptObjectPrefix, v), nil
	case int:
		return fmt.Sprintf("%s%d", scriptObjectPrefix, v), nil
	case dbus.ObjectPath:
		if string(v) == "" {
			return "", &ScriptPathError{Reason: "empty object path response"}
		}
		return string(v), nil
	default:
		return "", &ScriptPathError{Reason: fmt.Sprintf("unexpected type %T", val)}
	}
}

func runScript(conn *dbus.Conn, scriptName, scriptPath string) error {
	if scriptPath == "" {
		obj := conn.Object(dbusDestination, dbus.ObjectPath(dbusScriptingPath))
		call := obj.Call(dbusScriptingIface+".start", 0, scriptName)
		if call.Err != nil {
			return fmt.Errorf("could not start script %q via Scripting.start: %w", scriptName, call.Err)
		}
		return nil
	}

	obj := conn.Object(dbusDestination, dbus.ObjectPath(scriptPath))

	call := obj.Call(dbusScriptIface+".run", 0)
	if call.Err != nil {
		call = obj.Call(dbusScriptIface+".start", 0)
		if call.Err != nil {
			return fmt.Errorf("could not run/start script %q: %w", scriptName, call.Err)
		}
	}
	return nil
}

func unloadScript(conn *dbus.Conn, scriptName string) {
	obj := conn.Object(dbusDestination, dbus.ObjectPath(dbusScriptingPath))
	call := obj.Call(dbusScriptingIface+".unloadScript", 0, scriptName)
	if call.Err != nil {
		log.Printf("warning: unloadScript failed (may have self-unloaded): %v", call.Err)
	}
}

func launchCommand(cmdSlice []string) (*exec.Cmd, error) {
	expanded := make([]string, len(cmdSlice))
	for i, arg := range cmdSlice {
		expanded[i] = expandEnvVars(arg)
	}
	verbose("expanded command: %v", expanded)
	cmd := exec.Command(expanded[0], expanded[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = filteredEnv(os.Environ(),
		"XDG_ACTIVATION_TOKEN",
		"DESKTOP_STARTUP_ID",
		"GIO_LAUNCHED_DESKTOP_FILE",
		"GIO_LAUNCHED_DESKTOP_FILE_PID",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func filteredEnv(env []string, drop ...string) []string {
	if len(drop) == 0 || len(env) == 0 {
		return env
	}
	dropSet := make(map[string]struct{}, len(drop))
	for _, key := range drop {
		dropSet[key] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		key := kv[:eq]
		if _, remove := dropSet[key]; remove {
			continue
		}
		out = append(out, kv)
	}
	return out
}

var envVarBraceRE = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
var envVarPlainRE = regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)

func expandEnvVars(s string) string {
	s = envVarBraceRE.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[2 : len(match)-1]
		return os.Getenv(varName)
	})
	s = envVarPlainRE.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[1:]
		return os.Getenv(varName)
	})
	return s
}

func waitAndCleanup(timeout time.Duration, cmds []*exec.Cmd) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case sig := <-sigCh:
		signalProcessGroups(cmds, sig)
		switch sig {
		case syscall.SIGINT:
			return &ExitError{Code: exitCodeInterrupted, Err: fmt.Errorf("interrupted by SIGINT")}
		case syscall.SIGTERM:
			return &ExitError{Code: exitCodeTerminated, Err: fmt.Errorf("interrupted by SIGTERM")}
		default:
			return &ExitError{Code: exitCodeTerminated, Err: fmt.Errorf("interrupted by signal %v", sig)}
		}
	}
}

func signalProcessGroups(cmds []*exec.Cmd, sig os.Signal) {
	ss, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	for _, cmd := range cmds {
		if cmd == nil || cmd.Process == nil {
			continue
		}
		_ = syscall.Kill(-cmd.Process.Pid, ss)
	}
}

func waitForPlacement(timeout time.Duration, resultCh <-chan placeResult, cmds []*exec.Cmd) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-resultCh:
		verbose("placement callback received: success=%v windowID=%s caption=%s geometry=%s",
			result.Success, result.WindowID, result.Caption, result.Geometry)
		return nil
	case <-timer.C:
		verbose("timeout reached, placement may have failed")
		return nil
	case sig := <-sigCh:
		signalProcessGroups(cmds, sig)
		switch sig {
		case syscall.SIGINT:
			return &ExitError{Code: exitCodeInterrupted, Err: fmt.Errorf("interrupted by SIGINT")}
		case syscall.SIGTERM:
			return &ExitError{Code: exitCodeTerminated, Err: fmt.Errorf("interrupted by SIGTERM")}
		default:
			return &ExitError{Code: exitCodeTerminated, Err: fmt.Errorf("interrupted by signal %v", sig)}
		}
	}
}

func waitForSignal(cmds []*exec.Cmd) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	sig := <-sigCh
	verbose("received signal %v, cleaning up", sig)
	signalProcessGroups(cmds, sig)
	switch sig {
	case syscall.SIGINT:
		return &ExitError{Code: exitCodeInterrupted, Err: fmt.Errorf("interrupted by SIGINT")}
	case syscall.SIGTERM:
		return &ExitError{Code: exitCodeTerminated, Err: fmt.Errorf("interrupted by SIGTERM")}
	default:
		return &ExitError{Code: exitCodeTerminated, Err: fmt.Errorf("interrupted by signal %v", sig)}
	}
}
