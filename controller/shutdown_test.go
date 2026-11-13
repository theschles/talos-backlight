package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestNormalBrightness(t *testing.T) {
	t.Run("new enrollment retains captured ratio", func(t *testing.T) {
		p := &fixture{value: 6150, max: 7500}
		c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
		normal, err := c.normalBrightness()
		if err != nil || normal != (normalLevel{6150, 7500}) {
			t.Fatalf("normal=%+v error=%v", normal, err)
		}
		p.value = 0
		again, err := c.normalBrightness()
		if err != nil || again != normal || again.atMaximum(100) != 82 {
			t.Fatalf("reloaded=%+v error=%v", again, err)
		}
	})

	t.Run("existing node uses 75 percent", func(t *testing.T) {
		dir := t.TempDir()
		if err := saveState(filepath.Join(dir, "startup-boot.json"), "old-boot"); err != nil {
			t.Fatal(err)
		}
		c := &controller{panel: &fixture{value: 67828, max: 82311}, journal: filepath.Join(dir, "restore.json")}
		normal, err := c.normalBrightness()
		if err != nil || normal.atMaximum(82311) != 61733 {
			t.Fatalf("normal=%+v error=%v", normal, err)
		}
	})
}

func TestRestoreForShutdown(t *testing.T) {
	p := &fixture{value: 3750, max: 7500}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	if err := c.off(0); err != nil {
		t.Fatal(err)
	}
	restarted := &controller{panel: p, journal: c.journal}
	if err := restarted.recover(); err != nil || p.value != 0 {
		t.Fatalf("ordinary restart lost indefinite off: brightness=%d error=%v", p.value, err)
	}
	normal := normalLevel{75, 100}
	if err := restarted.restoreForShutdown(normal); err != nil || p.value != 5625 || restarted.pending != nil {
		t.Fatalf("shutdown restore: brightness=%d error=%v", p.value, err)
	}
	if _, err := os.Stat(c.journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restoration journal was not cleared")
	}
	p.value = 7000
	if err := restarted.restoreForShutdown(normal); err != nil || p.value != 7000 {
		t.Fatalf("shutdown dimmed an already bright panel: brightness=%d error=%v", p.value, err)
	}
}

func TestShutdownSequence(t *testing.T) {
	for _, tc := range []struct {
		sequence string
		action   machine.SequenceEvent_Action
		want     string
	}{
		{"shutdown", machine.SequenceEvent_START, "shutdown"},
		{"reboot", machine.SequenceEvent_START, "reboot"},
		{"reboot", machine.SequenceEvent_STOP, "reboot"},
		{"boot", machine.SequenceEvent_START, ""},
	} {
		data, err := anypb.New(&machine.SequenceEvent{Sequence: tc.sequence, Action: tc.action})
		if err != nil {
			t.Fatal(err)
		}
		data.TypeUrl = "talos/runtime/machine.SequenceEvent"
		if got := shutdownSequence(&machine.Event{Data: data}); got.GetSequence() != tc.want {
			t.Errorf("event %s %s got %q want %q", tc.sequence, tc.action, got.GetSequence(), tc.want)
		}
	}
	if got := shutdownSequence(&machine.Event{}); got != nil {
		t.Fatalf("greeting incorrectly handled as %q", got)
	}
}

func TestShutdownPolicy(t *testing.T) {
	p := shutdownPolicy{Classes: map[string]bool{"apple-gmux": false}, Nodes: map[string]bool{"laptop-a": true}}
	if p.enabled("apple-gmux", "other") || !p.enabled("apple-gmux", "laptop-a") || !p.enabled("intel-edp", "laptop-b") {
		t.Fatal("expected node override, then class override, then default enabled")
	}
}

func TestTimedDimRestoration(t *testing.T) {
	p := &fixture{value: 67828, max: 82311}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	if err := c.dim(25, 45*time.Second); err != nil || p.value != 20578 || c.deadline.IsZero() {
		t.Fatalf("timed dim: brightness=%d error=%v", p.value, err)
	}
	if err := c.restore(false); err != nil || p.value != 67828 {
		t.Fatalf("timed dim restoration: brightness=%d error=%v", p.value, err)
	}
}

func TestShutdownTriggerOverrides(t *testing.T) {
	disabled := false
	p := shutdownPolicy{Triggers: map[string]shutdownOverrides{
		"laptop-a": {LocalShutdown: &disabled, APIReboot: &disabled},
	}}
	if p.allows("apple-gmux", "laptop-a", "shutdown", false) ||
		p.allows("apple-gmux", "laptop-a", "reboot", true) ||
		!p.allows("apple-gmux", "laptop-a", "shutdown", true) ||
		!p.allows("intel-edp", "laptop-b", "shutdown", false) {
		t.Fatal("trigger overrides must affect only the configured node and trigger")
	}
}

func TestAbortedShutdownAcceptsNextCommand(t *testing.T) {
	p := &fixture{value: 20, max: 100}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	listener := &shutdownListener{handled: make(chan struct{}, 1)}
	allowed := func(string, bool) bool { return true }
	for _, action := range []machine.SequenceEvent_Action{machine.SequenceEvent_START, machine.SequenceEvent_STOP} {
		data, err := anypb.New(&machine.SequenceEvent{Sequence: "shutdown", Action: action})
		if err != nil {
			t.Fatal(err)
		}
		data.TypeUrl = "talos/runtime/machine.SequenceEvent"
		listener.handle(c, normalLevel{75, 100}, allowed, &machine.Event{Data: data, ActorId: "test"})
	}
	if c.shuttingDown || p.value != 75 {
		t.Fatalf("aborted shutdown left write lock=%t brightness=%d", c.shuttingDown, p.value)
	}
}

func TestShutdownGuardSurvivesEventStreamLoss(t *testing.T) {
	for _, stopEvent := range []bool{true, false} {
		name := "timeout"
		if stopEvent {
			name = "matching stop"
		}
		t.Run(name, func(t *testing.T) {
			p := &fixture{value: 20, max: 100}
			c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
			listener := &shutdownListener{handled: make(chan struct{}, 1)}
			defer listener.stop()
			send := func(action machine.SequenceEvent_Action) {
				t.Helper()
				data, err := anypb.New(&machine.SequenceEvent{Sequence: "shutdown", Action: action})
				if err != nil {
					t.Fatal(err)
				}
				data.TypeUrl = "talos/runtime/machine.SequenceEvent"
				listener.handle(c, normalLevel{75, 100}, func(string, bool) bool { return true },
					&machine.Event{Data: data, ActorId: "test"})
			}
			send(machine.SequenceEvent_START)
			grace := shutdownStreamLossGrace
			if !stopEvent {
				grace = 100 * time.Millisecond
			}
			listener.streamLost(c, grace)
			response := httptest.NewRecorder()
			c.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/off", nil))
			if response.Code != http.StatusConflict || p.value != 75 {
				t.Fatalf("stream loss allowed blackout: HTTP %d, brightness %d", response.Code, p.value)
			}
			if stopEvent {
				send(machine.SequenceEvent_STOP)
			}
			deadline := time.Now().Add(time.Second)
			for {
				c.mu.Lock()
				protected := c.shuttingDown
				c.mu.Unlock()
				if !protected {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("shutdown guard did not clear")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
