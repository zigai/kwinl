package main

import (
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
