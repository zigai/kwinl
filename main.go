package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	captureObjectPath = "/io/github/kwinl/Capture"
	captureIface      = "io.github.kwinl.Capture"

	placeObjectPath = "/io/github/kwinl/Place"
	placeIface      = "io.github.kwinl.Place"

	formatJSON = "json"
)

var (
	errInvalidCommandExpectedStringOrStringArray = errors.New("invalid command: expected string or string array")
	errInvalidCommandExpectedStringArray         = errors.New("invalid command: expected string array")
	errAlreadyOwned                              = errors.New("already owned")
	errNoCapturePayloadReceived                  = errors.New("no payload received from KWin script")
	errNoPlacementCallbackReceived               = errors.New("no placement callback received")
	errSplitCommandUnfinishedEscape              = errors.New("unfinished escape at end of command")
	errSplitCommandUnterminatedQuote             = errors.New("unterminated quote in command")
	errInterruptedBySIGINT                       = errors.New("interrupted by SIGINT")
	errInterruptedBySIGTERM                      = errors.New("interrupted by SIGTERM")
	errInterruptedBySignal                       = errors.New("interrupted by signal")
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
	X      string `json:"x"      yaml:"x"`
	Y      string `json:"y"      yaml:"y"`
	Width  string `json:"width"  yaml:"width"`
	Height string `json:"height" yaml:"height"`
}

type presetGeometryValue string

func (v *presetGeometryValue) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*v = ""
		return nil
	}

	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		*v = presetGeometryValue(asString)
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var asNumber json.Number
	if err := decoder.Decode(&asNumber); err != nil {
		return fmt.Errorf("decode preset geometry number: %w", err)
	}

	*v = presetGeometryValue(asNumber.String())

	return nil
}

func (pg *PresetGeometry) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*pg = PresetGeometry{X: "", Y: "", Width: "", Height: ""}
		return nil
	}

	type rawPresetGeometry struct {
		X      presetGeometryValue `json:"x"`
		Y      presetGeometryValue `json:"y"`
		Width  presetGeometryValue `json:"width"`
		Height presetGeometryValue `json:"height"`
	}

	var raw rawPresetGeometry
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal preset geometry: %w", err)
	}

	*pg = PresetGeometry{
		X:      string(raw.X),
		Y:      string(raw.Y),
		Width:  string(raw.Width),
		Height: string(raw.Height),
	}

	return nil
}

type Preset struct {
	Name       string         `json:"name"                 yaml:"name"`
	App        string         `json:"app,omitempty"        yaml:"app,omitempty"`
	Match      string         `json:"match,omitempty"      yaml:"match,omitempty"`
	Command    CommandSpec    `json:"command"              yaml:"command"`
	Geometry   PresetGeometry `json:"geometry"             yaml:"geometry"`
	Tile       string         `json:"tile,omitempty"       yaml:"tile,omitempty"`
	Anchor     string         `json:"anchor,omitempty"     yaml:"anchor,omitempty"`
	Monitor    string         `json:"monitor,omitempty"    yaml:"monitor,omitempty"`
	Desktop    string         `json:"desktop,omitempty"    yaml:"desktop,omitempty"`
	Maximized  string         `json:"maximized,omitempty"  yaml:"maximized,omitempty"`
	FullScreen bool           `json:"fullscreen,omitempty" yaml:"fullscreen,omitempty"`
	Centered   bool           `json:"centered,omitempty"   yaml:"centered,omitempty"`
	Pinned     bool           `json:"pinned,omitempty"     yaml:"pinned,omitempty"`
	Minimized  bool           `json:"minimized,omitempty"  yaml:"minimized,omitempty"`
	KeepAbove  bool           `json:"keepAbove,omitempty"  yaml:"keepAbove,omitempty"`
	KeepBelow  bool           `json:"keepBelow,omitempty"  yaml:"keepBelow,omitempty"`
}

type Template struct {
	Version string   `json:"version,omitempty" yaml:"version,omitempty"`
	Timeout string   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Presets []Preset `json:"presets"           yaml:"presets"`
}

type layoutFile struct {
	Base string
	File string
	Path string
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
		return errInvalidCommandExpectedStringOrStringArray
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
			return errInvalidCommandExpectedStringArray
		}

		*c = CommandSpec(asSlice)

		return nil
	case yaml.DocumentNode:
		if len(value.Content) != 1 {
			return errInvalidCommandExpectedStringOrStringArray
		}

		return c.UnmarshalYAML(value.Content[0])
	case yaml.MappingNode, yaml.AliasNode:
		return errInvalidCommandExpectedStringOrStringArray
	case 0:
		*c = nil
		return nil
	default:
		return errInvalidCommandExpectedStringOrStringArray
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
	Code     int
	Err      error
	Reported bool
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

func newExitError(code int, err error) *ExitError {
	return &ExitError{Code: code, Err: err, Reported: false}
}

func reportedExitError(code int, err error) *ExitError {
	return &ExitError{Code: code, Err: err, Reported: true}
}

type ScriptPathError struct {
	Reason string
}

func (e *ScriptPathError) Error() string {
	return "unexpected loadScript return: " + e.Reason
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
	Use:   "kwinl",
	Short: "KWin window placement tool",
	Long: `kwinl loads temporary KWin scripts via D-Bus that intercept newly created
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
	Example: `  kwinl place --app org.kde.konsole --geom 50,50,900,700 --timeout 8s --cmd "konsole --separate"
  kwinl place --app org.kde.konsole --geom 0,0,50%,100% --anchor top-left --cmd "konsole"
  kwinl place --match "^Firefox.*Private" --geom 0,0,50%,100% --cmd "firefox --private-window"
  kwinl place --app firefox --match "YouTube" --geom 0,0,50%,100% --cmd firefox`,
	DisableFlagsInUseLine: true,
	SilenceUsage:          true,
	SilenceErrors:         true,
	Args:                  cobra.NoArgs,
	RunE:                  runPlace,
}

var launchCmd = &cobra.Command{
	Use:   "launch [config.yaml|config.yml|config.json|-] [--timeout <duration>]",
	Short: "Batch launch windows from a YAML/JSON template",
	Long: `Reads a template file containing multiple window presets and launches
all specified applications with their configured geometries.

Use "-" to read a template from stdin, or pipe directly with no positional args.`,
	Example: `  kwinl launch layout.yaml
  kwinl launch -
  cat layout.yaml | kwinl launch
  kwinl launch workspace.json --timeout 15s`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLaunch,
}

var layoutsCmd = &cobra.Command{
	Use:   "layouts",
	Short: "Manage saved layouts in ~/.config/kwinl",
}

var layoutsListCmd = &cobra.Command{
	Use:           "list",
	Short:         "List saved layouts from ~/.config/kwinl",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLayoutsList,
}

var layoutsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a saved layout by name",
	Long: `Removes a layout from ~/.config/kwinl.

Name can be a basename (without extension) or an explicit filename.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLayoutsRemove,
}

var layoutsLaunchCmd = &cobra.Command{
	Use:   "launch <name> [--timeout <duration>]",
	Short: "Launch a saved layout by name",
	Long: `Launches a layout from ~/.config/kwinl.

Name can be a basename (without extension) or an explicit filename.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLayoutsLaunch,
}

var captureCmd = &cobra.Command{
	Use:   "capture [layout.yaml|layout.yml|layout.json|-] [flags]",
	Short: "Capture currently open windows into a layout template",
	Long: `Captures the geometry/monitor/desktop of currently open windows and writes a YAML
or JSON template (based on output file extension) suitable for use with "kwinl launch".

By default, only windows with a known application ID are included. App IDs are read from
desktopFileName and fall back to appId when needed. Use --include-unknown
to also capture windows without an application ID (these will be matched by window title).

Maximized and fullscreen states are captured and will be restored when launching.

If --infer-command is enabled (default), each preset uses:
  command: ["gtk-launch", "<app-id>"]
This is a best-effort launcher and may not reproduce multi-window apps exactly.

If a captured preset has no launch command, capture prints a warning and that preset
must be edited before using "kwinl launch".`,
	Example: `  kwinl capture
  kwinl capture -
  kwinl capture layout.yaml
  kwinl capture layout.json --include-unknown
  kwinl capture layout.yml --current-desktop
  kwinl capture layout.yaml --monitor DP-1`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCapture,
}

var validateCmd = &cobra.Command{
	Use:           "validate <layout-file>",
	Short:         "Validate a layout template without launching",
	Long:          `Validates a YAML/JSON layout file for syntax errors, missing fields, and invalid values without launching any windows.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runValidate,
}

