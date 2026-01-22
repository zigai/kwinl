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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	dbusDestination      = "org.kde.KWin"
	dbusScriptingPath    = "/Scripting"
	dbusScriptingIface   = "org.kde.kwin.Scripting"
	dbusScriptIface      = "org.kde.kwin.Script"
	scriptObjectPrefix   = "/Scripting/Script"
	defaultTimeoutSec    = 8
	exitCodeInternal     = 1
	exitCodeUsage        = 2
	exitCodeDBusFailure  = 10
	exitCodeLoadFailed   = 11
	exitCodeLaunchFailed = 20
	exitCodeInterrupted  = 130
	exitCodeTerminated   = 143
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
	DF         string
	Geom       ParsedGeometry
	Anchor     string
	Monitor    string
	Desktop    string
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
	Name     string         `json:"name" yaml:"name"`
	DF       string         `json:"df" yaml:"df"`
	Command  CommandSpec    `json:"command" yaml:"command"`
	Geometry PresetGeometry `json:"geometry" yaml:"geometry"`
	Anchor   string         `json:"anchor,omitempty" yaml:"anchor,omitempty"`
	Monitor  string         `json:"monitor,omitempty" yaml:"monitor,omitempty"`
	Desktop  string         `json:"desktop,omitempty" yaml:"desktop,omitempty"`
}

type Template struct {
	Presets []Preset `json:"presets" yaml:"presets"`
	Timeout string   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
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

type ScriptPathWarning struct {
	Reason string
}

func (e *ScriptPathWarning) Error() string {
	return fmt.Sprintf("unexpected loadScript return: %s", e.Reason)
}

var (
	placeDFFlag       string
	placeGeomFlag     string
	placeAnchorFlag   string
	placeMonitorFlag  string
	placeDesktopFlag  string
	placeTimeoutFlag  string
	placeCommandFlag  string
	launchTimeoutFlag string
)

var rootCmd = &cobra.Command{
	Use:   "kwin-place",
	Short: "KWin window placement tool",
	Long: `kwin-place loads temporary KWin scripts via D-Bus that intercept newly created
windows and move/resize them to the requested geometry.`,
}

var placeCmd = &cobra.Command{
	Use:   "place --df <desktopFileName> --geom <x>,<y>,<w>,<h> --cmd \"<command>\" [--anchor <anchor>] [--monitor <id>] [--desktop <id>] [--timeout <duration>]",
	Short: "Launch a command and place its window at a specific geometry",
	Long: `Loads a temporary KWin script via D-Bus that intercepts newly created
windows matching the specified desktopFileName and moves/resizes them to the
requested geometry. Only windows created after the script loads are affected.

Geometry values can be absolute pixels (e.g., 100) or percentages (e.g., 50%).
Percentages are relative to the target monitor's dimensions.`,
	Example: `  kwin-place place --df org.kde.konsole --geom 50,50,900,700 --timeout 8s --cmd "konsole --separate"
  kwin-place place --df org.kde.konsole --geom 0,0,50%,100% --anchor top-left --cmd "konsole"
  kwin-place place --df org.kde.konsole --geom 0,0,50%,100% --monitor 1 --desktop 2 --cmd "konsole"`,
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
	Example: `  kwin-place launch layout.yaml
  kwin-place launch workspace.json --timeout 15s`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runLaunch,
}

func init() {
	placeCmd.Flags().StringVar(&placeDFFlag, "df", "", "desktopFileName to match (required)")
	placeCmd.Flags().StringVar(&placeGeomFlag, "geom", "", "geometry as x,y,w,h (values can be pixels or percentages like 50%)")
	placeCmd.Flags().StringVar(&placeAnchorFlag, "anchor", "top-left", "anchor point for positioning")
	placeCmd.Flags().StringVar(&placeMonitorFlag, "monitor", "", "target monitor (index like 0, 1 or name like DP-1)")
	placeCmd.Flags().StringVar(&placeDesktopFlag, "desktop", "", "target virtual desktop (1-based index or name)")
	placeCmd.Flags().StringVar(&placeTimeoutFlag, "timeout", "8s", "timeout duration (e.g., 8s, 500ms)")
	placeCmd.Flags().StringVar(&placeCommandFlag, "cmd", "", "command to run (quoted string)")
	must(placeCmd.MarkFlagRequired("df"))
	must(placeCmd.MarkFlagRequired("geom"))
	must(placeCmd.MarkFlagRequired("cmd"))

	launchCmd.Flags().StringVar(&launchTimeoutFlag, "timeout", "", "timeout override (e.g., 10s)")

	rootCmd.AddCommand(placeCmd)
	rootCmd.AddCommand(launchCmd)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("kwin-place: ")
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
	if errors.As(err, &ce) {
		return true
	}
	return false
}

func isValidAnchor(anchor string) bool {
	for _, a := range validAnchors {
		if a == anchor {
			return true
		}
	}
	return false
}

func runPlace(cmd *cobra.Command, args []string) error {
	cfg, err := parseAndValidatePlace(cmd, args)
	if err != nil {
		return err
	}

	defer os.RemoveAll(cfg.TempDir)

	if err := writeJSFile(cfg); err != nil {
		return fmt.Errorf("failed to write JS file: %w", err)
	}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot connect to session D-Bus: %w", err),
		}
	}
	defer conn.Close()

	scriptPath, err := loadScript(conn, cfg.JSFile, cfg.ScriptName)
	if err != nil {
		var warning *ScriptPathWarning
		if errors.As(err, &warning) {
			log.Printf("warning: %v", warning)
		} else {
			return &ExitError{
				Code: exitCodeLoadFailed,
				Err:  fmt.Errorf("loadScript failed: %w", err),
			}
		}
	}
	defer unloadScript(conn, cfg.ScriptName)

	if err := runScript(conn, cfg.ScriptName, scriptPath); err != nil {
		return &ExitError{
			Code: exitCodeLoadFailed,
			Err:  err,
		}
	}

	cmdProc, err := launchCommand(cfg.Cmd)
	if err != nil {
		return &ExitError{
			Code: exitCodeLaunchFailed,
			Err:  fmt.Errorf("failed to launch command: %w", err),
		}
	}

	return waitAndCleanup(cfg.Timeout, []*exec.Cmd{cmdProc})
}

