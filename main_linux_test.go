package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestLaunchTimeoutUnloadsStartedScript(t *testing.T) {
	server := newLaunchBus(t)

	layout := filepath.Join(t.TempDir(), "layout.json")

	data := `{"timeout":"10ms","presets":[{"app":"org.example.App","command":["true"],"geometry":{"x":0,"y":0,"width":100,"height":100}}]}`
	if err := os.WriteFile(layout, []byte(data), 0o600); err != nil {
		t.Fatalf("write layout: %v", err)
	}

	oldTimeout := launchTimeoutFlag
	launchTimeoutFlag = ""

	t.Cleanup(func() { launchTimeoutFlag = oldTimeout })

	err := runLaunch(launchCmd, []string{layout})
	if !errors.Is(err, errNoPlacementCallbackReceived) {
		t.Fatalf("expected placement timeout, got %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	if server.loads != 1 || len(server.loaded) != 0 {
		t.Fatalf("loaded %d scripts; %d remain after failed launch", server.loads, len(server.loaded))
	}

	scripts, err := readScriptMarkers()
	if err != nil {
		t.Fatalf("read tracking markers: %v", err)
	}

	if len(scripts) != 0 {
		t.Fatalf("tracking markers remain after failed launch: %+v", scripts)
	}
}

type launchBus struct {
	mu     sync.Mutex
	loaded map[string]bool
	loads  int
}

func startPrivateBus(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	socketName := filepath.Base(filepath.Dir(dir))
	cmd := exec.CommandContext(t.Context(), "dbus-daemon", "--session", "--nofork", "--print-address=1", "--address=unix:abstract=kwinl-test-"+socketName)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("open bus address pipe: %v", err)
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start private bus: %v", err)
	}

	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("stop private bus: %v", err)
		}

		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != -1 {
				t.Errorf("wait for private bus: %v", err)
			}
		}
	})

	address, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read bus address: %v", err)
	}

	address = strings.TrimSpace(address)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", address)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("TMPDIR", dir)

	return address
}

func newLaunchBus(t *testing.T) *launchBus {
	t.Helper()

	address := startPrivateBus(t)

	conn, err := dbus.Connect(address)
	if err != nil {
		t.Fatalf("connect private bus: %v", err)
	}

	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close private bus connection: %v", err)
		}
	})

	reply, err := conn.RequestName(dbusDestination, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("own fake KWin service: %v, reply %d", err, reply)
	}

	server := &launchBus{loaded: make(map[string]bool)}
	if err := conn.ExportMethodTable(map[string]any{
		"loadScript":   server.loadScript,
		"unloadScript": server.unloadScript,
	}, dbus.ObjectPath(dbusScriptingPath), dbusScriptingIface); err != nil {
		t.Fatalf("export fake KWin scripting service: %v", err)
	}

	if err := conn.ExportMethodTable(map[string]any{
		"run": func() *dbus.Error { return nil },
	}, dbus.ObjectPath(scriptObjectPrefix+"1"), dbusScriptIface); err != nil {
		t.Fatalf("export fake KWin script: %v", err)
	}

	return server
}

func (s *launchBus) loadScript(path, name string) (int32, *dbus.Error) {
	if _, err := os.ReadFile(path); err != nil {
		return 0, dbus.MakeFailedError(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.loaded[name] = true
	s.loads++

	return 1, nil
}

func (s *launchBus) unloadScript(name string) *dbus.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.loaded, name)

	return nil
}