var cleanupCmd = &cobra.Command{
	Use:           "cleanup",
	Short:         "Unload orphaned kwinl scripts",
	Long:          `Discovers and unloads KWin scripts matching kwinl-* pattern.`,
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
	placeCmd.Flags().StringVarP(&placeGeomFlag, "geom", "g", "", "geometry as x,y,w,h (or w,h with --centered; values can be pixels or percentages like 50%)")
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
	layoutsLaunchCmd.Flags().StringVarP(&launchTimeoutFlag, "timeout", "t", "", "timeout override (e.g., 10s)")

	captureCmd.Flags().StringVarP(&captureTimeoutFlag, "timeout", "t", "2s", "capture timeout (e.g., 2s, 500ms)")
	captureCmd.Flags().BoolVar(&captureInferCommandFlag, "infer-command", true, "infer a best-effort launcher command using gtk-launch")
	captureCmd.Flags().BoolVarP(&captureIncludeUnknown, "include-unknown", "u", false, "include windows without desktopFileName/appId (matched by title; may require manual command)")
	captureCmd.Flags().BoolVarP(&captureCurrentDesktop, "current-desktop", "d", false, "only capture windows on current desktop")
	captureCmd.Flags().StringVarP(&captureMonitorFilter, "monitor", "M", "", "only capture windows on specified monitor")

	cleanupCmd.Flags().BoolVarP(&cleanupDryRunFlag, "dry-run", "n", false, "list without unloading")

	layoutsCmd.AddCommand(layoutsListCmd)
	layoutsCmd.AddCommand(layoutsRemoveCmd)
	layoutsCmd.AddCommand(layoutsLaunchCmd)

	rootCmd.AddCommand(placeCmd)
	rootCmd.AddCommand(launchCmd)
	rootCmd.AddCommand(layoutsCmd)
	rootCmd.AddCommand(captureCmd)
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(cleanupCmd)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func verbosef(format string, args ...any) {
	if verboseFlag {
		fmt.Fprintf(os.Stderr, "[verbose] "+format+"\n", args...)
	}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("kwinl: ")

	if err := rootCmd.Execute(); err != nil {
		if shouldLogError(err) {
			log.Println(err)
		}

		os.Exit(exitCodeFor(err))
	}
}

func shouldLogError(err error) bool {
	var ee *ExitError
	return !errors.As(err, &ee) || !ee.Reported
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

func removeTempDirWarn(tempDir string) {
	if err := os.RemoveAll(tempDir); err != nil {
		log.Printf("warning: failed to remove temp dir %s: %v", tempDir, err)
	}
}

func closeDBusConnWarn(conn *dbus.Conn) {
	if err := conn.Close(); err != nil {
		log.Printf("warning: failed to close D-Bus connection: %v", err)
	}
}

func initPlaceCallback(conn *dbus.Conn) (*placeReceiver, string, error) {
	recv := &placeReceiver{ch: make(chan placeResult, 1)}
	callbackService := fmt.Sprintf("io.github.kwinl.Place.p%d.r%s", os.Getpid(), generateRandomSuffix())

	verbosef("registering D-Bus service %s", callbackService)

	reply, err := conn.RequestName(callbackService, dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot acquire place D-Bus name %q: %w", callbackService, err))
	}

	if reply != dbus.RequestNameReplyPrimaryOwner {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot acquire place D-Bus name %q: %w", callbackService, errAlreadyOwned))
	}

	if err := conn.Export(recv, dbus.ObjectPath(placeObjectPath), placeIface); err != nil {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot export place D-Bus object: %w", err))
	}

	return recv, callbackService, nil
}

func loadPlaceScriptPath(conn *dbus.Conn, cfg Config) (string, error) {
	scriptPath, err := loadScript(conn, cfg.JSFile, cfg.ScriptName)
	if err == nil {
		return scriptPath, nil
	}

	var warning *ScriptPathError
	if errors.As(err, &warning) {
		log.Printf("warning: %v", warning)
		return scriptPath, nil
	}

	return "", newExitError(exitCodeLoadFailed, fmt.Errorf("loadScript failed: %w", err))
}

func runPlace(cmd *cobra.Command, args []string) error {
	cfg, err := parseAndValidatePlace()
	if err != nil {
		return err
	}

	defer removeTempDirWarn(cfg.TempDir)

	verbosef("connecting to session D-Bus")

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return newExitError(exitCodeDBusFailure, fmt.Errorf("cannot connect to session D-Bus: %w", err))
	}

	defer closeDBusConnWarn(conn)

	recv, callbackService, err := initPlaceCallback(conn)
	if err != nil {
		return err
	}

	verbosef("writing script to %s", cfg.JSFile)

	if err := writeJSFile(cfg, callbackService, placeKeepFlag); err != nil {
		return fmt.Errorf("failed to write JS file: %w", err)
	}

	verbosef("loading script %s", cfg.ScriptName)

	scriptPath, err := loadPlaceScriptPath(conn, cfg)
	if err != nil {
		return err
	}

	verbosef("script loaded at path %s", scriptPath)

	defer unloadScript(conn, cfg.ScriptName)

	verbosef("running script")

	if err := runScript(conn, cfg.ScriptName, scriptPath); err != nil {
		return newExitError(exitCodeLoadFailed, err)
	}

	verbosef("launching command: %v", cfg.Cmd)

	cmdProc, err := launchCommand(cfg.Cmd)
	if err != nil {
		return newExitError(exitCodeLaunchFailed, fmt.Errorf("failed to launch command: %w", err))
	}

	return waitForPlaceCommand(cfg.Timeout, recv.ch, cmdProc, placeKeepFlag)
}

func waitForPlaceCommand(timeout time.Duration, results <-chan placeResult, cmdProc *exec.Cmd, keepMode bool) error {
	cmds := []*exec.Cmd{cmdProc}

	waitAndCleanup := func() error {
		if err := waitForPlacement(timeout, results, cmds); err != nil {
			if shouldCleanupStartedCommands(err) {
				cleanupStartedCommands(cmds)
			}

			return err
		}

		return nil
	}

	if keepMode {
		verbosef("keep mode: waiting for initial placement callback (timeout: %s)", timeout)

		if err := waitAndCleanup(); err != nil {
			return err
		}

		verbosef("keep mode: waiting indefinitely for SIGINT/SIGTERM")

		return waitForSignal(cmds)
	}

	verbosef("waiting for placement callback (timeout: %s)", timeout)

	return waitAndCleanup()
}

func parsePlaceCommandFlagValue() ([]string, error) {
	cmdStr := strings.TrimSpace(placeCommandFlag)
	if cmdStr == "" {
		return nil, &CommandError{Reason: "missing --cmd command string"}
	}

	cmdSlice, err := splitCommand(cmdStr)
	if err != nil {
		return nil, &CommandError{Reason: fmt.Sprintf("invalid --cmd value: %v", err)}
	}

	if len(cmdSlice) == 0 {
		return nil, &CommandError{Reason: "command is empty after parsing --cmd"}
	}

	return cmdSlice, nil
}

func validatePlaceSelectorsAndAnchor() error {
	if placeAppFlag == "" && placeMatchFlag == "" {
		return &ValidationError{
			Field:   "app/match",
			Value:   "",
			Message: "at least one of --app or --match is required",
		}
	}

	if placeMatchFlag != "" {
		if _, err := regexp.Compile(placeMatchFlag); err != nil {
			return &ValidationError{
				Field:   "match",
				Value:   placeMatchFlag,
				Message: "invalid regex: " + err.Error(),
			}
		}
	}

	if !isValidAnchor(placeAnchorFlag) {
		return &ValidationError{
			Field:   "anchor",
			Value:   placeAnchorFlag,
			Message: "valid anchors: " + strings.Join(validAnchors, ", "),
		}
	}

	return nil
}

func parseAndValidatePlace() (Config, error) {
	cmdSlice, err := parsePlaceCommandFlagValue()
	if err != nil {
		return Config{}, err
	}

	if err := validatePlaceSelectorsAndAnchor(); err != nil {
		return Config{}, err
	}

	geom, err := parsePlaceGeom(placeGeomFlag, placeCenteredFlag)
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

	tempDir, err := os.MkdirTemp("", "kwinl-*")
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
		ScriptPath: "",
		TempDir:    tempDir,
		JSFile:     jsFile,
	}, nil
}

