package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type levels struct {
	Classes map[string]int `json:"classes"`
	Nodes   map[string]int `json:"nodes"`
}

func (l levels) startupPercent(class, node string) (int, error) {
	n, ok := l.Nodes[node]
	if !ok {
		n, ok = l.Classes[class]
	}
	if ok && (n < 1 || n > 100) {
		return 0, fmt.Errorf("startup brightness must be between 1 and 100 percent")
	}
	return n, nil
}

func (c *controller) startup(config, bootIDPath, class, node string, hadJournal bool) error {
	data, err := os.ReadFile(config)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var l levels
	if err := json.Unmarshal(data, &l); err != nil {
		return err
	}
	percent, err := l.startupPercent(class, node)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(bootIDPath)
	if err != nil {
		return err
	}
	bootID := strings.TrimSpace(string(raw))
	if len(bootID) != 36 {
		return errors.New("invalid kernel boot ID")
	}
	marker := filepath.Join(filepath.Dir(c.journal), "startup-boot.json")
	if raw, err := os.ReadFile(marker); err == nil {
		var previous string
		if err := json.Unmarshal(raw, &previous); err != nil {
			return err
		}
		if previous == bootID {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	c.pendingStartupBoot = bootID
	if percent == 0 || hadJournal || c.pending != nil || c.recoveryError != nil {
		return c.completeStartup()
	}
	current, err := c.panel.read()
	if err != nil {
		return err
	}
	if current.Power != 0 || current.Requested != current.Actual {
		return errors.New("panel is not unblanked with matching brightness readings")
	}
	value := max(1, (current.Maximum*percent+50)/100)
	if current.Requested != value {
		if err := c.panel.set(value, current.Maximum); err != nil {
			return err
		}
	}
	return c.completeStartup()
}

func (c *controller) completeStartup() error {
	marker := filepath.Join(filepath.Dir(c.journal), "startup-boot.json")
	if err := saveState(marker, c.pendingStartupBoot); err != nil {
		return err
	}
	c.pendingStartupBoot = ""
	return nil
}
