package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	socket = "/run/backlight/control.sock"
	maxOff = 10 * time.Minute
)

type panel struct {
	attributes string
	brightness string
}

type sample struct {
	Requested int `json:"requested"`
	Actual    int `json:"actual"`
	Maximum   int `json:"maximum"`
	Power     int `json:"power"`
}

func (p panel) read() (sample, error) {
	var values [4]int
	for i, name := range []string{"brightness", "actual_brightness", "max_brightness", "bl_power"} {
		data, err := os.ReadFile(filepath.Join(p.attributes, name))
		if err != nil {
			return sample{}, err
		}
		value := strings.TrimSpace(string(data))
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 0xffffff || strconv.Itoa(n) != value {
			return sample{}, fmt.Errorf("invalid %s", name)
		}
		values[i] = n
	}
	s := sample{values[0], values[1], values[2], values[3]}
	if s.Maximum < 1 || s.Requested > s.Maximum || s.Actual > s.Maximum {
		return sample{}, errors.New("brightness out of range")
	}
	return s, nil
}

func (p panel) set(value, maximum int) error {
	// No truncate or create flags. Sysfs accepts one decimal write per open.
	f, err := os.OpenFile(p.brightness, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.Itoa(value) + "\n")
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	s, err := p.read()
	if err == nil && s != (sample{value, value, maximum, 0}) {
		err = fmt.Errorf("unexpected brightness readback %+v", s)
	}
	return err
}

type saved struct {
	Brightness int  `json:"brightness"`
	Maximum    int  `json:"maximum"`
	Indefinite bool `json:"indefinite,omitempty"`
	Target     *int `json:"target,omitempty"`
}

type backlight interface {
	read() (sample, error)
	set(value, maximum int) error
}