func runLaunch(cmd *cobra.Command, args []string) error {
	template, err := parseLaunchTemplateInput(args, cmd.InOrStdin())
	if err != nil {
		return err
	}

	return runLaunchFromTemplate(template)
}

func runLayoutsList(cmd *cobra.Command, args []string) error {
	layoutsDir, err := ensureLayoutsDir()
	if err != nil {
		return err
	}

	layouts, err := collectLayoutFiles(layoutsDir)
	if err != nil {
		return fmt.Errorf("failed to list layouts in %q: %w", layoutsDir, err)
	}

	for _, item := range formatLayoutList(layouts) {
		fmt.Println(item)
	}

	return nil
}

func runLayoutsRemove(cmd *cobra.Command, args []string) error {
	layout, err := resolveLayoutByName(args[0])
	if err != nil {
		return err
	}

	if err := os.Remove(layout.Path); err != nil {
		return fmt.Errorf("failed to remove layout %q: %w", layout.File, err)
	}

	fmt.Printf("removed: %s\n", layout.File)

	return nil
}

func runLayoutsLaunch(cmd *cobra.Command, args []string) error {
	layout, err := resolveLayoutByName(args[0])
	if err != nil {
		return err
	}

	return runLaunchFromTemplatePath(layout.Path)
}

func runLaunchFromTemplatePath(templatePath string) error {
	template, err := parseTemplate(templatePath)
	if err != nil {
		return err
	}

	return runLaunchFromTemplate(template)
}

func runLaunchFromTemplate(template Template) error {
	if err := validateTemplate(template); err != nil {
		return err
	}

	timeout, err := determineLaunchTimeout(template)
	if err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp("", "kwinl-launch-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}

	defer removeTempDirWarn(tempDir)

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return newExitError(exitCodeDBusFailure, fmt.Errorf("cannot connect to session D-Bus: %w", err))
	}

	defer closeDBusConnWarn(conn)

	recv, callbackService, err := initLaunchCallback(conn, len(template.Presets))
	if err != nil {
		return err
	}

	var (
		scriptNames []string
		cmdProcs    []*exec.Cmd
	)

	launchPhaseComplete := false

	defer func() {
		unloadAllScripts(conn, scriptNames)

		if !launchPhaseComplete {
			cleanupStartedCommands(cmdProcs)
		}
	}()

	scriptNames, cmdProcs, err = launchTemplatePresets(conn, recv.ch, template.Presets, tempDir, callbackService, timeout)
	if err != nil {
		return err
	}

	launchPhaseComplete = true

	return waitAndCleanup(timeout, cmdProcs)
}

func writeLaunchPresetJSFile(presetRun launchPresetRun) error {
	js := generateJS(presetRun.JSConfig)
	if err := os.WriteFile(presetRun.JSFile, []byte(js), 0o600); err != nil {
		return &PresetError{Preset: presetRun.Label, Field: "", Err: fmt.Errorf("failed to write script: %w", err)}
	}

	return nil
}

func runLaunchPresetScriptAndCommand(conn *dbus.Conn, presetRun launchPresetRun, scriptPath string) (*exec.Cmd, error) {
	if err := runScript(conn, presetRun.ScriptName, scriptPath); err != nil {
		return nil, newExitError(exitCodeLoadFailed, err)
	}

	cmdProc, err := launchCommand(presetRun.Command)
	if err != nil {
		return nil, newExitError(exitCodeLaunchFailed, fmt.Errorf("failed to launch command for preset %s: %w", presetRun.Label, err))
	}

	return cmdProc, nil
}

func launchTemplatePresets(
	conn *dbus.Conn,
	results <-chan placeResult,
	presets []Preset,
	tempDir, callbackService string,
	timeout time.Duration,
) ([]string, []*exec.Cmd, error) {
	scriptNames := make([]string, 0, len(presets))
	cmdProcs := make([]*exec.Cmd, 0, len(presets))

	for i, preset := range presets {
		presetRun, err := buildLaunchPresetRun(i, preset, tempDir, callbackService)
		if err != nil {
			return nil, nil, err
		}

		if err := writeLaunchPresetJSFile(presetRun); err != nil {
			return nil, nil, err
		}

		scriptPath, err := loadLaunchPresetScript(conn, presetRun)
		if err != nil {
			return nil, nil, err
		}

		scriptNames = append(scriptNames, presetRun.ScriptName)

		cmdProc, err := runLaunchPresetScriptAndCommand(conn, presetRun, scriptPath)
		if err != nil {
			return nil, nil, err
		}

		cmdProcs = append(cmdProcs, cmdProc)

		if err := waitForLaunchPresetCallback(results, presetRun.Label, presetRun.ScriptName, timeout); err != nil {
			return nil, nil, err
		}
	}

	return scriptNames, cmdProcs, nil
}

type launchPresetRun struct {
	Label      string
	ScriptName string
	JSFile     string
	Command    []string
	JSConfig   jsPlacementConfig
}

func buildLaunchPresetRun(index int, preset Preset, tempDir, callbackService string) (launchPresetRun, error) {
	label := presetLabel(index, preset.Name)
	tile := normalizeTileValue(preset.Tile)

	anchor := preset.Anchor
	if anchor == "" {
		anchor = "top-left"
	}

	geom, err := resolveLaunchPresetGeometry(preset, tile)
	if err != nil {
		return launchPresetRun{}, &PresetError{Preset: label, Field: "geometry", Err: err}
	}

	if preset.Centered {
		anchor = "center"
		geom.X = GeomValue{Value: 50, Percent: true}
		geom.Y = GeomValue{Value: 50, Percent: true}
	}

	scriptName := fmt.Sprintf("kwinl-%d-%d-%s", os.Getpid(), index, generateRandomSuffix())
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
		CallbackToken:   scriptName,
		KeepMode:        false,
	}

	return launchPresetRun{
		Label:      label,
		ScriptName: scriptName,
		JSFile:     jsFile,
		Command:    []string(preset.Command),
		JSConfig:   jsCfg,
	}, nil
}

func presetLabel(index int, name string) string {
	if name != "" {
		return name
	}

	return fmt.Sprintf("#%d", index)
}

func resolveLaunchPresetGeometry(preset Preset, tile string) (ParsedGeometry, error) {
	geomProvided := hasAllPresetGeometry(preset.Geometry)
	if geomProvided || tile == "" {
		return parsePresetGeometry(preset.Geometry, preset.Centered)
	}

	return defaultLaunchPresetGeometry(), nil
}

func defaultLaunchPresetGeometry() ParsedGeometry {
	// Geometry is optional for quick-tiling presets.
	return ParsedGeometry{
		X: GeomValue{Value: 0, Percent: false},
		Y: GeomValue{Value: 0, Percent: false},
		W: GeomValue{Value: 100, Percent: true},
		H: GeomValue{Value: 100, Percent: true},
	}
}

func initLaunchCallback(conn *dbus.Conn, presetCount int) (*placeReceiver, string, error) {
	recv := &placeReceiver{ch: make(chan placeResult, presetCount+2)}
	callbackService := fmt.Sprintf("io.github.kwinl.Place.p%d.r%s", os.Getpid(), generateRandomSuffix())

	reply, err := conn.RequestName(callbackService, dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot acquire launch place D-Bus name %q: %w", callbackService, err))
	}

	if reply != dbus.RequestNameReplyPrimaryOwner {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot acquire launch place D-Bus name %q: %w", callbackService, errAlreadyOwned))
	}

	if err := conn.Export(recv, dbus.ObjectPath(placeObjectPath), placeIface); err != nil {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot export launch place D-Bus object: %w", err))
	}

	return recv, callbackService, nil
}

