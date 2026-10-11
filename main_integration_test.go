package main

import (
	"fmt"
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