func parseAndValidatePlace(cmd *cobra.Command, args []string) (Config, error) {
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

	timeout, err := parseTimeout(placeTimeoutFlag)
	if err != nil {
		return Config{}, err
	}

	scriptName := generateScriptName()

	tempDir, err := os.MkdirTemp("", "kwin-place-*")
	if err != nil {
		return Config{}, fmt.Errorf("failed to create temp dir: %w", err)
	}

	jsFile := filepath.Join(tempDir, scriptName+".js")

	return Config{
		DF:         placeDFFlag,
		Geom:       geom,
		Anchor:     placeAnchorFlag,
		Monitor:    placeMonitorFlag,
		Desktop:    placeDesktopFlag,
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

	tempDir, err := os.MkdirTemp("", "kwin-place-launch-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot connect to session D-Bus: %w", err),
		}
	}
	defer conn.Close()

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

		anchor := preset.Anchor
		if anchor == "" {
			anchor = "top-left"
		}

		geom, err := parsePresetGeometry(preset.Geometry)
		if err != nil {
			return &PresetError{Preset: label, Field: "geometry", Err: err}
		}

		scriptName := fmt.Sprintf("kwin-place-%d-%d-%s", os.Getpid(), i, generateRandomSuffix())
		jsFile := filepath.Join(tempDir, scriptName+".js")

		js := generateJS(scriptName, preset.DF, geom, anchor, preset.Monitor, preset.Desktop)
		if err := os.WriteFile(jsFile, []byte(js), 0600); err != nil {
			return &PresetError{Preset: label, Err: fmt.Errorf("failed to write script: %w", err)}
		}

		scriptPath, err := loadScript(conn, jsFile, scriptName)
		if err != nil {
			var warning *ScriptPathWarning
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
	}

	return waitAndCleanup(timeout, cmdProcs)
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