func loadLaunchPresetScript(conn *dbus.Conn, preset launchPresetRun) (string, error) {
	scriptPath, err := loadScript(conn, preset.JSFile, preset.ScriptName)
	if err == nil {
		return scriptPath, nil
	}

	var warning *ScriptPathError
	if errors.As(err, &warning) {
		log.Printf("warning: preset %s: %v", preset.Label, warning)
		return scriptPath, nil
	}

	return "", newExitError(exitCodeLoadFailed, fmt.Errorf("loadScript failed for preset %s: %w", preset.Label, err))
}

func nextLaunchPresetCallback(ch <-chan placeResult, callbackToken string, timeout time.Duration) (placeResult, bool) {
	perPresetWait := timeout

	const maxPerPresetWait = 2 * time.Second
	if perPresetWait <= 0 || perPresetWait > maxPerPresetWait {
		perPresetWait = maxPerPresetWait
	}

	timer := time.NewTimer(perPresetWait)

	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	for {
		select {
		case result := <-ch:
			if result.CallbackToken != callbackToken {
				verbosef("launch preset callback token mismatch: expected=%s got=%s windowID=%s caption=%s geometry=%s",
					callbackToken, result.CallbackToken, result.WindowID, result.Caption, result.Geometry)

				continue
			}

			return result, true
		case <-timer.C:
			return placeResult{
				CallbackToken: "",
				Success:       false,
				WindowID:      "",
				Caption:       "",
				Geometry:      "",
				Message:       "",
			}, false
		}
	}
}

func placementCallbackError(result placeResult) error {
	if result.Success {
		return nil
	}

	message := strings.TrimSpace(result.Message)
	if message == "" {
		message = "placement failed"
	}

	return &ValidationError{
		Field:   "target",
		Value:   "",
		Message: message,
	}
}

func placementTimeoutError(timeout time.Duration) error {
	return newExitError(exitCodeLoadFailed, fmt.Errorf("placement timed out after %s (%w)", timeout, errNoPlacementCallbackReceived))
}

func waitForLaunchPresetCallback(ch <-chan placeResult, label, callbackToken string, timeout time.Duration) error {
	result, ok := nextLaunchPresetCallback(ch, callbackToken, timeout)
	if ok {
		verbosef("launch preset %s placement callback: success=%v windowID=%s caption=%s geometry=%s",
			label, result.Success, result.WindowID, result.Caption, result.Geometry)

		if err := placementCallbackError(result); err != nil {
			return presetErr(label, err)
		}

		return nil
	}

	perPresetWait := timeout

	const maxPerPresetWait = 2 * time.Second
	if perPresetWait <= 0 || perPresetWait > maxPerPresetWait {
		perPresetWait = maxPerPresetWait
	}

	verbosef("launch preset %s placement callback timeout after %s; continuing",
		label, perPresetWait)

	return presetErr(label, placementTimeoutError(perPresetWait))
}

func parseLaunchTemplateInput(args []string, stdin io.Reader) (Template, error) {
	if len(args) > 1 {
		return Template{}, &ValidationError{Field: "launch", Value: "", Message: "expected at most one layout argument"}
	}

	if len(args) == 1 {
		target := strings.TrimSpace(args[0])
		if target == "" {
			return Template{}, &ValidationError{Field: "layout", Value: "", Message: "path is required"}
		}

		if target == "-" {
			return parseTemplateFromReader("stdin", stdin)
		}

		return parseTemplate(target)
	}

	hasPipedInput, err := stdinHasPipedData(stdin)
	if err != nil {
		return Template{}, fmt.Errorf("failed to inspect stdin: %w", err)
	}

	if !hasPipedInput {
		return Template{}, &ValidationError{
			Field:   "layout",
			Value:   "",
			Message: "expected a layout file path or piped stdin (use '-' to read stdin explicitly)",
		}
	}

	return parseTemplateFromReader("stdin", stdin)
}

func stdinHasPipedData(stdin io.Reader) (bool, error) {
	f, ok := stdin.(*os.File)
	if !ok {
		// Non-file readers are usually intentional (tests or injected input).
		return true, nil
	}

	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat stdin: %w", err)
	}

	return (info.Mode() & os.ModeCharDevice) == 0, nil
}

func parseTemplateFromReader(source string, r io.Reader) (Template, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Template{}, &TemplateError{Path: source, Reason: "failed to read stdin", Err: err}
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return Template{}, &TemplateError{Path: source, Reason: "empty input", Err: nil}
	}

	jsonTemplate, jsonErr := parseTemplateJSON(source, data)
	if jsonErr == nil {
		return jsonTemplate, nil
	}

	yamlTemplate, yamlErr := parseTemplateYAML(source, data)
	if yamlErr == nil {
		return yamlTemplate, nil
	}

	return Template{}, &TemplateError{
		Path:   source,
		Reason: "invalid YAML/JSON",
		Err:    fmt.Errorf("json parse error: %w; yaml parse error: %w", jsonErr, yamlErr),
	}
}

func parseTemplateJSON(source string, data []byte) (Template, error) {
	var template Template
	if err := json.Unmarshal(data, &template); err != nil {
		return Template{}, &TemplateError{Path: source, Reason: "invalid JSON", Err: err}
	}

	return template, nil
}

func parseTemplateYAML(source string, data []byte) (Template, error) {
	var template Template
	if err := yaml.Unmarshal(data, &template); err != nil {
		return Template{}, &TemplateError{Path: source, Reason: "invalid YAML", Err: err}
	}

	return template, nil
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
	CallbackToken string
	Success       bool
	WindowID      string
	Caption       string
	Geometry      string
	Message       string
}

//nolint:unparam // D-Bus method signature requires *dbus.Error return; always nil.
func (r *placeReceiver) Placed(callbackToken string, success bool, windowID, caption, geom, message string) (bool, *dbus.Error) {
	select {
	case r.ch <- placeResult{
		CallbackToken: callbackToken,
		Success:       success,
		WindowID:      windowID,
		Caption:       caption,
		Geometry:      geom,
		Message:       message,
	}:
	default:
	}

	return true, nil
}

func runCapture(cmd *cobra.Command, args []string) error {
	outPath, format, err := parseCaptureOutputTarget(args)
	if err != nil {
		return err
	}

	timeout, err := parseTimeout(captureTimeoutFlag)
	if err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp("", "kwinl-capture-*")
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
		return newExitError(exitCodeDBusFailure, fmt.Errorf("cannot connect to session D-Bus: %w", err))
	}

	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("warning: failed to close D-Bus connection: %v", err)
		}
	}()

	recv, serviceName, err := initCaptureCallback(conn)
	if err != nil {
		return err
	}

	scriptName, err := startCaptureScript(conn, tempDir, serviceName)
	if err != nil {
		return err
	}
	defer unloadScript(conn, scriptName)

	payload, err := waitForCapturePayload(recv.ch, timeout)
	if err != nil {
		return err
	}

	template, err := buildTemplateFromCapturePayload(payload)
	if err != nil {
		return newExitError(exitCodeLoadFailed, fmt.Errorf("invalid capture payload: %w", err))
	}

	warnCaptureNonLaunchablePresets(template)

	data, err := marshalCaptureTemplate(template, format)
	if err != nil {
		return err
	}

	return writeCaptureOutput(outPath, data)
}

func initCaptureCallback(conn *dbus.Conn) (*captureReceiver, string, error) {
	recv := &captureReceiver{ch: make(chan string, 1)}
	serviceName := fmt.Sprintf("io.github.kwinl.Capture.p%d.r%s", os.Getpid(), generateRandomSuffix())

	reply, err := conn.RequestName(serviceName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot acquire capture D-Bus name %q: %w", serviceName, err))
	}

	if reply != dbus.RequestNameReplyPrimaryOwner {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot acquire capture D-Bus name %q: %w", serviceName, errAlreadyOwned))
	}

	if err := conn.Export(recv, dbus.ObjectPath(captureObjectPath), captureIface); err != nil {
		return nil, "", newExitError(exitCodeDBusFailure, fmt.Errorf("cannot export capture D-Bus object: %w", err))
	}

	return recv, serviceName, nil
}

