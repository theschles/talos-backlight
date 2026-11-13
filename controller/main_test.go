package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	value int
	max   int
	fail  bool
}

func (f *fixture) read() (sample, error) {
	return sample{f.value, f.value, f.max, 0}, nil
}

func (f *fixture) set(value, _ int) error {
	f.value = value
	if value == 0 && f.fail {
		return errors.New("simulated write error after changing hardware")
	}
	return nil
}

func TestCommands(t *testing.T) {
	p := &fixture{value: 67828, max: 82311}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	request := func(method, path string, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		c.ServeHTTP(response, httptest.NewRequest(method, path, nil))
		if response.Code != want {
			t.Fatalf("%s %s got %d: %s", method, path, response.Code, response.Body)
		}
	}

	request(http.MethodGet, "/status", http.StatusOK)
	request(http.MethodPost, "/off?for=11m", http.StatusBadRequest)
	request(http.MethodPost, "/off?for=10m", http.StatusOK)
	if p.value != 0 {
		t.Fatalf("expected off, got %d", p.value)
	}
	if _, err := os.Stat(c.journal); err != nil {
		t.Fatal(err)
	}
	request(http.MethodPost, "/off?for=10m", http.StatusConflict)
	request(http.MethodPost, "/on", http.StatusOK)
	if p.value != 67828 {
		t.Fatalf("expected original value, got %d", p.value)
	}

	request(http.MethodPost, "/off?for=10m", http.StatusOK)
	restarted := &controller{panel: p, journal: c.journal}
	if err := restarted.recover(); err != nil || p.value != 67828 {
		t.Fatalf("startup restoration failed: brightness=%d error=%v", p.value, err)
	}
	c = restarted

	p.fail = true
	request(http.MethodPost, "/off?for=10m", http.StatusConflict)
	if p.value != 67828 || c.pending != nil {
		t.Fatalf("rollback after partial write failed: brightness=%d", p.value)
	}
	p.fail = false

	request(http.MethodPost, "/off?for=1s", http.StatusOK)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchdog(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		restored := c.pending == nil && p.value == 67828
		c.mu.Unlock()
		if restored {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("local timer did not restore the saved value")
}

func TestDellRescueFromCurrentZero(t *testing.T) {
	p := &fixture{max: 7500}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	if err := c.restore(true); err != nil || p.value != 5625 {
		t.Fatalf("Dell recovery from zero: brightness=%d error=%v", p.value, err)
	}
	if err := c.off(10 * time.Second); err != nil || p.value != 0 {
		t.Fatalf("Dell off: brightness=%d error=%v", p.value, err)
	}
	if err := c.restore(true); err != nil || p.value != 5625 {
		t.Fatalf("Dell restores its recorded baseline: brightness=%d error=%v", p.value, err)
	}
	p.value, p.max = 0, 7400
	if err := c.restore(true); err != nil || p.value != 5550 {
		t.Fatalf("rescue uses the current panel maximum: brightness=%d error=%v", p.value, err)
	}
}

func TestOffWithoutDuration(t *testing.T) {
	p := &fixture{value: 3750, max: 7500}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	response := httptest.NewRecorder()
	c.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/off", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"off_until_on":true`) {
		t.Fatalf("off response: %d %s", response.Code, response.Body)
	}
	c = &controller{panel: p, journal: c.journal}
	if err := c.recover(); err != nil || p.value != 0 || c.pending == nil {
		t.Fatalf("controller restart lost indefinite blackout: brightness=%d error=%v", p.value, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchdog(ctx)
	time.Sleep(550 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.value != 0 || !c.deadline.IsZero() {
		t.Fatal("watchdog restored an indefinite blackout")
	}
	if err := c.restore(true); err != nil || p.value != 3750 || c.pending != nil {
		t.Fatalf("explicit on did not restore: brightness=%d error=%v", p.value, err)
	}
}

func TestStartupBrightness(t *testing.T) {
	dir := t.TempDir()
	config, boot := filepath.Join(dir, "levels.json"), filepath.Join(dir, "boot")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(config, `{"classes":{"intel-edp":75},"nodes":{"laptop-b":50}}`)
	write(boot, "00000000-0000-0000-0000-000000000001")
	p := &fixture{value: 7000, max: 7500}
	c := &controller{panel: p, journal: filepath.Join(dir, "restore.json")}
	if err := c.startup(config, boot, "intel-edp", "laptop-b", false); err != nil || p.value != 3750 {
		t.Fatalf("node override: brightness=%d error=%v", p.value, err)
	}
	if err := c.off(0); err != nil {
		t.Fatal(err)
	}
	restarted := &controller{panel: p, journal: c.journal}
	if err := restarted.recover(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.startup(config, boot, "intel-edp", "laptop-b", true); err != nil || p.value != 0 {
		t.Fatalf("same-boot restart: brightness=%d error=%v", p.value, err)
	}
	p.value = 7500 // Firmware brings the panel back on during a host boot.
	write(boot, "00000000-0000-0000-0000-000000000002")
	if err := restarted.recover(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.startup(config, boot, "intel-edp", "other", true); err != nil || p.value != 3750 {
		t.Fatalf("restoration takes precedence: brightness=%d error=%v", p.value, err)
	}
	write(boot, "00000000-0000-0000-0000-000000000003")
	if err := restarted.startup(config, boot, "intel-edp", "other", false); err != nil || p.value != 5625 {
		t.Fatalf("class default on a later boot: brightness=%d error=%v", p.value, err)
	}
}

func TestSetBrightness(t *testing.T) {
	p := &fixture{value: 3000, max: 7500}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	for _, raw := range []string{"-1", "1.0", "4x", "7501", ""} {
		if err := c.setBrightness(raw, false); err == nil || p.value != 3000 {
			t.Fatalf("invalid value %q: brightness=%d error=%v", raw, p.value, err)
		}
	}
	if err := c.setBrightness("101", true); err == nil || p.value != 3000 {
		t.Fatalf("invalid percentage: brightness=%d error=%v", p.value, err)
	}
	if err := c.setBrightness("25", true); err != nil || p.value != 1875 {
		t.Fatalf("set percentage: brightness=%d error=%v", p.value, err)
	}
	if err := c.setBrightness("0", false); err != nil || p.value != 0 {
		t.Fatalf("set zero: brightness=%d error=%v", p.value, err)
	}
	if err := c.restore(true); err != nil || p.value != 1875 {
		t.Fatalf("restore after zero: brightness=%d error=%v", p.value, err)
	}
	if err := c.off(time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := c.setBrightness("5500", false); err != nil || p.value != 5500 || c.pending != nil {
		t.Fatalf("set overrides timed off: brightness=%d pending=%v error=%v", p.value, c.pending, err)
	}
}

func TestRepeatedOff(t *testing.T) {
	p := &fixture{value: 3000, max: 7500}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	if err := c.off(time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := c.off(0); err != nil || !c.deadline.IsZero() || !c.pending.Indefinite {
		t.Fatalf("indefinite off must replace the timed off: %v", err)
	}
	if err := c.off(0); err != nil {
		t.Fatal(err)
	}
	if err := c.restore(true); err != nil || p.value != 3000 {
		t.Fatalf("repeated off must retain original brightness: %v", err)
	}
}

func TestRecoverCommittedSet(t *testing.T) {
	p := &fixture{value: 3000, max: 7500}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	if err := c.off(0); err != nil {
		t.Fatal(err)
	}
	target := 5500
	state := *c.pending
	state.Target = &target
	if err := c.save(state); err != nil {
		t.Fatal(err)
	}
	p.value = target
	restarted := &controller{panel: p, journal: c.journal}
	if err := restarted.recover(); err != nil || p.value != target || restarted.pending != nil {
		t.Fatalf("committed write replayed stale restoration: value=%d err=%v", p.value, err)
	}
}

func TestInvalidJournalAllowsManualRecovery(t *testing.T) {
	p := &fixture{max: 7500}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	if err := os.WriteFile(c.journal, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	c.loadRestoration()
	response := httptest.NewRecorder()
	c.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(response.Body.String(), "restoration_error") {
		t.Fatal("status did not expose the invalid journal")
	}
	response = httptest.NewRecorder()
	c.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/on", nil))
	if response.Code != http.StatusOK || p.value != 5625 || c.recoveryError != nil {
		t.Fatalf("manual recovery failed: %s", response.Body)
	}
}

func TestChangedPanelMaximumAllowsManualRecovery(t *testing.T) {
	p := &fixture{value: 3000, max: 7500}
	c := &controller{panel: p, journal: filepath.Join(t.TempDir(), "restore.json")}
	if err := c.off(0); err != nil {
		t.Fatal(err)
	}
	p.max = 5000
	restarted := &controller{panel: p, journal: c.journal}
	restarted.loadRestoration()
	if restarted.recoveryError == nil || p.value != 0 {
		t.Fatal("incompatible journal was not preserved for manual recovery")
	}
	if err := restarted.setBrightness("2500", false); err != nil || p.value != 2500 || restarted.recoveryError != nil {
		t.Fatalf("manual set did not recover the panel: brightness=%d err=%v", p.value, err)
	}
}

func TestHostPanelLock(t *testing.T) {
	dir := t.TempDir()
	first, err := lockPanel(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockPanel(dir); err == nil {
		second.Close()
		t.Fatal("two controllers acquired the same host lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := lockPanel(dir)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}

type startupReadFixture struct {
	fixture
	unreadable bool
}

func (p *startupReadFixture) read() (sample, error) {
	if p.unreadable {
		return sample{}, errors.New("transient panel read failure")
	}
	return p.fixture.read()
}

func TestStartupRetriesAfterReadFailure(t *testing.T) {
	dir := t.TempDir()
	config, boot := filepath.Join(dir, "levels.json"), filepath.Join(dir, "boot")
	if err := os.WriteFile(config, []byte(`{"nodes":{"host":50}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(boot, []byte("00000000-0000-0000-0000-000000000001"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &startupReadFixture{fixture: fixture{value: 7000, max: 7500}, unreadable: true}
	c := &controller{panel: p, journal: filepath.Join(dir, "restore.json")}
	if err := c.startup(config, boot, "intel-edp", "host", false); err == nil {
		t.Fatal("expected the first panel read to fail")
	}
	p.unreadable = false
	restarted := &controller{panel: p, journal: c.journal}
	if err := restarted.startup(config, boot, "intel-edp", "host", false); err != nil || p.value != 3750 {
		t.Fatalf("same-boot retry: brightness=%d error=%v", p.value, err)
	}
}

func TestManualBrightnessSupersedesStartupRetry(t *testing.T) {
	dir := t.TempDir()
	config, boot := filepath.Join(dir, "levels.json"), filepath.Join(dir, "boot")
	if err := os.WriteFile(config, []byte(`{"nodes":{"host":50}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(boot, []byte("00000000-0000-0000-0000-000000000001"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &startupReadFixture{fixture: fixture{value: 7000, max: 7500}, unreadable: true}
	c := &controller{panel: p, journal: filepath.Join(dir, "restore.json")}
	if err := c.startup(config, boot, "intel-edp", "host", false); err == nil {
		t.Fatal("expected the first panel read to fail")
	}
	p.unreadable = false
	response := httptest.NewRecorder()
	c.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/set?value=2100", nil))
	if response.Code != http.StatusOK || p.value != 2100 {
		t.Fatalf("manual brightness command failed: %s", response.Body)
	}
	restarted := &controller{panel: p, journal: c.journal}
	if err := restarted.startup(config, boot, "intel-edp", "host", false); err != nil || p.value != 2100 {
		t.Fatalf("same-boot retry: brightness=%d error=%v", p.value, err)
	}
}
