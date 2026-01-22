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
	Name       string         `json:"name" yaml:"name"`
	App        string         `json:"app,omitempty" yaml:"app,omitempty"`
	Match      string         `json:"match,omitempty" yaml:"match,omitempty"`
	Command    CommandSpec    `json:"command" yaml:"command"`
	Geometry   PresetGeometry `json:"geometry" yaml:"geometry"`
	Anchor     string         `json:"anchor,omitempty" yaml:"anchor,omitempty"`
	Monitor    string         `json:"monitor,omitempty" yaml:"monitor,omitempty"`
	Desktop    string         `json:"desktop,omitempty" yaml:"desktop,omitempty"`
	Maximized  string         `json:"maximized,omitempty" yaml:"maximized,omitempty"`
	FullScreen bool           `json:"fullscreen,omitempty" yaml:"fullscreen,omitempty"`
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
	version = "1.0.0"

	placeAppFlag      string
	placeGeomFlag     string
	placeAnchorFlag   string
	placeMonitorFlag  string
	placeDesktopFlag  string
	placeTimeoutFlag  string
	placeCommandFlag  string
	launchTimeoutFlag string

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
	Use:   "place --app <app-id> --geom <x>,<y>,<w>,<h> --cmd \"<command>\" [--anchor <anchor>] [--monitor <id>] [--desktop <id>] [--timeout <duration>]",
	Short: "Launch a command and place its window at a specific geometry",
	Long: `Loads a temporary KWin script via D-Bus that intercepts newly created
windows matching the specified application ID and moves/resizes them to the
requested geometry. Only windows created after the script loads are affected.

Geometry values can be absolute pixels (e.g., 100) or percentages (e.g., 50%).
Percentages are relative to the target monitor's dimensions.`,
	Example: `  kwin-layout place --app org.kde.konsole --geom 50,50,900,700 --timeout 8s --cmd "konsole --separate"
  kwin-layout place --app org.kde.konsole --geom 0,0,50%,100% --anchor top-left --cmd "konsole"
  kwin-layout place --app org.kde.konsole --geom 0,0,50%,100% --monitor 1 --desktop 2 --cmd "konsole"`,
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

func init() {
	rootCmd.SetVersionTemplate("{{.Version}}\n")

	placeCmd.Flags().StringVar(&placeAppFlag, "app", "", "application ID to match (required)")
	placeCmd.Flags().StringVar(&placeGeomFlag, "geom", "", "geometry as x,y,w,h (values can be pixels or percentages like 50%)")
	placeCmd.Flags().StringVar(&placeAnchorFlag, "anchor", "top-left", "anchor point for positioning")
	placeCmd.Flags().StringVar(&placeMonitorFlag, "monitor", "", "target monitor (index like 0, 1 or name like DP-1)")
	placeCmd.Flags().StringVar(&placeDesktopFlag, "desktop", "", "target virtual desktop (1-based index or name)")
	placeCmd.Flags().StringVar(&placeTimeoutFlag, "timeout", "8s", "timeout duration (e.g., 8s, 500ms)")
	placeCmd.Flags().StringVar(&placeCommandFlag, "cmd", "", "command to run (quoted string)")
	must(placeCmd.MarkFlagRequired("app"))
	must(placeCmd.MarkFlagRequired("geom"))
	must(placeCmd.MarkFlagRequired("cmd"))

	launchCmd.Flags().StringVar(&launchTimeoutFlag, "timeout", "", "timeout override (e.g., 10s)")

	captureCmd.Flags().StringVar(&captureTimeoutFlag, "timeout", "2s", "capture timeout (e.g., 2s, 500ms)")
	captureCmd.Flags().BoolVar(&captureInferCommandFlag, "infer-command", true, "infer a best-effort launcher command using gtk-launch")
	captureCmd.Flags().BoolVar(&captureIncludeUnknown, "include-unknown", false, "include windows without desktopFileName (matched by title)")
	captureCmd.Flags().BoolVar(&captureCurrentDesktop, "current-desktop", false, "only capture windows on current desktop")
	captureCmd.Flags().StringVar(&captureMonitorFilter, "monitor", "", "only capture windows on specified monitor")

	rootCmd.AddCommand(placeCmd)
	rootCmd.AddCommand(launchCmd)
	rootCmd.AddCommand(captureCmd)
}

