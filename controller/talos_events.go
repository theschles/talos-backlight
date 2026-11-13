package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

const shutdownStreamLossGrace = 2 * time.Minute

type shutdownListener struct {
	mu              sync.Mutex
	streamLossTimer *time.Timer
	cancel          context.CancelFunc
	ready           atomic.Bool
	handled         chan struct{}
	sequence        string
	actor           string
}

func startShutdownListener(c *controller, normal normalLevel) *shutdownListener {
	listener := &shutdownListener{}
	endpoint := os.Getenv("BACKLIGHT_TALOS_HOST")
	if endpoint == "" || !normal.valid() {
		return listener
	}
	config, err := os.ReadFile(brightnessConfig)
	if err != nil {
		log.Printf("shutdown brightness policy unavailable: %v", err)
		return listener
	}
	var settings struct {
		Shutdown shutdownPolicy `json:"shutdown"`
	}
	if err := json.Unmarshal(config, &settings); err != nil {
		log.Printf("shutdown brightness policy unavailable: %v", err)
		return listener
	}
	class, node := os.Getenv("BACKLIGHT_CLASS"), os.Getenv("BACKLIGHT_NODE")
	allowed := func(sequence string, fromAPI bool) bool {
		return settings.Shutdown.allows(class, node, sequence, fromAPI)
	}
	if _, err := netip.ParseAddr(endpoint); err != nil {
		log.Printf("shutdown brightness requires a local node IP")
		return listener
	}
	ctx, cancel := context.WithCancel(context.Background())
	listener.cancel = cancel
	listener.handled = make(chan struct{}, 1)
	go listener.run(ctx, endpoint, "/etc/backlight-talos/talosconfig", c, normal, allowed)
	return listener
}

func (l *shutdownListener) stop() {
	if l.cancel != nil {
		l.cancel()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopStreamLossTimer()
}

func (l *shutdownListener) awaitEvent() {
	if !l.ready.Load() {
		return
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-l.handled:
	case <-timer.C:
	}
}

func (l *shutdownListener) handle(c *controller, normal normalLevel, allowed func(string, bool) bool, event *machine.Event) {
	sequence := shutdownSequence(event)
	if sequence == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	name, actor := sequence.GetSequence(), event.GetActorId()
	switch sequence.GetAction() {
	case machine.SequenceEvent_START:
		if !allowed(name, actor != "") || l.sequence == name && l.actor == actor {
			return
		}
		l.resetSequenceNotification()
		l.sequence, l.actor = name, actor
		log.Printf("Talos %s started", name)
		if err := c.restoreForShutdown(normal); err != nil {
			log.Printf("shutdown brightness restoration failed: %v", err)
		}
		l.handled <- struct{}{}
	case machine.SequenceEvent_STOP:
		if l.sequence == name && l.actor == actor {
			l.finish(c)
		}
	}
}

// finish clears the sequence while the listener mutex is held.
func (l *shutdownListener) finish(c *controller) {
	l.resetSequenceNotification()
	l.sequence, l.actor = "", ""
	c.mu.Lock()
	c.shuttingDown = false
	c.mu.Unlock()
}

func (l *shutdownListener) resetSequenceNotification() {
	l.stopStreamLossTimer()
	select {
	case <-l.handled:
	default:
	}
}

func (l *shutdownListener) stopStreamLossTimer() {
	if l.streamLossTimer != nil {
		l.streamLossTimer.Stop()
		l.streamLossTimer = nil
	}
}

func (l *shutdownListener) streamLost(c *controller, grace time.Duration) {
	l.ready.Store(false)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sequence == "" || l.streamLossTimer != nil {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(grace, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.streamLossTimer == timer {
			log.Printf("Talos %s outcome unavailable after %s; allowing brightness commands", l.sequence, grace)
			l.finish(c)
		}
	})
	l.streamLossTimer = timer
}

func (l *shutdownListener) run(ctx context.Context, endpoint, config string, c *controller, normal normalLevel, allowed func(string, bool) bool) {
	for ctx.Err() == nil {
		err := l.watch(ctx, endpoint, config, func(event *machine.Event) {
			l.handle(c, normal, allowed, event)
		})
		if ctx.Err() != nil {
			l.ready.Store(false)
			return
		}
		l.streamLost(c, shutdownStreamLossGrace)
		log.Printf("Talos shutdown event stream unavailable: %v", err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func (l *shutdownListener) watch(ctx context.Context, endpoint, config string, handle func(*machine.Event)) error {
	creds, err := readerCredentials(config)
	if err != nil {
		return err
	}
	talos, err := client.New(ctx, client.WithConfigContext(creds), client.WithEndpoints(endpoint))
	if err != nil {
		return err
	}
	defer talos.Close()
	stream, err := talos.Events(ctx)
	if err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetData().GetTypeUrl() != "" {
		return errors.New("expected the Talos event subscription greeting")
	}
	l.ready.Store(true)
	log.Print("Talos shutdown event stream connected")
	for {
		event, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("receive Talos event: %w", err)
		}
		handle(event)
	}
}

func readerCredentials(path string) (*clientconfig.Context, error) {
	config, err := clientconfig.Open(path)
	if err != nil {
		return nil, err
	}
	creds := config.Contexts[config.Context]
	if creds == nil || creds.CA == "" || creds.Crt == "" || creds.Key == "" {
		return nil, errors.New("expected a Talos client certificate and CA")
	}
	raw, err := base64.StdEncoding.DecodeString(creds.Crt)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid Talos client certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	roles := cert.Subject.Organization
	if len(roles) != 1 || roles[0] != "os:reader" {
		return nil, errors.New("Talos shutdown event credential must have only the os:reader role")
	}
	return &clientconfig.Context{CA: creds.CA, Crt: creds.Crt, Key: creds.Key}, nil
}

func shutdownSequence(event *machine.Event) *machine.SequenceEvent {
	var sequence machine.SequenceEvent
	data := event.GetData()
	if data == nil || data.GetTypeUrl() != "talos/runtime/machine.SequenceEvent" ||
		data.UnmarshalTo(&sequence) != nil {
		return nil
	}
	switch sequence.GetSequence() {
	case "shutdown", "reboot":
		return &sequence
	default:
		return nil
	}
}
