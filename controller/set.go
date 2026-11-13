package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

func brightnessInteger(raw string) (int, error) {
	if raw == "" {
		return 0, errors.New("brightness must be a nonnegative integer")
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return 0, errors.New("brightness must be a nonnegative integer")
		}
	}
	return strconv.Atoi(raw)
}

func percentLevel(maximum, percent int) int {
	return int((int64(maximum)*int64(percent) + 50) / 100)
}

func (c *controller) setBrightness(raw string, percentage bool) error {
	value, err := brightnessInteger(raw)
	if err != nil {
		return err
	}
	current, err := c.panel.read()
	if err != nil {
		return err
	}
	if percentage {
		if value > 100 {
			return errors.New("percentage must be between 0 and 100")
		}
		value = percentLevel(current.Maximum, value)
	}
	if value > current.Maximum {
		return fmt.Errorf("brightness must be between 0 and %d", current.Maximum)
	}
	if current.Power != 0 || current.Requested != current.Actual {
		return errors.New("expected an unblanked panel with matching brightness readings")
	}
	if value == 0 {
		if c.recoveryError != nil {
			return errors.New("restore the panel with on or a positive brightness before dimming")
		}
		if current.Requested == 0 && c.deadline.IsZero() {
			return nil
		}
		if c.pending == nil {
			return c.off(0)
		}
	}
	if value > 0 {
		return c.setLit(value, current.Maximum)
	}
	if err := c.panel.set(value, current.Maximum); err != nil {
		return err
	}
	if c.pending != nil {
		state := *c.pending
		state.Indefinite = true
		state.Target = nil
		if err := c.save(state); err != nil {
			return err
		}
		c.pending = &state
		c.deadline = time.Time{}
	}
	return nil
}

func (c *controller) setLit(value, maximum int) error {
	if c.pending != nil {
		state := *c.pending
		state.Target = &value
		if err := c.save(state); err != nil {
			return err
		}
		c.pending = &state
	}
	if err := c.panel.set(value, maximum); err != nil {
		return err
	}
	if c.pending != nil || c.recoveryError != nil {
		return c.clearJournal()
	}
	return nil
}