func startCaptureScript(conn *dbus.Conn, tempDir, serviceName string) (string, error) {
	scriptName := fmt.Sprintf("kwinl-capture-%d-%s", os.Getpid(), generateRandomSuffix())
	jsFile := filepath.Join(tempDir, scriptName+".js")
	opts := captureOptions{
		InferCommand:   captureInferCommandFlag,
		IncludeUnknown: captureIncludeUnknown,
		CurrentDesktop: captureCurrentDesktop,
		MonitorFilter:  captureMonitorFilter,
	}
	js := generateCaptureJS(scriptName, serviceName, opts)

	if err := os.WriteFile(jsFile, []byte(js), 0o600); err != nil {
		return "", fmt.Errorf("failed to write capture JS file: %w", err)
	}

	scriptPath, err := loadScript(conn, jsFile, scriptName)
	if err != nil {
		var warning *ScriptPathError
		if errors.As(err, &warning) {
			log.Printf("warning: %v", warning)
		} else {
			return "", newExitError(exitCodeLoadFailed, fmt.Errorf("loadScript failed: %w", err))
		}
	}

	if err := runScript(conn, scriptName, scriptPath); err != nil {
		return "", newExitError(exitCodeLoadFailed, err)
	}

	return scriptName, nil
}

func waitForCapturePayload(ch <-chan string, timeout time.Duration) (string, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case payload := <-ch:
		return payload, nil
	case <-timer.C:
		return "", newExitError(exitCodeLoadFailed, fmt.Errorf("capture timed out after %s (%w)", timeout, errNoCapturePayloadReceived))
	}
}

func marshalCaptureTemplate(template Template, format string) ([]byte, error) {
	if format == formatJSON {
		data, err := json.MarshalIndent(template, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal JSON: %w", err)
		}

		return append(data, '\n'), nil
	}

	return marshalTemplateYAML(template)
}

func writeCaptureOutput(outPath string, data []byte) error {
	if outPath == "-" {
		_, _ = os.Stdout.Write(data)
		return nil
	}

	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write %q: %w", outPath, err)
	}

	return nil
}

func parseCaptureOutputTarget(args []string) (string, string, error) {
	outPath := "-"
	if len(args) > 0 {
		outPath = strings.TrimSpace(args[0])
	}

	if outPath == "" {
		outPath = "-"
	}

	format := "yaml"

	if outPath != "-" {
		ext := strings.ToLower(filepath.Ext(outPath))
		switch ext {
		case ".yaml", ".yml":
			format = "yaml"
		case ".json":
			format = formatJSON
		default:
			return "", "", &ValidationError{
				Field:   "output",
				Value:   outPath,
				Message: "expected .yaml/.yml/.json output file (or '-' for stdout)",
			}
		}
	}

	return outPath, format, nil
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
	fmt.Fprintf(os.Stderr, "✗ %s validation failed:\n", filepath.Base(path))
	fmt.Fprintf(os.Stderr, "  %v\n", err)

	return reportedExitError(exitCodeUsage, err)
}

func runCleanup(cmd *cobra.Command, args []string) error {
	verbosef("connecting to session D-Bus")

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return newExitError(exitCodeDBusFailure, fmt.Errorf("cannot connect to session D-Bus: %w", err))
	}

	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("warning: failed to close D-Bus connection: %v", err)
		}
	}()

	scripts, err := discoverKwinLayoutScripts(conn)

	scripts, err = validateCleanupDiscovery(scripts, err)
	if err != nil {
		return err
	}

	if len(scripts) == 0 {
		fmt.Println("no orphaned kwinl scripts found")
		return nil
	}

	count := 0

	for _, scriptName := range scripts {
		if cleanupDryRunFlag {
			fmt.Printf("would unload: %s\n", scriptName)
		} else {
			verbosef("unloading script %s", scriptName)

			obj := conn.Object(dbusDestination, dbus.ObjectPath(dbusScriptingPath))

			call := obj.Call(dbusScriptingIface+".unloadScript", 0, scriptName)
			if call.Err != nil {
				verbosef("unload failed for %s: %v", scriptName, call.Err)
				continue
			}

			fmt.Printf("unloaded: %s\n", scriptName)
		}

		count++
	}

	fmt.Printf("total: %d script(s)\n", count)

	return nil
}

func validateCleanupDiscovery(scripts []string, err error) ([]string, error) {
	if err == nil {
		return scripts, nil
	}

	return nil, newExitError(exitCodeDBusFailure, err)
}

func discoverKwinLayoutScripts(conn *dbus.Conn) ([]string, error) {
	obj := conn.Object(dbusDestination, dbus.ObjectPath(dbusScriptingPath))

	var xmlData string

	err := obj.Call("org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&xmlData)
	if err != nil {
		return nil, fmt.Errorf("introspection failed: %w", err)
	}

	verbosef("introspection result: %d bytes", len(xmlData))

	var scripts []string

	scriptPathRe := regexp.MustCompile(`<node name="(Script\d+)"`)
	matches := scriptPathRe.FindAllStringSubmatch(xmlData, -1)

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}

		nodeName := match[1]
		scriptPath := fmt.Sprintf("%s/%s", dbusScriptingPath, nodeName)

		verbosef("checking script at %s", scriptPath)
		scriptObj := conn.Object(dbusDestination, dbus.ObjectPath(scriptPath))

		var pluginName dbus.Variant

		err := scriptObj.Call("org.freedesktop.DBus.Properties.Get", 0, dbusScriptIface, "pluginName").Store(&pluginName)
		if err != nil {
			verbosef("could not get pluginName for %s: %v", scriptPath, err)
			continue
		}

		name, ok := pluginName.Value().(string)
		if !ok {
			verbosef("pluginName not a string for %s", scriptPath)
			continue
		}

		verbosef("found script: %s", name)

		if strings.HasPrefix(name, "kwinl-") {
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
	normalizeCaptureNumericScalars(&node)

	data, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal YAML: %w", err)
	}

	return unquoteYAMLKeyY(data), nil
}

func setCommandFlowStyle(node *yaml.Node) {
	switch node.Kind {
	case yaml.DocumentNode:
		setCommandFlowStyleNodes(node.Content)
	case yaml.MappingNode:
		setCommandFlowStyleMapping(node)
	case yaml.SequenceNode:
		setCommandFlowStyleNodes(node.Content)
	case yaml.ScalarNode:
		return
	case yaml.AliasNode:
		if node.Alias != nil {
			setCommandFlowStyle(node.Alias)
		}
	}
}

var captureNumericScalarKeys = map[string]struct{}{
	"x":      {},
	"y":      {},
	"width":  {},
	"height": {},
}

func normalizeCaptureNumericScalars(node *yaml.Node) {
	switch node.Kind {
	case yaml.DocumentNode:
		normalizeCaptureNumericScalarsNodes(node.Content)
	case yaml.SequenceNode:
		normalizeCaptureNumericScalarsNodes(node.Content)
	case yaml.MappingNode:
		normalizeCaptureNumericScalarsMapping(node)
	case yaml.ScalarNode:
		return
	case yaml.AliasNode:
		if node.Alias != nil {
			normalizeCaptureNumericScalars(node.Alias)
		}
	}
}

func normalizeCaptureNumericScalarsNodes(nodes []*yaml.Node) {
	for _, c := range nodes {
		normalizeCaptureNumericScalars(c)
	}
}

func normalizeCaptureNumericScalarsMapping(node *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		v := node.Content[i+1]
		coerceCaptureNumericScalar(k, v)
		normalizeCaptureNumericScalars(v)
	}
}

func coerceCaptureNumericScalar(key, value *yaml.Node) {
	if key.Kind != yaml.ScalarNode || value.Kind != yaml.ScalarNode {
		return
	}

	if !isCaptureNumericScalarKey(key.Value) {
		return
	}

	if !isYAMLIntegerLikeString(value.Value) {
		return
	}

	value.Tag = "!!int"
	value.Style = 0
}

func isCaptureNumericScalarKey(v string) bool {
	_, ok := captureNumericScalarKeys[v]
	return ok
}

func isYAMLIntegerLikeString(v string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(v))
	return err == nil
}

func setCommandFlowStyleNodes(nodes []*yaml.Node) {
	for _, c := range nodes {
		setCommandFlowStyle(c)
	}
}

