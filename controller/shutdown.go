package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const brightnessConfig = "/etc/backlight/levels.json"

type shutdownOverrides struct {
	LocalShutdown *bool `json:"localShutdown"`
	APIShutdown   *bool `json:"apiShutdown"`
	APIReboot     *bool `json:"apiReboot"`
}

type shutdownPolicy struct {
	Triggers map[string]shutdownOverrides `json:"triggers"`
	Classes  map[string]bool              `json:"classes"`
	Nodes    map[string]bool              `json:"nodes"`
}

func (p shutdownPolicy) enabled(class, node string) bool {
	if enabled, ok := p.Nodes[node]; ok {
		return enabled
	}
	enabled, ok := p.Classes[class]
	return !ok || enabled
}

// Unspecified triggers retain the existing node/class policy.
func (p shutdownPolicy) allows(class, node, sequence string, fromAPI bool) bool {
	settings := p.Triggers[node]
	var override *bool
	switch {
	case sequence == "shutdown" && fromAPI:
		override = settings.APIShutdown
	case sequence == "shutdown":
		override = settings.LocalShutdown
	case sequence == "reboot" && fromAPI:
		override = settings.APIReboot
	}
	if override != nil {
		return *override
	}
	return p.enabled(class, node)
}

type normalLevel struct {
	Brightness int `json:"brightness"`
	Maximum    int `json:"maximum"`
}

func (n normalLevel) valid() bool {
	return n.Brightness > 0 && n.Brightness <= n.Maximum && n.Maximum <= 0xffffff
}

func (n normalLevel) atMaximum(maximum int) int {
	return max(1, int((int64(maximum)*int64(n.Brightness)+int64(n.Maximum)/2)/int64(n.Maximum)))
}

// Record the normal level before startup or blackout recovery changes the panel.
// A startup marker or restore journal indicates enrollment under an older controller.
func (c *controller) normalBrightness() (normalLevel, error) {
	dir := filepath.Dir(c.journal)
	path := filepath.Join(dir, "normal.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var normal normalLevel
		if err := json.Unmarshal(data, &normal); err != nil {
			return normalLevel{}, err
		}
		if !normal.valid() {
			return normalLevel{}, errors.New("invalid saved normal brightness")
		}
		return normal, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return normalLevel{}, err
	}
	normal := normalLevel{75, 100}
	_, bootErr := os.Stat(filepath.Join(dir, "startup-boot.json"))
	_, journalErr := os.Stat(c.journal)
	for _, err := range []error{bootErr, journalErr} {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return normalLevel{}, err
		}
	}
	if errors.Is(bootErr, os.ErrNotExist) && errors.Is(journalErr, os.ErrNotExist) {
		current, err := c.panel.read()
		if err != nil {
			return normalLevel{}, err
		}
		if current.Power == 0 && current.Requested > 0 && current.Requested == current.Actual {
			normal = normalLevel{current.Requested, current.Maximum}
		}
	}
	return normal, saveState(path, normal)
}

func (c *controller) restoreForShutdown(normal normalLevel) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shuttingDown = true
	if c.recoveryError != nil {
		return c.recoveryError
	}
	current, err := c.panel.read()
	if err != nil {
		return err
	}
	if current.Power != 0 || current.Requested != current.Actual {
		return errors.New("panel is not unblanked with matching brightness readings")
	}
	target := normal.atMaximum(current.Maximum)
	if current.Requested < target {
		if err := c.setLit(target, current.Maximum); err != nil {
			return err
		}
		fmt.Printf("SHUTDOWN_BRIGHTNESS restored=%d previous=%d\n", target, current.Requested)
		return nil
	}
	if c.pending != nil {
		return c.clearJournal()
	}
	return nil
}
