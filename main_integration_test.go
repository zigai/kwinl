package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runJavaScript(t *testing.T, script string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "node")
	cmd.Stdin = strings.NewReader(script)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("execute JavaScript: %v\n%s", err, out)
	}

	return strings.TrimSpace(string(out))
}

func TestWindowActionMatchesUnicodeCaption(t *testing.T) {
	t.Parallel()

	if err := validateWindowRegexSelector("match", "(?=.)^.$"); err != nil {
		t.Fatalf("validate Unicode caption selector: %v", err)
	}

	script := generateWindowActionJS(jsWindowActionConfig{
		ScriptName: "script",
		Selector:   windowSelector{Match: "(?=.)^.$", States: []string{}},
		Action:     windowActionClose,
	})
	setup := `var closed=0;
var workspace={stackingOrder:[{caption:"\ud800\udc00",closeWindow:function(){closed++;}}]};
function callDBus(){};function print(){}
`

	if got := runJavaScript(t, setup+script+`console.log(closed);`); got != "1" {
		t.Fatalf("Unicode caption did not match a single character: closed %s windows", got)
	}
}

func TestCaptureFullscreenPresetsAreLaunchable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		horizontal bool
		vertical   bool
	}{
		{name: "fullscreen_only"},
		{name: "fullscreen_with_horizontal_maximization", horizontal: true},
		{name: "fullscreen_with_vertical_maximization", vertical: true},
		{name: "fullscreen_with_both_maximizations", horizontal: true, vertical: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			setup := fmt.Sprintf(`var screen={name:"DP-1",geometry:{x:0,y:0,width:1000,height:1000}};
var workspace={screens:[screen],activeScreen:screen,stackingOrder:[{
 desktopFileName:"org.example.App",frameGeometry:{x:0,y:0,width:100,height:100},
 fullScreen:true,maximizedHorizontally:%v,maximizedVertically:%v
}]};
function callDBus(service,path,iface,method,payload){if(method==="Send")console.log(payload);}
`, tt.horizontal, tt.vertical)
			payload := runJavaScript(t, setup+generateCaptureJS("script", "service", captureOptions{InferCommand: true}))

			template, err := buildTemplateFromCapturePayload(payload)
			if err != nil {
				t.Fatalf("build captured layout: %v", err)
			}

			if err := validateTemplate(template); err != nil {
				t.Fatalf("captured fullscreen layout cannot launch: %v", err)
			}

			preset := template.Presets[0]
			if !preset.FullScreen || preset.Maximized != "" {
				t.Fatalf("fullscreen state was not preserved: %+v", preset)
			}
		})
	}
}

func placementScriptForTargets(t *testing.T, monitor, desktop string) string {
	t.Helper()

	restore := savePlaceFlags()
	defer restore()

	placeAppFlag = "org.example.App"
	placeMatchFlag = ""
	placeGeomFlag = "0,0,100,100"
	placeCommandFlag = "true"
	placeAnchorFlag = "top-left"
	placeMonitorFlag = monitor
	placeDesktopFlag = desktop
	placeTimeoutFlag = "1s"
	placeCenteredFlag = false
	placePinnedFlag = false
	placeKeepAboveFlag = false
	placeKeepBelowFlag = false

	cfg, err := parseAndValidatePlace()
	if err != nil {
		t.Fatalf("parse place input: %v", err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(cfg.TempDir); err != nil {
			t.Errorf("remove placement directory: %v", err)
		}
	})

	return generateJS(jsPlacementConfig{
		ScriptName: cfg.ScriptName,
		App:        cfg.App,
		Anchor:     cfg.Anchor,
		Monitor:    cfg.Monitor,
		Desktop:    cfg.Desktop,
		Geom:       cfg.Geom,
	})
}

func TestPlacePrefersExactMonitorNameOverSuffix(t *testing.T) {
	script := placementScriptForTargets(t, "dp-1", "")
	setup := `var added;
var internal={name:"eDP-1",geometry:{x:0,y:0,width:1000,height:1000}};
var external={name:"DP-1",geometry:{x:1000,y:0,width:1000,height:1000}};
var workspace={screens:[internal,external],activeScreen:internal,desktops:[],stackingOrder:[],
 windowAdded:{connect:function(fn){added=fn;}},sendWindowToOutput:function(w,screen){w.output=screen.name;}};
function callDBus(){};function print(){}
var window={internalId:"1",desktopFileName:"org.example.App",frameGeometry:{x:0,y:0,width:100,height:100}};
`

	got := runJavaScript(t, setup+script+`added(window);console.log(JSON.stringify({monitor:window.output,x:window.frameGeometry.x}));`)
	if got != `{"monitor":"DP-1","x":1000}` {
		t.Fatalf("placed on wrong monitor: %s", got)
	}
}