func must(err error) {
	if err != nil {
		panic(err)
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

	tempDir, err := os.MkdirTemp("", "kwin-layout-*")
	if err != nil {
		return Config{}, fmt.Errorf("failed to create temp dir: %w", err)
	}

	jsFile := filepath.Join(tempDir, scriptName+".js")

	return Config{
		App:        placeAppFlag,
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

	tempDir, err := os.MkdirTemp("", "kwin-layout-launch-*")
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

		scriptName := fmt.Sprintf("kwin-layout-%d-%d-%s", os.Getpid(), i, generateRandomSuffix())
		jsFile := filepath.Join(tempDir, scriptName+".js")

		jsCfg := jsPlacementConfig{
			ScriptName: scriptName,
			App:        preset.App,
			Match:      preset.Match,
			Anchor:     anchor,
			Monitor:    preset.Monitor,
			Desktop:    preset.Desktop,
			Maximized:  preset.Maximized,
			FullScreen: preset.FullScreen,
			Geom:       geom,
		}
		js := generateJS(jsCfg)
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

type captureReceiver struct {
	ch chan string
}

func (r *captureReceiver) Send(payload string) (bool, *dbus.Error) {
	select {
	case r.ch <- payload:
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
		case ".json":
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
	defer os.RemoveAll(tempDir)

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return &ExitError{
			Code: exitCodeDBusFailure,
			Err:  fmt.Errorf("cannot connect to session D-Bus: %w", err),
		}
	}
	defer conn.Close()

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
	for _, valid := range validMaximizedValues {
		if v == valid {
			return true
		}
	}
	return false
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
	jsCfg := jsPlacementConfig{
		ScriptName: cfg.ScriptName,
		App:        cfg.App,
		Anchor:     cfg.Anchor,
		Monitor:    cfg.Monitor,
		Desktop:    cfg.Desktop,
		Geom:       cfg.Geom,
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
	ScriptName string
	App        string
	Match      string
	Anchor     string
	Monitor    string
	Desktop    string
	Maximized  string
	FullScreen bool
	Geom       ParsedGeometry
}

func generateJS(cfg jsPlacementConfig) string {
	scriptNameJSON, _ := json.Marshal(cfg.ScriptName)
	appJSON, _ := json.Marshal(cfg.App)
	matchJSON, _ := json.Marshal(cfg.Match)
	anchorJSON, _ := json.Marshal(cfg.Anchor)
	monitorJSON, _ := json.Marshal(cfg.Monitor)
	desktopJSON, _ := json.Marshal(cfg.Desktop)
	maximizedJSON, _ := json.Marshal(cfg.Maximized)

	return fmt.Sprintf(`// Auto-generated: %s
var SCRIPT_NAME = %s;
var TARGET_APP = %s;
var TARGET_MATCH = %s;
var ANCHOR = %s;
var MONITOR = %s;
var DESKTOP = %s;
var MAXIMIZED = %s;
var FULLSCREEN = %v;
var GEOM_X = {value: %d, percent: %v};
var GEOM_Y = {value: %d, percent: %v};
var GEOM_W = {value: %d, percent: %v};
var GEOM_H = {value: %d, percent: %v};

function idOf(w) { return "" + w.internalId; }

function appMatches(w) {
  if (TARGET_APP === "" && TARGET_MATCH === "") return false;
  if (TARGET_APP !== "") {
    var app = w.desktopFileName ? ("" + w.desktopFileName) : "";
    if (app === TARGET_APP) return true;
    if (app === (TARGET_APP + ".desktop")) return true;
    if (app.endsWith("/" + TARGET_APP + ".desktop")) return true;
    if (app.endsWith("/" + TARGET_APP)) return true;
  }
  if (TARGET_MATCH !== "") {
    try {
      var re = new RegExp(TARGET_MATCH);
      if (re.test(w.caption || "")) return true;
    } catch (e) {}
  }
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
  if (isManageable(w0) && appMatches(w0)) baseline[idOf(w0)] = true;
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

  print("[kwin-layout] candidate:",
        "caption=", ("" + w.caption),
        "app=", ("" + w.desktopFileName),
        "id=", idOf(w));

  if (FULLSCREEN) {
    w.fullScreen = true;
  } else {
    w.frameGeometry = target;
    if (MAXIMIZED === "both") {
      try { w.setMaximize(true, true); } catch (e) {}
    } else if (MAXIMIZED === "horizontal") {
      try { w.setMaximize(false, true); } catch (e) {}
    } else if (MAXIMIZED === "vertical") {
      try { w.setMaximize(true, false); } catch (e) {}
    }
  }

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

    if (FULLSCREEN) return;

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
  if (!appMatches(w)) return false;

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
`, cfg.ScriptName, string(scriptNameJSON), string(appJSON), string(matchJSON),
		string(anchorJSON), string(monitorJSON), string(desktopJSON),
		string(maximizedJSON), cfg.FullScreen,
		cfg.Geom.X.Value, cfg.Geom.X.Percent,
		cfg.Geom.Y.Value, cfg.Geom.Y.Percent,
		cfg.Geom.W.Value, cfg.Geom.W.Percent,
		cfg.Geom.H.Value, cfg.Geom.H.Percent)
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