func setCommandFlowStyleMapping(node *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]

		v := node.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == "command" && v.Kind == yaml.SequenceNode {
			v.Style = yaml.FlowStyle
		}

		setCommandFlowStyle(v)
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
		return Template{}, fmt.Errorf("parse capture payload JSON: %w", err)
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
			Command:    nil,
			Anchor:     p.Anchor,
			Monitor:    p.Monitor,
			Desktop:    p.Desktop,
			Tile:       "",
			Maximized:  p.Maximized,
			FullScreen: p.FullScreen,
			Centered:   false,
			Pinned:     false,
			Minimized:  false,
			KeepAbove:  false,
			KeepBelow:  false,
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
		return Template{}, &ValidationError{Field: "capture", Value: "", Message: "no capturable windows found"}
	}

	return Template{Version: version, Timeout: "", Presets: presets}, nil
}

func warnCaptureNonLaunchablePresets(template Template) {
	missingCommandLabels := captureMissingCommandPresetLabels(template)
	if len(missingCommandLabels) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr,
		"warning: captured %d preset(s) without command; \"kwinl launch\" will reject them: %s\n",
		len(missingCommandLabels), strings.Join(missingCommandLabels, ", "))
	fmt.Fprintln(os.Stderr,
		"warning: add command manually or recapture with --infer-command and identifiable app IDs")
}

func captureMissingCommandPresetLabels(template Template) []string {
	labels := make([]string, 0, len(template.Presets))
	for i, p := range template.Presets {
		if len(p.Command) == 0 {
			labels = append(labels, presetLabel(i, p.Name))
		}
	}

	return labels
}

func isSupportedLayoutExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

func getLayoutsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve user home directory: %w", err)
	}

	return filepath.Join(home, ".config", "kwinl"), nil
}

func ensureLayoutsDir() (string, error) {
	layoutsDir, err := getLayoutsDir()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(layoutsDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create layouts directory %q: %w", layoutsDir, err)
	}

	return layoutsDir, nil
}

func collectLayoutFiles(dir string) ([]layoutFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read layouts directory %q: %w", dir, err)
	}

	layouts := make([]layoutFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		fileName := entry.Name()

		ext := filepath.Ext(fileName)
		if !isSupportedLayoutExt(ext) {
			continue
		}

		layouts = append(layouts, layoutFile{
			Base: strings.TrimSuffix(fileName, ext),
			File: fileName,
			Path: filepath.Join(dir, fileName),
		})
	}

	slices.SortFunc(layouts, func(a, b layoutFile) int {
		if cmp := strings.Compare(a.Base, b.Base); cmp != 0 {
			return cmp
		}

		return strings.Compare(a.File, b.File)
	})

	return layouts, nil
}

func formatLayoutList(layouts []layoutFile) []string {
	baseCount := make(map[string]int, len(layouts))
	for _, layout := range layouts {
		baseCount[layout.Base]++
	}

	items := make([]string, 0, len(layouts))
	for _, layout := range layouts {
		if baseCount[layout.Base] > 1 {
			items = append(items, fmt.Sprintf("%s (%s)", layout.Base, layout.File))
			continue
		}

		items = append(items, layout.Base)
	}

	return items
}

func resolveLayoutByName(name string) (layoutFile, error) {
	layoutsDir, err := getLayoutsDir()
	if err != nil {
		return layoutFile{}, err
	}

	return resolveLayoutByNameInDir(layoutsDir, name)
}

func resolveLayoutByNameInDir(layoutsDir, name string) (layoutFile, error) {
	name, err := normalizeLayoutLookupName(name)
	if err != nil {
		return layoutFile{}, err
	}

	layout, handled, err := resolveLayoutByExactFilename(layoutsDir, name)
	if err != nil {
		return layoutFile{}, err
	}

	if handled {
		return layout, nil
	}

	return resolveLayoutByBasename(layoutsDir, name)
}

func normalizeLayoutLookupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", &ValidationError{Field: "layout", Value: "", Message: "name is required"}
	}

	if filepath.Base(name) != name {
		return "", &ValidationError{Field: "layout", Value: name, Message: "must be a layout name, not a path"}
	}

	return name, nil
}

func resolveLayoutByExactFilename(layoutsDir, name string) (layoutFile, bool, error) {
	ext := filepath.Ext(name)
	if ext == "" {
		return layoutFile{Base: "", File: "", Path: ""}, false, nil
	}

	if !isSupportedLayoutExt(ext) {
		return layoutFile{}, true, &ValidationError{
			Field:   "layout",
			Value:   name,
			Message: "unsupported extension (expected .yaml, .yml, or .json)",
		}
	}

	path := filepath.Join(layoutsDir, name)

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return layoutFile{}, true, layoutNotFoundError(layoutsDir, name)
		}

		return layoutFile{}, true, fmt.Errorf("failed to access layout %q: %w", name, err)
	}

	if info.IsDir() {
		return layoutFile{}, true, &ValidationError{
			Field:   "layout",
			Value:   name,
			Message: "refers to a directory",
		}
	}

	return layoutFile{
		Base: strings.TrimSuffix(name, ext),
		File: name,
		Path: path,
	}, true, nil
}

func resolveLayoutByBasename(layoutsDir, name string) (layoutFile, error) {
	layouts, err := collectLayoutFiles(layoutsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return layoutFile{}, layoutNotFoundError(layoutsDir, name)
		}

		return layoutFile{}, fmt.Errorf("failed to list layouts in %q: %w", layoutsDir, err)
	}

	matches := make([]layoutFile, 0, 2)

	for _, layout := range layouts {
		if layout.Base == name {
			matches = append(matches, layout)
		}
	}

	if len(matches) == 0 {
		return layoutFile{}, layoutNotFoundError(layoutsDir, name)
	}

	if len(matches) == 1 {
		return matches[0], nil
	}

	fileNames := make([]string, 0, len(matches))
	for _, m := range matches {
		fileNames = append(fileNames, m.File)
	}

	return layoutFile{}, &ValidationError{
		Field:   "layout",
		Value:   name,
		Message: "ambiguous name; matches: " + strings.Join(fileNames, ", ") + " (use an exact filename)",
	}
}

func layoutNotFoundError(layoutsDir, name string) error {
	return &ValidationError{
		Field:   "layout",
		Value:   name,
		Message: fmt.Sprintf("not found in %q", layoutsDir),
	}
}