func validateTemplate(t Template) error {
	if len(t.Presets) == 0 {
		return &ValidationError{Field: "presets", Message: "at least one preset is required"}
	}

	for i, p := range t.Presets {
		label := p.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}

		if p.DF == "" {
			return &PresetError{
				Preset: label,
				Err:    &ValidationError{Field: "df", Message: "required field is missing"},
			}
		}

		if len(p.Command) == 0 {
			return &PresetError{
				Preset: label,
				Err:    &ValidationError{Field: "command", Message: "required field is missing"},
			}
		}

		geom, err := parsePresetGeometry(p.Geometry)
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
	}

	return nil
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
	return fmt.Sprintf("kwin-place-%d-%s", os.Getpid(), generateRandomSuffix())
}

func generateRandomSuffix() string {
	randomBytes := make([]byte, 6)
	if _, err := rand.Read(randomBytes); err == nil {
		return hex.EncodeToString(randomBytes)
	}
	log.Printf("warning: crypto/rand.Read failed; falling back to time-based suffix")
	now := time.Now().UnixNano()
	pid := os.Getpid()
	return fmt.Sprintf("%x%x", uint64(now), uint32(pid))
}

func parseGeomValue(s, component string) (GeomValue, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "%") {
		pct, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
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

	x, err := parseGeomValue(parts[0], "x")
	if err != nil {
		return ParsedGeometry{}, err
	}

	y, err := parseGeomValue(parts[1], "y")
	if err != nil {
		return ParsedGeometry{}, err
	}

	w, err := parseGeomValue(parts[2], "width")
	if err != nil {
		return ParsedGeometry{}, err
	}

	h, err := parseGeomValue(parts[3], "height")
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

func parsePresetGeometry(pg PresetGeometry) (ParsedGeometry, error) {
	x, err := parseGeomValue(pg.X, "x")
	if err != nil {
		return ParsedGeometry{}, err
	}

	y, err := parseGeomValue(pg.Y, "y")
	if err != nil {
		return ParsedGeometry{}, err
	}

	w, err := parseGeomValue(pg.Width, "width")
	if err != nil {
		return ParsedGeometry{}, err
	}

	h, err := parseGeomValue(pg.Height, "height")
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

func writeJSFile(cfg Config) error {
	js := generateJS(cfg.ScriptName, cfg.DF, cfg.Geom, cfg.Anchor, cfg.Monitor, cfg.Desktop)
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

func generateJS(scriptName, df string, g ParsedGeometry, anchor, monitor, desktop string) string {
	scriptNameJSON, _ := json.Marshal(scriptName)
	dfJSON, _ := json.Marshal(df)
	anchorJSON, _ := json.Marshal(anchor)
	monitorJSON, _ := json.Marshal(monitor)
	desktopJSON, _ := json.Marshal(desktop)

	return fmt.Sprintf(`// Auto-generated: %s
var SCRIPT_NAME = %s;
var TARGET_DF = %s;
var ANCHOR = %s;
var MONITOR = %s;
var DESKTOP = %s;
var GEOM_X = {value: %d, percent: %v};
var GEOM_Y = {value: %d, percent: %v};
var GEOM_W = {value: %d, percent: %v};
var GEOM_H = {value: %d, percent: %v};

function idOf(w) { return "" + w.internalId; }

function dfMatches(w) {
  var df = w.desktopFileName ? ("" + w.desktopFileName) : "";
  if (df === TARGET_DF) return true;
  if (df === (TARGET_DF + ".desktop")) return true;
  if (df.endsWith("/" + TARGET_DF + ".desktop")) return true;
  if (df.endsWith("/" + TARGET_DF)) return true;
  return false;
}

function isManageable(w) {
  if (!w) return false;
  if (w.deleted) return false;
  if (w.specialWindow) return false;
  if (w.popupWindow) return false;
  if (w.dock) return false;
  if (w.desktopWindow) return false;
  return true;
}

function findMonitor(id) {
  if (!id || id === "") return workspace.activeScreen;
  var screens = workspace.screens;
  var idx = parseInt(id);
  if (!isNaN(idx) && idx >= 0 && idx < screens.length) {
    return screens[idx];
  }
  for (var i = 0; i < screens.length; i++) {
    if (screens[i].name === id) return screens[i];
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

var baseline = {};
for (var i = 0; i < workspace.stackingOrder.length; i++) {
  var w0 = workspace.stackingOrder[i];
  if (isManageable(w0) && dfMatches(w0)) baseline[idOf(w0)] = true;
}

var handled = false;
var targetMon = findMonitor(MONITOR);
var target = resolveGeom(targetMon);

function finish() {
  try { workspace.windowAdded.disconnect(onAdded); } catch (e) {}
  try {
    if (typeof callDBus === "function") {
      callDBus("org.kde.KWin", "/Scripting", "org.kde.kwin.Scripting", "unloadScript", SCRIPT_NAME);
    }
  } catch (e) {}
}

function applyAndStick(w) {
  if (handled) return;
  handled = true;

  print("[kwin-place] candidate:",
        "caption=", ("" + w.caption),
        "df=", ("" + w.desktopFileName),
        "id=", idOf(w));

  w.frameGeometry = target;

  if (MONITOR !== "") {
    try { workspace.sendWindowToOutput(w, targetMon); } catch (e) {}
  }

  var desk = findDesktop(DESKTOP);
  if (desk) {
    try { w.desktops = [desk]; } catch (e) {}
  }

  var triesLeft = 6;

  function ensure() {
    if (triesLeft <= 0) {
      finish();
      return;
    }
    triesLeft--;

    var g = w.frameGeometry;
    var ok =
      (Math.round(g.x) === target.x) &&
      (Math.round(g.y) === target.y) &&
      (Math.round(g.width) === target.width) &&
      (Math.round(g.height) === target.height);

    if (!ok) w.frameGeometry = target;
  }

  try { w.frameGeometryChanged.connect(ensure); } catch (e) {}
  try { w.windowShown.connect(ensure); } catch (e) {}
  try { w.activeChanged.connect(ensure); } catch (e) {}

  ensure();
}

function isNewMatch(w) {
  if (!isManageable(w)) return false;
  if (!dfMatches(w)) return false;

  var id = idOf(w);
  if (baseline[id]) return false;
  return true;
}

function onAdded(w) {
  if (!handled && isNewMatch(w)) applyAndStick(w);
}

workspace.windowAdded.connect(onAdded);

for (var i = 0; i < workspace.stackingOrder.length; i++) {
  var w1 = workspace.stackingOrder[i];
  if (!handled && isNewMatch(w1)) {
    applyAndStick(w1);
    break;
  }
}
`, scriptName, string(scriptNameJSON), string(dfJSON), string(anchorJSON),
		string(monitorJSON), string(desktopJSON),
		g.X.Value, g.X.Percent,
		g.Y.Value, g.Y.Percent,
		g.W.Value, g.W.Percent,
		g.H.Value, g.H.Percent)
}

func loadScript(conn *dbus.Conn, jsPath, scriptName string) (string, error) {
	obj := conn.Object(dbusDestination, dbus.ObjectPath(dbusScriptingPath))
	call := obj.Call(dbusScriptingIface+".loadScript", 0, jsPath, scriptName)
	if call.Err != nil {
		return "", call.Err
	}

	return normalizeScriptPath(call.Body)
}

func normalizeScriptPath(body []interface{}) (string, error) {
	if len(body) == 0 {
		return "", &ScriptPathWarning{Reason: "empty response"}
	}

	val := body[0]

	switch v := val.(type) {
	case string:
		if strings.HasPrefix(v, scriptObjectPrefix) {
			return v, nil
		}
		if v == "" {
			return "", &ScriptPathWarning{Reason: "empty string response"}
		}
		return "", &ScriptPathWarning{Reason: fmt.Sprintf("unexpected string %q", v)}
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
			return "", &ScriptPathWarning{Reason: "empty object path response"}
		}
		return string(v), nil
	default:
		return "", &ScriptPathWarning{Reason: fmt.Sprintf("unexpected type %T", val)}
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
	cmd := exec.Command(cmdSlice[0], cmdSlice[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
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