type controller struct {
	mu                 sync.Mutex
	panel              backlight
	journal            string
	pending            *saved
	deadline           time.Time
	shuttingDown       bool
	recoveryError      error
	listenerReady      func() bool
	pendingStartupBoot string
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (c *controller) save(s saved) error {
	return saveState(c.journal, s)
}

func saveState(path string, value any) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := errors.Join(json.NewEncoder(f).Encode(value), f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func (c *controller) loadRestoration() {
	if err := c.recover(); err != nil {
		c.pending = nil
		c.recoveryError = err
		log.Printf("restoration journal needs manual recovery: %v", err)
	}
}

func (c *controller) recover() error {
	data, err := os.ReadFile(c.journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var s saved
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("read restoration journal: %w", err)
	}
	if s.Target != nil && (*s.Target <= 0 || *s.Target > s.Maximum) {
		return errors.New("invalid saved brightness target")
	}
	c.pending = &s
	if s.Target == nil && s.Indefinite && s.Brightness > 0 && s.Brightness <= s.Maximum {
		current, err := c.panel.read()
		if err != nil {
			return err
		}
		if current == (sample{0, 0, s.Maximum, 0}) {
			return nil
		}
	}
	return c.restore(false)
}

func (c *controller) restore(manual bool) error {
	current, err := c.panel.read()
	if err != nil {
		return err
	}
	if c.pending == nil {
		if !manual {
			return nil
		}
		if current.Requested > 0 && current.Actual > 0 && current.Power == 0 {
			if c.recoveryError != nil {
				return c.clearJournal()
			}
			return nil
		}
		if current.Requested != 0 || current.Actual != 0 || current.Power != 0 {
			return errors.New("cannot safely infer a restoration value")
		}
		if err := c.panel.set(percentLevel(current.Maximum, 75), current.Maximum); err != nil {
			return err
		}
		if c.recoveryError != nil {
			return c.clearJournal()
		}
		return nil
	}
	s := *c.pending
	if s.Maximum != current.Maximum || s.Brightness <= 0 || s.Brightness > s.Maximum || current.Power != 0 {
		return errors.New("saved brightness or current panel state failed validation")
	}
	if s.Target != nil && current == (sample{*s.Target, *s.Target, s.Maximum, 0}) {
		return c.clearJournal()
	}
	if err := c.panel.set(s.Brightness, s.Maximum); err != nil {
		return err
	}
	return c.clearJournal()
}

func (c *controller) clearJournal() error {
	if err := os.Remove(c.journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(filepath.Dir(c.journal)); err != nil {
		return err
	}
	c.pending = nil
	c.recoveryError = nil
	c.deadline = time.Time{}
	return nil
}

func (c *controller) off(duration time.Duration) error {
	if duration == 0 {
		current, err := c.panel.read()
		if err != nil {
			return err
		}
		if current.Requested == 0 && current.Actual == 0 && current.Power == 0 {
			return c.setBrightness("0", false)
		}
	}
	return c.dim(0, duration)
}

func (c *controller) dim(percent int, duration time.Duration) error {
	if c.recoveryError != nil {
		return errors.New("restore the panel with on or a positive brightness before dimming")
	}
	if c.pending != nil {
		return errors.New("restoration already armed; use on to restore it")
	}
	current, err := c.panel.read()
	if err != nil {
		return err
	}
	if current.Requested == 0 || current.Requested != current.Actual || current.Power != 0 {
		return errors.New("expected a lit, unblanked baseline with matching brightness readings")
	}
	s := saved{Brightness: current.Requested, Maximum: current.Maximum, Indefinite: duration == 0}
	target := (current.Maximum*percent + 50) / 100
	if percent > 0 && (target < 1 || target >= current.Requested) {
		return errors.New("trial requires a positive target below the current brightness")
	}
	if err := c.save(s); err != nil {
		return err
	}
	c.pending = &s
	if duration > 0 {
		c.deadline = time.Now().Add(duration)
	}
	if err := c.panel.set(target, current.Maximum); err != nil {
		c.deadline = time.Now()
		return errors.Join(err, c.restore(false))
	}
	return nil
}

func (c *controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shuttingDown && r.Method == "POST" {
		http.Error(w, "node shutdown or reboot is in progress", http.StatusConflict)
		return
	}

	var err error
	switch {
	case r.Method == "GET" && r.URL.Path == "/status":
	case r.Method == "POST" && (r.URL.Path == "/set" || r.URL.Path == "/percent"):
		err = c.setBrightness(r.URL.Query().Get("value"), r.URL.Path == "/percent")
	case r.Method == "POST" && r.URL.Path == "/on":
		err = c.restore(true)
	case r.Method == "POST" && r.URL.Path == "/off":
		var duration time.Duration
		if r.URL.Query().Has("for") {
			var parseErr error
			duration, parseErr = time.ParseDuration(r.URL.Query().Get("for"))
			if parseErr != nil || duration < time.Second || duration > maxOff {
				http.Error(w, "off duration must be between 1s and 10m", http.StatusBadRequest)
				return
			}
		}
		err = c.off(duration)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if r.Method == "POST" && c.pendingStartupBoot != "" {
		if err := c.completeStartup(); err != nil {
			http.Error(w, fmt.Sprintf("brightness command applied but startup override could not be saved: %v", err), http.StatusInternalServerError)
			return
		}
	}
	current, err := c.panel.read()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	result := struct {
		sample
		RestoreAt         *time.Time `json:"restore_at,omitempty"`
		OffUntilOn        bool       `json:"off_until_on,omitempty"`
		ListenerConnected bool       `json:"shutdown_listener_connected"`
		RestorationReady  bool       `json:"shutdown_restoration_ready"`
		RestorationError  string     `json:"restoration_error,omitempty"`
	}{sample: current}
	if c.listenerReady != nil {
		result.ListenerConnected = c.listenerReady()
	}
	result.RestorationReady = result.ListenerConnected && c.recoveryError == nil &&
		!c.shuttingDown && current.Power == 0 && current.Requested == current.Actual
	if c.recoveryError != nil {
		result.RestorationError = c.recoveryError.Error()
	}
	if c.pending != nil {
		if c.deadline.IsZero() {
			result.OffUntilOn = true
		} else {
			result.RestoreAt = &c.deadline
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Print(err)
	}
}

func (c *controller) watchdog(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			c.mu.Lock()
			if !c.shuttingDown && c.pending != nil && !c.deadline.IsZero() && !now.Before(c.deadline) {
				if err := c.restore(false); err != nil {
					log.Printf("restore failed, retrying: %v", err)
					c.deadline = time.Now().Add(time.Second)
				}
			}
			c.mu.Unlock()
		}
	}
}

func checkedPanel() (panel, error) {
	const device = "/panel"
	p := panel{device, "/brightness"}
	var kind string
	switch os.Getenv("BACKLIGHT_CLASS") {
	case "apple-gmux":
		kind = "platform"
	case "intel-edp":
		kind = "raw"
	default:
		return p, errors.New("expected a supported backlight class")
	}
	data, err := os.ReadFile(device + "/type")
	if err != nil || strings.TrimSpace(string(data)) != kind {
		return p, fmt.Errorf("expected a %s backlight", kind)
	}
	class, err := os.Stat(device + "/brightness")
	if err != nil {
		return p, err
	}
	bound, err := os.Lstat(p.brightness)
	if err != nil || !bound.Mode().IsRegular() || !os.SameFile(class, bound) {
		return p, errors.New("brightness bind does not match the panel device")
	}
	return p, nil
}

// Hold the host-state lock until the process exits, including during restoration.
func lockPanel(stateDir string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(stateDir, "controller.lock"),
		os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another controller holds the host backlight lock: %w", err)
	}
	return lock, nil
}

func serve() error {
	p, err := checkedPanel()
	if err != nil {
		return err
	}
	c := &controller{panel: p, journal: "/state/restore.json"}
	lock, err := lockPanel(filepath.Dir(c.journal))
	if err != nil {
		return err
	}
	defer lock.Close()
	_, journalErr := os.Stat(c.journal)
	normal, err := c.normalBrightness()
	if err != nil {
		log.Printf("shutdown brightness baseline unavailable: %v", err)
	}
	c.loadRestoration()
	if err := c.startup("/etc/backlight/levels.json", "/proc/sys/kernel/random/boot_id",
		os.Getenv("BACKLIGHT_CLASS"), os.Getenv("BACKLIGHT_NODE"), journalErr == nil); err != nil {
		log.Printf("startup brightness: %v", err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	listener := startShutdownListener(c, normal)
	c.listenerReady = listener.ready.Load
	defer listener.stop()
	server := &http.Server{Handler: c, ReadHeaderTimeout: time.Second}
	go c.watchdog(ctx)
	served := make(chan error, 1)
	go func() { served <- server.Serve(l) }()
	select {
	case err = <-served:
	case <-ctx.Done():
		err = server.Close()
		listener.awaitEvent()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shuttingDown || c.pending != nil && c.deadline.IsZero() {
		return err
	}
	return errors.Join(err, c.restore(false))
}

func command(args []string) error {
	if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[0] != "off" && args[0] != "set" && args[0] != "percent") {
		return errors.New("usage: backlight status | on | off [duration] | set LEVEL | percent PERCENT")
	}
	method, path := "POST", "/"+args[0]
	switch args[0] {
	case "status":
		method = "GET"
	case "on":
	case "set", "percent":
		if len(args) != 2 {
			return errors.New("expected a brightness value")
		}
		if _, err := brightnessInteger(args[1]); err != nil {
			return err
		}
		path += "?value=" + args[1]
	case "off":
		if len(args) == 2 {
			duration, err := time.ParseDuration(args[1])
			if err != nil {
				return err
			}
			path += "?for=" + duration.String()
		}
	default:
		return errors.New("unknown backlight command")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, err := http.NewRequest(method, "http://backlight"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(strings.TrimSpace(string(body)))
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func main() {
	var err error
	if len(os.Args) == 2 && os.Args[1] == "serve" {
		err = serve()
	} else {
		err = command(os.Args[1:])
	}
	if err != nil {
		log.Fatal(err)
	}
}