func TestPlaceResolvesDigitPrefixedDesktopName(t *testing.T) {
	script := placementScriptForTargets(t, "", "1-work")
	setup := `var added;
var screen={name:"DP-1",geometry:{x:0,y:0,width:1000,height:1000}};
var workspace={screens:[screen],activeScreen:screen,desktops:[{name:"main"},{name:"1-work"}],stackingOrder:[],
 windowAdded:{connect:function(fn){added=fn;}}};
function callDBus(){};function print(){}
var window={internalId:"1",desktopFileName:"org.example.App",frameGeometry:{x:0,y:0,width:100,height:100}};
`

	got := runJavaScript(t, setup+script+`added(window);console.log(window.desktops[0].name);`)
	if got != "1-work" {
		t.Fatalf("placed on wrong desktop: %q", got)
	}
}

func TestWindowActionPrefersExactMonitorNameOverSuffix(t *testing.T) {
	restore := saveWindowFlags()
	defer restore()

	windowIDFlag = ""
	windowAppFlag = ""
	windowMatchFlag = ""
	windowClassFlag = ""
	windowDesktopFlag = ""
	windowMonitorFlag = "dp-1"
	windowStateFlags = nil
	windowLimitFlag = 0
	windowAllFlag = false
	windowTimeoutFlag = "1s"

	cfg, err := parseWindowActionConfig(windowActionClose)
	if err != nil {
		t.Fatalf("parse window action: %v", err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(cfg.TempDir); err != nil {
			t.Errorf("remove window action directory: %v", err)
		}
	})

	script := generateWindowActionJS(jsWindowActionConfig{ScriptName: cfg.ScriptName, Selector: cfg.Selector, Action: windowActionClose})

	setup := `var closed=[];
var internal={name:"eDP-1",geometry:{x:0,y:0,width:1000,height:1000}};
var external={name:"DP-1",geometry:{x:1000,y:0,width:1000,height:1000}};
var workspace={screens:[internal,external],stackingOrder:[
 {frameGeometry:{x:1000,y:0,width:100,height:100},closeWindow:function(){closed.push("DP-1");}},
 {frameGeometry:{x:0,y:0,width:100,height:100},closeWindow:function(){closed.push("eDP-1");}}
]};
function callDBus(){};function print(){}
`
	if got := runJavaScript(t, setup+script+`console.log(JSON.stringify(closed));`); got != `["DP-1"]` {
		t.Fatalf("closed windows on wrong monitor: %s", got)
	}
}

func TestWindowActionPreservesUnrelatedAppNamespaces(t *testing.T) {
	restore := saveWindowFlags()
	defer restore()

	windowIDFlag = ""
	windowAppFlag = "org.alpha.Editor"
	windowMatchFlag = ""
	windowClassFlag = ""
	windowDesktopFlag = ""
	windowMonitorFlag = ""
	windowStateFlags = nil
	windowLimitFlag = 0
	windowAllFlag = false
	windowTimeoutFlag = "1s"

	cfg, err := parseWindowActionConfig(windowActionClose)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(cfg.TempDir); err != nil {
			t.Error(err)
		}
	})

	script := generateWindowActionJS(jsWindowActionConfig{ScriptName: cfg.ScriptName, Selector: cfg.Selector, Action: windowActionClose})

	setup := `var closed=[];
var workspace={stackingOrder:[
 {desktopFileName:"org.alpha.Editor",resourceClass:"Editor",closeWindow:function(){closed.push("org.alpha.Editor");}},
 {desktopFileName:"org.beta.Editor",resourceClass:"Editor",closeWindow:function(){closed.push("org.beta.Editor");}}
]};
function callDBus(){};function print(){}
`
	if got := runJavaScript(t, setup+script+`console.log(JSON.stringify(closed));`); got != `["org.alpha.Editor"]` {
		t.Fatalf("closed unrelated application: %s", got)
	}
}

func TestPlacePreservesUnrelatedAppNamespaces(t *testing.T) {
	script := placementScriptForTargets(t, "", "")

	setup := `var added;
var screen={name:"DP-1",geometry:{x:0,y:0,width:1000,height:1000}};
var workspace={screens:[screen],activeScreen:screen,desktops:[],stackingOrder:[],windowAdded:{connect:function(fn){added=fn;}}};
function callDBus(){};function print(){}
var unrelated={internalId:"1",desktopFileName:"org.other.App",resourceClass:"App",frameGeometry:{x:300,y:300,width:50,height:50}};
var intended={internalId:"2",desktopFileName:"org.example.App",resourceClass:"App",frameGeometry:{x:300,y:300,width:50,height:50}};
`

	got := runJavaScript(t, setup+script+`added(unrelated);added(intended);console.log(JSON.stringify([unrelated.frameGeometry,intended.frameGeometry]));`)
	if got != `[{"x":300,"y":300,"width":50,"height":50},{"x":0,"y":0,"width":100,"height":100}]` {
		t.Fatalf("placed unrelated application: %s", got)
	}
}