func parseTemplate(path string) (Template, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if !isSupportedLayoutExt(ext) {
		return Template{}, &TemplateError{
			Path:   path,
			Reason: "unsupported file extension (expected .yaml, .yml, or .json)",
			Err:    nil,
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Template{}, &TemplateError{Path: path, Reason: "failed to read file", Err: err}
	}

	if ext == ".json" {
		return parseTemplateJSON(path, data)
	}

	return parseTemplateYAML(path, data)
}

var (
	validMaximizedValues = []string{"", "horizontal", "vertical", "both"}
	validTileValues      = []string{
		"",
		"left", "right", "top", "bottom",
		"top-left", "top-right", "bottom-left", "bottom-right",
	}
)

func validateTemplate(t Template) error {
	if len(t.Presets) == 0 {
		return &ValidationError{Field: "presets", Value: "", Message: "at least one preset is required"}
	}

	for i, p := range t.Presets {
		if err := validatePreset(i, p); err != nil {
			return err
		}
	}

	return nil
}

func validatePreset(index int, p Preset) error {
	label := presetLabel(index, p.Name)

	if err := validatePresetIdentity(label, p); err != nil {
		return err
	}

	tile, err := validatePresetTile(label, p)
	if err != nil {
		return err
	}

	geomComplete, err := validatePresetGeometryRequirements(label, p, tile)
	if err != nil {
		return err
	}

	if err := validatePresetGeometryValues(label, p, geomComplete); err != nil {
		return err
	}

	if err := validatePresetAnchor(label, p); err != nil {
		return err
	}

	return validatePresetMaximized(label, p)
}

func validatePresetIdentity(label string, p Preset) error {
	if strings.TrimSpace(p.App) == "" && strings.TrimSpace(p.Match) == "" {
		return presetErr(label, &ValidationError{Field: "app/match", Value: "", Message: "either app or match is required"})
	}

	if strings.TrimSpace(p.Match) != "" {
		if _, err := regexp.Compile(p.Match); err != nil {
			return presetErr(label, &ValidationError{Field: "match", Value: p.Match, Message: "invalid regex: " + err.Error()})
		}
	}

	if len(p.Command) == 0 {
		return presetErr(label, &ValidationError{Field: "command", Value: "", Message: "required field is missing"})
	}

	if strings.TrimSpace(p.Command[0]) == "" {
		return presetErr(label, &ValidationError{Field: "command", Value: "", Message: "executable must not be empty"})
	}

	return nil
}

func validatePresetTile(label string, p Preset) (string, error) {
	tile := normalizeTileValue(p.Tile)
	if !isValidTile(tile) {
		return "", presetErr(label, &ValidationError{
			Field:   "tile",
			Value:   p.Tile,
			Message: "valid values: left, right, top, bottom, top-left, top-right, bottom-left, bottom-right (or empty)",
		})
	}

	if tile == "" {
		return tile, nil
	}

	if p.Centered {
		return "", presetErr(label, &ValidationError{Field: "centered", Value: "", Message: "cannot be combined with tile"})
	}

	if p.Maximized != "" {
		return "", presetErr(label, &ValidationError{Field: "maximized", Value: "", Message: "cannot be combined with tile"})
	}

	if p.FullScreen {
		return "", presetErr(label, &ValidationError{Field: "fullscreen", Value: "", Message: "cannot be combined with tile"})
	}

	return tile, nil
}

func validatePresetGeometryRequirements(label string, p Preset, tile string) (bool, error) {
	geomProvided := hasAnyPresetGeometry(p.Geometry)

	geomComplete := hasRequiredPresetGeometry(p.Geometry, p.Centered)
	if geomProvided && !geomComplete {
		message := "x, y, width, height must all be set together"
		if p.Centered {
			message = "width and height must both be set when centered is true"
		}

		return false, presetErr(label, &ValidationError{
			Field:   "geometry",
			Value:   "",
			Message: message,
		})
	}

	if !geomComplete && tile == "" {
		return false, presetErr(label, &ValidationError{Field: "geometry", Value: "", Message: "required unless tile is set"})
	}

	return geomComplete, nil
}

func validatePresetGeometryValues(label string, p Preset, geomComplete bool) error {
	if !geomComplete {
		return nil
	}

	geom, err := parsePresetGeometry(p.Geometry, p.Centered)
	if err != nil {
		return &PresetError{Preset: label, Field: "geometry", Err: err}
	}

	if geom.W.Value <= 0 {
		return presetErr(label, &GeometryError{Component: "width", Value: "", Reason: "must be > 0"})
	}

	if geom.H.Value <= 0 {
		return presetErr(label, &GeometryError{Component: "height", Value: "", Reason: "must be > 0"})
	}

	return nil
}

func validatePresetAnchor(label string, p Preset) error {
	if p.Anchor == "" || isValidAnchor(p.Anchor) {
		return nil
	}

	return presetErr(label, &ValidationError{
		Field:   "anchor",
		Value:   p.Anchor,
		Message: "valid anchors: " + strings.Join(validAnchors, ", "),
	})
}

func validatePresetMaximized(label string, p Preset) error {
	if isValidMaximized(p.Maximized) {
		return nil
	}

	return presetErr(label, &ValidationError{
		Field:   "maximized",
		Value:   p.Maximized,
		Message: "valid values: horizontal, vertical, both (or empty)",
	})
}

func presetErr(label string, err error) error {
	return &PresetError{Preset: label, Field: "", Err: err}
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

func hasRequiredPresetGeometry(pg PresetGeometry, centered bool) bool {
	if centered {
		return strings.TrimSpace(pg.Width) != "" &&
			strings.TrimSpace(pg.Height) != ""
	}

	return hasAllPresetGeometry(pg)
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
	return fmt.Sprintf("kwinl-%d-%s", os.Getpid(), generateRandomSuffix())
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
			Value:     "",
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

	return parseGeomParts(parts, false)
}

func parsePlaceGeom(s string, centered bool) (ParsedGeometry, error) {
	if !centered {
		return parseGeom(s)
	}

	parts := strings.Split(s, ",")
	if len(parts) == 2 {
		parts = []string{"", "", parts[0], parts[1]}
	}

	if len(parts) != 4 {
		message := "expected x,y,w,h (4 comma-separated values)"
		if centered {
			message = "expected w,h (2 comma-separated values) or x,y,w,h (4 comma-separated values)"
		}

		return ParsedGeometry{}, &ValidationError{
			Field:   "geom",
			Value:   s,
			Message: message,
		}
	}

	return parseGeomParts(parts, centered)
}

func parseGeomParts(parts []string, allowEmptyXY bool) (ParsedGeometry, error) {
	x, err := parseGeomValue(parts[0], "x", allowEmptyXY)
	if err != nil {
		return ParsedGeometry{}, err
	}

	y, err := parseGeomValue(parts[1], "y", allowEmptyXY)
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
		return ParsedGeometry{}, &GeometryError{Component: "width", Value: "", Reason: "must be > 0"}
	}

	if h.Value <= 0 {
		return ParsedGeometry{}, &GeometryError{Component: "height", Value: "", Reason: "must be > 0"}
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
		return ParsedGeometry{}, &GeometryError{Component: "width", Value: "", Reason: "must be > 0"}
	}

	if h.Value <= 0 {
		return ParsedGeometry{}, &GeometryError{Component: "height", Value: "", Reason: "must be > 0"}
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
		Tile:            "",
		Pinned:          cfg.Pinned,
		Minimized:       cfg.Minimized,
		KeepAbove:       cfg.KeepAbove,
		KeepBelow:       cfg.KeepBelow,
		Maximized:       "",
		FullScreen:      false,
		Geom:            cfg.Geom,
		Verbose:         verboseFlag,
		CallbackService: callbackService,
		CallbackToken:   cfg.ScriptName,
		KeepMode:        keepMode,
	}
	js := generateJS(jsCfg)

	if err := os.WriteFile(cfg.JSFile, []byte(js), 0o600); err != nil {
		return fmt.Errorf("write placement JS file %q: %w", cfg.JSFile, err)
	}

	return nil
}

type splitCommandParser struct {
	args     []string
	buf      strings.Builder
	inSingle bool
	inDouble bool
	escaped  bool
	inArg    bool
}

func (p *splitCommandParser) flush() {
	if !p.inArg && p.buf.Len() == 0 {
		return
	}

	p.args = append(p.args, p.buf.String())
	p.buf.Reset()
	p.inArg = false
}

func (p *splitCommandParser) consumeEscaped(r rune) bool {
	if !p.escaped {
		return false
	}

	p.buf.WriteRune(r)
	p.escaped = false

	return true
}

func (p *splitCommandParser) startEscape(r rune) bool {
	if r != '\\' || p.inSingle {
		return false
	}

	p.escaped = true

	return true
}

func (p *splitCommandParser) toggleQuote(r rune) bool {
	switch {
	case r == '\'' && !p.inDouble:
		p.inSingle = !p.inSingle
		p.inArg = true

		return true
	case r == '"' && !p.inSingle:
		p.inDouble = !p.inDouble
		p.inArg = true

		return true
	default:
		return false
	}
}

func isSplitCommandWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n'
}

func (p *splitCommandParser) consume(r rune) {
	if p.consumeEscaped(r) {
		return
	}

	if p.startEscape(r) {
		return
	}

	if p.toggleQuote(r) {
		return
	}

	if isSplitCommandWhitespace(r) && !p.inSingle && !p.inDouble {
		p.flush()
		return
	}

	p.inArg = true
	p.buf.WriteRune(r)
}

func splitCommand(input string) ([]string, error) {
	var parser splitCommandParser

	for _, r := range input {
		parser.consume(r)
	}

	if parser.escaped {
		return nil, errSplitCommandUnfinishedEscape
	}

	if parser.inSingle || parser.inDouble {
		return nil, errSplitCommandUnterminatedQuote
	}

	parser.flush()

	return parser.args, nil
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
	CallbackToken   string
	KeepMode        bool
}

func mustJSONString(v string) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal JSON string: %v", err))
	}

	return string(data)
}

