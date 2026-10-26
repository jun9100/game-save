// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBarkTTL       = 600
	defaultBarkTimeout   = 5 * time.Second
	barkQueueCapacity    = 32
	barkDedupeWindow     = 5 * time.Minute
	barkMaximumEventKeys = 64
)

var barkEventKinds = map[string]struct{}{
	eventConflict:       {},
	eventUpload:         {},
	eventRollback:       {},
	eventDeleteSnapshot: {},
	eventDeleteScope:    {},
	eventPrune:          {},
	eventDeviceNew:      {},
	eventDeviceRemoved:  {},
	eventVerify:         {},
	eventSettings:       {},
}

// barkConfig is intentionally environment-only: device keys are URLs and should never be
// persisted in the service data directory or exposed by the dashboard.
type barkConfig struct {
	Endpoint string
	Group    string
	TTL      int
	Events   string
	Timeout  time.Duration
}

type barkMessage struct {
	Title string
	Body  string
	Key   string
}

type barkNotifier struct {
	endpoint *url.URL
	group    string
	ttl      int
	events   map[string]struct{}
	timeout  time.Duration
	client   *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	queue  chan barkMessage
	done   chan struct{}

	mu   sync.Mutex
	seen map[string]time.Time
}

func newBarkNotifier(config barkConfig) (*barkNotifier, error) {
	if config.Endpoint == "" {
		return nil, nil
	}
	endpoint, err := normalizeBarkEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	if config.TTL < 0 || config.TTL > 30*24*60*60 {
		return nil, errors.New("GAME_SAVE_BARK_TTL must be between 0 and 2592000 seconds")
	}
	events, err := parseBarkEvents(config.Events)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, errors.New("GAME_SAVE_BARK_EVENTS must include at least one event")
	}
	if config.TTL == 0 {
		config.TTL = defaultBarkTTL
	}
	if config.Timeout == 0 {
		config.Timeout = defaultBarkTimeout
	}
	if config.Timeout <= 0 {
		return nil, errors.New("GAME_SAVE_BARK_TIMEOUT must be positive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	notifier := &barkNotifier{
		endpoint: endpoint,
		group:    strings.TrimSpace(config.Group),
		ttl:      config.TTL,
		events:   events,
		timeout:  config.Timeout,
		client:   &http.Client{},
		ctx:      ctx,
		cancel:   cancel,
		queue:    make(chan barkMessage, barkQueueCapacity),
		done:     make(chan struct{}),
		seen:     make(map[string]time.Time),
	}
	go notifier.run()
	return notifier, nil
}

// normalizeBarkEndpoint accepts both a device endpoint and the full URL copied from a Bark curl
// example. A full URL ends in /title/body; those two placeholder path segments must not be retained
// when game-save supplies its own message.
func normalizeBarkEndpoint(value string) (*url.URL, error) {
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(value))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" ||
		(endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil {
		return nil, errors.New("GAME_SAVE_BARK_URL must be an absolute http or https URL without credentials")
	}

	parts := strings.Split(strings.Trim(endpoint.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return nil, errors.New("GAME_SAVE_BARK_URL must include the Bark device path")
	}
	if len(parts) >= 3 {
		parts = parts[:len(parts)-2]
	}
	endpoint.Path = "/" + strings.Join(parts, "/")
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	endpoint.RawFragment = ""
	return endpoint, nil
}

func parseBarkEvents(value string) (map[string]struct{}, error) {
	if value == "" {
		value = eventConflict
	}
	events := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := barkEventKinds[item]; !ok {
			return nil, fmt.Errorf("GAME_SAVE_BARK_EVENTS contains unsupported event %q", item)
		}
		events[item] = struct{}{}
	}
	return events, nil
}

func (n *barkNotifier) Close() {
	if n == nil {
		return
	}
	n.cancel()
	<-n.done
}

func (n *barkNotifier) enqueue(message barkMessage) {
	if n == nil || message.Title == "" || message.Body == "" {
		return
	}
	select {
	case <-n.ctx.Done():
		return
	default:
	}
	if !n.shouldSend(message.Key, time.Now().UTC()) {
		return
	}
	select {
	case n.queue <- message:
	case <-n.ctx.Done():
		n.release(message.Key)
	default:
		n.release(message.Key)
		slog.Warn("Bark notification queue is full; notification dropped")
	}
}