func generateJS(cfg jsPlacementConfig) string {
	scriptNameJSON := mustJSONString(cfg.ScriptName)
	appJSON := mustJSONString(cfg.App)
	matchJSON := mustJSONString(cfg.Match)
	anchorJSON := mustJSONString(cfg.Anchor)
	monitorJSON := mustJSONString(cfg.Monitor)
	desktopJSON := mustJSONString(cfg.Desktop)
	tileJSON := mustJSONString(cfg.Tile)
	maximizedJSON := mustJSONString(cfg.Maximized)
	callbackServiceJSON := mustJSONString(cfg.CallbackService)
	callbackTokenJSON := mustJSONString(cfg.CallbackToken)

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
var CALLBACK_TOKEN = %s;
var CALLBACK_PATH = "/io/github/kwinl/Place";
var CALLBACK_IFACE = "io.github.kwinl.Place";
var KEEP_MODE = %v;

function vlog() {
  if (!VERBOSE) return;
  var msg = "[kwinl] ";
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
  return null;
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
             CALLBACK_TOKEN, true, "" + w.internalId, "" + w.caption,
             g.x + "," + g.y + "," + g.width + "," + g.height, "");
    vlog("notifySuccess: sent callback");
  } catch (e) { vlog("notifySuccess: error", e); }
}

function notifyFailure(reason) {
  if (!CALLBACK_SERVICE) return;
  try {
    callDBus(CALLBACK_SERVICE, CALLBACK_PATH, CALLBACK_IFACE, "Placed",
             CALLBACK_TOKEN, false, "", "", "", "" + reason);
    vlog("notifyFailure: sent callback reason=", reason);
  } catch (e) { vlog("notifyFailure: error", e); }
}

function abortPlacement(reason) {
  notifyFailure(reason);
  try {
    callDBus("org.kde.KWin", "/Scripting", "org.kde.kwin.Scripting", "unloadScript", SCRIPT_NAME);
  } catch (e) { vlog("abortPlacement: unload error", e); }
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
var targetDesk = PINNED ? null : findDesktop(DESKTOP);
var targetResolutionError = "";
if (MONITOR !== "" && !targetMon) {
  targetResolutionError = "invalid monitor target: " + MONITOR;
} else if (!PINNED && DESKTOP !== "" && !targetDesk) {
  targetResolutionError = "invalid desktop target: " + DESKTOP;
}

var target = null;
if (targetResolutionError === "") {
  target = resolveGeom(targetMon);
  vlog("target geometry:", target.x, target.y, target.width, target.height);
} else {
  vlog("target resolution failed:", targetResolutionError);
  abortPlacement(targetResolutionError);
}

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
  if (targetResolutionError !== "") return;
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
    if (targetDesk) {
      try { w.desktops = [targetDesk]; } catch (e) {}
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
	`, cfg.ScriptName, scriptNameJSON, appJSON, matchJSON,
		anchorJSON, monitorJSON, desktopJSON, tileJSON, cfg.Pinned,
		cfg.Minimized, cfg.KeepAbove, cfg.KeepBelow,
		maximizedJSON, cfg.FullScreen,
		cfg.Geom.X.Value, cfg.Geom.X.Percent,
		cfg.Geom.Y.Value, cfg.Geom.Y.Percent,
		cfg.Geom.W.Value, cfg.Geom.W.Percent,
		cfg.Geom.H.Value, cfg.Geom.H.Percent,
		cfg.Verbose, callbackServiceJSON, callbackTokenJSON, cfg.KeepMode)
}

func generateCaptureJS(scriptName, serviceName string, opts captureOptions) string {
	scriptNameJSON := mustJSONString(scriptName)
	serviceJSON := mustJSONString(serviceName)
	pathJSON := mustJSONString(captureObjectPath)
	ifaceJSON := mustJSONString(captureIface)
	monitorFilterJSON := mustJSONString(opts.MonitorFilter)

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

function appFromWindow(w) {
  var app = "";
  try {
    if (w.desktopFileName) app = "" + w.desktopFileName;
  } catch (e) {}
  if (!app) {
    try {
      if (w.appId) app = "" + w.appId;
    } catch (e) {}
  }
  return normalizeApp(app);
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

    var app = appFromWindow(w);
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
`, scriptName, scriptNameJSON, serviceJSON, pathJSON, ifaceJSON,
		opts.InferCommand, opts.IncludeUnknown, opts.CurrentDesktop, monitorFilterJSON)
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
		return normalizeStringScriptPath(v)
	case int32, int64, uint32, uint64, int:
		return fmt.Sprintf("%s%d", scriptObjectPrefix, v), nil
	case dbus.ObjectPath:
		return normalizeDBusScriptObjectPath(v)
	default:
		return "", &ScriptPathError{Reason: fmt.Sprintf("unexpected type %T", val)}
	}
}

func normalizeStringScriptPath(v string) (string, error) {
	if strings.HasPrefix(v, scriptObjectPrefix) {
		return v, nil
	}

	if v == "" {
		return "", &ScriptPathError{Reason: "empty string response"}
	}

	return "", &ScriptPathError{Reason: fmt.Sprintf("unexpected string %q", v)}
}

func normalizeDBusScriptObjectPath(v dbus.ObjectPath) (string, error) {
	if string(v) == "" {
		return "", &ScriptPathError{Reason: "empty object path response"}
	}

	return string(v), nil
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

	verbosef("expanded command: %v", expanded)
	cmd := exec.Command(expanded[0], expanded[1:]...) //nolint:noctx // Launcher process is managed via process-group signal forwarding, not request-scoped context cancellation.
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
		Chroot:                     "",
		Credential:                 nil,
		Ptrace:                     false,
		Setsid:                     false,
		Setpgid:                    true,
		Setctty:                    false,
		Noctty:                     false,
		Ctty:                       0,
		Foreground:                 false,
		Pgid:                       0,
		Pdeathsig:                  0,
		Cloneflags:                 0,
		Unshareflags:               0,
		UidMappings:                nil,
		GidMappings:                nil,
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                nil,
		UseCgroupFD:                false,
		CgroupFD:                   0,
		PidFD:                      nil,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start command %q: %w", expanded[0], err)
	}

	reapCommand(cmd)

	return cmd, nil
}

func reapCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}

	go func() {
		if err := cmd.Wait(); err != nil {
			verbosef("command %q exited: %v", cmd.Path, err)
		}
	}()
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

var (
	envVarBraceRE = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
	envVarPlainRE = regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)
)

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
			return newExitError(exitCodeInterrupted, errInterruptedBySIGINT)
		case syscall.SIGTERM:
			return newExitError(exitCodeTerminated, errInterruptedBySIGTERM)
		default:
			return newExitError(exitCodeTerminated, fmt.Errorf("%w %v", errInterruptedBySignal, sig))
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

func cleanupStartedCommands(cmds []*exec.Cmd) {
	if len(cmds) == 0 {
		return
	}

	verbosef("partial launch failure: terminating %d started command(s)", len(cmds))
	signalProcessGroups(cmds, syscall.SIGTERM)
}

func shouldCleanupStartedCommands(err error) bool {
	if err == nil {
		return false
	}

	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code != exitCodeInterrupted && ee.Code != exitCodeTerminated
	}

	return true
}

func waitForPlacement(timeout time.Duration, resultCh <-chan placeResult, cmds []*exec.Cmd) error {
	sigCh := make(chan os.Signal, 1)

	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-resultCh:
		verbosef("placement callback received: success=%v windowID=%s caption=%s geometry=%s",
			result.Success, result.WindowID, result.Caption, result.Geometry)

		return placementCallbackError(result)
	case <-timer.C:
		verbosef("timeout reached, placement may have failed")
		return placementTimeoutError(timeout)
	case sig := <-sigCh:
		signalProcessGroups(cmds, sig)

		switch sig {
		case syscall.SIGINT:
			return newExitError(exitCodeInterrupted, errInterruptedBySIGINT)
		case syscall.SIGTERM:
			return newExitError(exitCodeTerminated, errInterruptedBySIGTERM)
		default:
			return newExitError(exitCodeTerminated, fmt.Errorf("%w %v", errInterruptedBySignal, sig))
		}
	}
}

func waitForSignal(cmds []*exec.Cmd) error {
	sigCh := make(chan os.Signal, 1)

	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	sig := <-sigCh
	verbosef("received signal %v, cleaning up", sig)
	signalProcessGroups(cmds, sig)

	switch sig {
	case syscall.SIGINT:
		return newExitError(exitCodeInterrupted, errInterruptedBySIGINT)
	case syscall.SIGTERM:
		return newExitError(exitCodeTerminated, errInterruptedBySIGTERM)
	default:
		return newExitError(exitCodeTerminated, fmt.Errorf("%w %v", errInterruptedBySignal, sig))
	}
}