func (n *barkNotifier) shouldSend(key string, now time.Time) bool {
	if key == "" {
		key = "message"
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for previousKey, sentAt := range n.seen {
		if now.Sub(sentAt) > barkDedupeWindow {
			delete(n.seen, previousKey)
		}
	}
	if sentAt, ok := n.seen[key]; ok && now.Sub(sentAt) < barkDedupeWindow {
		return false
	}
	if len(n.seen) >= barkMaximumEventKeys {
		for previousKey := range n.seen {
			delete(n.seen, previousKey)
			break
		}
	}
	n.seen[key] = now
	return true
}

func (n *barkNotifier) release(key string) {
	if key == "" {
		key = "message"
	}
	n.mu.Lock()
	delete(n.seen, key)
	n.mu.Unlock()
}

func (n *barkNotifier) run() {
	defer close(n.done)
	for {
		select {
		case <-n.ctx.Done():
			return
		case message := <-n.queue:
			if err := n.send(message); err != nil {
				n.release(message.Key)
				slog.Warn("send Bark notification", "error", err)
			}
		}
	}
}

func (n *barkNotifier) send(message barkMessage) error {
	endpoint := *n.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + barkPathPart(message.Title) + "/" + barkPathPart(message.Body)
	endpoint.RawPath = ""
	query := endpoint.Query()
	if n.group != "" {
		query.Set("group", n.group)
	}
	query.Set("ttl", strconv.Itoa(n.ttl))
	endpoint.RawQuery = query.Encode()

	ctx, cancel := context.WithTimeout(n.ctx, n.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	response, err := n.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("Bark returned HTTP %d", response.StatusCode)
	}
	return nil
}

func barkPathPart(value string) string {
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		if character == '/' {
			return '-'
		}
		return character
	}, strings.TrimSpace(value))
	return truncateRunes(value, 512)
}

func truncateRunes(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	count := 0
	for index := range value {
		if count == maximum {
			return value[:index]
		}
		count++
	}
	return value
}

func (s *server) notifyBarkLocked(item event) {
	if s.bark == nil {
		return
	}
	if _, ok := s.bark.events[item.Kind]; !ok {
		return
	}

	scopeName := "A game save"
	if openSaveName, ok := openSaveEventScopeName(item.Scope); ok {
		scopeName = openSaveName
	} else if item.Scope != "" {
		if title := strings.TrimSpace(s.dashboard.Scopes[item.Scope].Title); title != "" {
			scopeName = title
		}
	}
	deviceName := "an unknown device"
	if item.Device != "" {
		device := s.dashboard.Devices[item.Device]
		deviceName = strings.TrimSpace(device.Alias)
		if deviceName == "" {
			deviceName = strings.TrimSpace(device.Name)
		}
		if deviceName == "" {
			deviceName = item.Device
		}
	}

	title, body := barkText(item, scopeName, deviceName)
	keyParts := []string{item.Kind, item.Scope, item.Device}
	if item.Kind != eventConflict {
		keyParts = append(keyParts, item.Detail)
	}
	key := strings.Join(keyParts, "\x00")
	s.bark.enqueue(barkMessage{Title: title, Body: body, Key: key})
}

func barkText(item event, scopeName, deviceName string) (string, string) {
	switch item.Kind {
	case eventConflict:
		return "Game Save conflict", scopeName + " from " + deviceName + " conflicts with the current cloud head. A conflict copy was retained for review."
	case eventUpload:
		return "Game Save uploaded", scopeName + " was uploaded from " + deviceName + "."
	case eventRollback:
		return "Game Save rollback", scopeName + " was rolled back to an earlier snapshot."
	case eventVerify:
		return "Game Save verification", item.Detail
	case eventDeviceNew:
		return "Game Save device", deviceName + " connected."
	case eventDeviceRemoved:
		return "Game Save device removed", item.Detail
	case eventDeleteSnapshot:
		return "Game Save snapshot deleted", scopeName + " had a snapshot deleted."
	case eventDeleteScope:
		return "Game Save deleted", item.Detail
	case eventPrune:
		return "Game Save retention", item.Detail
	case eventSettings:
		return "Game Save settings", item.Detail
	default:
		return "Game Save event", item.Detail
	}
}
