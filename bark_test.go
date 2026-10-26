// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBarkConflictNotification(t *testing.T) {
	requests := make(chan *url.URL, 2)
	barkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := *r.URL
		requests <- &clone
		w.WriteHeader(http.StatusOK)
	}))
	defer barkServer.Close()

	s, err := newServerWithOptions(t.TempDir(), testToken, 1024*1024, serverOptions{
		bark: barkConfig{
			Endpoint: barkServer.URL + "/device-key",
			Group:    "example",
			TTL:      600,
			Events:   eventConflict,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.mu.Lock()
	s.dashboard.Scopes["scope-1"] = scopeMetadata{Title: "Super Mario Odyssey"}
	s.dashboard.Devices["device-1"] = deviceInfo{Name: "Odin2"}
	s.recordEventLocked(event{
		Kind:     eventConflict,
		Scope:    "scope-1",
		Snapshot: "snapshot-2",
		Device:   "device-1",
		Detail:   "head snapshot-1",
	})
	s.mu.Unlock()

	select {
	case request := <-requests:
		if request.Path != "/device-key/Game Save conflict/Super Mario Odyssey from Odin2 conflicts with the current cloud head. A conflict copy was retained for review." {
			t.Fatalf("Bark path = %q", request.Path)
		}
		if request.Query().Get("group") != "example" || request.Query().Get("ttl") != "600" {
			t.Fatalf("Bark query = %q", request.RawQuery)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Bark notification")
	}
}

func TestBarkOpenSaveConflictUsesTypedScopeName(t *testing.T) {
	requests := make(chan *url.URL, 1)
	barkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := *r.URL
		requests <- &clone
		w.WriteHeader(http.StatusOK)
	}))
	defer barkServer.Close()

	s, err := newServerWithOptions(t.TempDir(), testToken, 1024*1024, serverOptions{
		bark: barkConfig{Endpoint: barkServer.URL + "/device-key", Events: eventConflict},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.mu.Lock()
	s.dashboard.Devices["desktop-1"] = deviceInfo{Name: "Test Desktop"}
	s.mu.Unlock()
	s.recordOpenSaveScopeEvent(eventConflict, "switch", "odyssey", openSaveScopeSnapshot{
		openSaveScopeCandidate: openSaveScopeCandidate{ID: "snapshot-2", Archive: "odyssey__main__snapshot-2.zip"},
		Device:                 "desktop-1",
	})

	select {
	case request := <-requests:
		if request.Path != "/device-key/Game Save conflict/OpenSave switch - odyssey from Test Desktop conflicts with the current cloud head. A conflict copy was retained for review." {
			t.Fatalf("Bark path = %q", request.Path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OpenSave conflict notification")
	}
}

func TestBarkCoalescesDuplicateConflict(t *testing.T) {
	requests := make(chan struct{}, 2)
	barkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer barkServer.Close()

	notifier, err := newBarkNotifier(barkConfig{Endpoint: barkServer.URL + "/device-key", Events: eventConflict})
	if err != nil {
		t.Fatal(err)
	}
	defer notifier.Close()

	message := barkMessage{Title: "Game Save conflict", Body: "same event", Key: "conflict\x00scope\x00device\x00head"}
	notifier.enqueue(message)
	notifier.enqueue(message)
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first Bark notification")
	}
	select {
	case <-requests:
		t.Fatal("duplicate Bark notification was not coalesced")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBarkRetriesAfterAFailedNotification(t *testing.T) {
	requests := make(chan struct{}, 2)
	statuses := []int{http.StatusBadGateway, http.StatusOK}
	barkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		status := statuses[0]
		statuses = statuses[1:]
		w.WriteHeader(status)
	}))
	defer barkServer.Close()

	notifier, err := newBarkNotifier(barkConfig{Endpoint: barkServer.URL + "/device-key", Events: eventConflict})
	if err != nil {
		t.Fatal(err)
	}
	defer notifier.Close()

	message := barkMessage{Title: "Game Save conflict", Body: "retry after failure", Key: "conflict\x00scope\x00device\x00head"}
	notifier.enqueue(message)
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed Bark request")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		notifier.mu.Lock()
		_, retained := notifier.seen[message.Key]
		notifier.mu.Unlock()
		if !retained || time.Now().After(deadline) {
			if retained {
				t.Fatal("failed Bark request was still marked as sent")
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	notifier.enqueue(message)
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for retry Bark request")
	}
}

func TestBarkFailureDoesNotBlockOpenSaveScopeUpload(t *testing.T) {
	requests := make(chan struct{}, 1)
	barkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer barkServer.Close()

	s, err := newServerWithOptions(t.TempDir(), testToken, 1024*1024, serverOptions{
		bark: barkConfig{Endpoint: barkServer.URL + "/device-key", Events: eventUpload},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	archive := []byte("odyssey-progress-1791")
	archiveName := "odyssey__main__snapshot-1.zip"
	if status := putOpenSaveScopeArchive(t, httpServer.URL, "switch", archiveName, archive); status != http.StatusCreated {
		t.Fatalf("archive upload status = %d, want 201", status)
	}
	candidate := openSaveScopeCandidateFor("snapshot-1", archiveName, "", archive, time.Now().UTC())
	response, body := putOpenSaveScopeCandidate(t, httpServer.URL,
		"/v1/opensave/scopes/switch/odyssey/snapshots/"+candidate.ID, "", candidate)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("scope upload status = %d: %s", response.StatusCode, body)
	}

	headRequest := openSaveScopeRequest(t, http.MethodGet,
		httpServer.URL+"/v1/opensave/scopes/switch/odyssey/head", nil)
	headResponse, err := http.DefaultClient.Do(headRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer headResponse.Body.Close()
	if headResponse.StatusCode != http.StatusOK {
		t.Fatalf("head status = %d, want 200", headResponse.StatusCode)
	}

	var head openSaveScopeSnapshot
	if err := json.NewDecoder(headResponse.Body).Decode(&head); err != nil {
		t.Fatal(err)
	}
	if head.ID != candidate.ID {
		t.Fatalf("head id = %q, want %q", head.ID, candidate.ID)
	}

	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed Bark notification")
	}
}

func TestBarkConfigurationValidation(t *testing.T) {
	for _, config := range []barkConfig{
		{Endpoint: "https://example.com", Events: eventConflict},
		{Endpoint: "https://example.com/device", Events: "unknown"},
		{Endpoint: "https://example.com/device", Events: eventConflict, TTL: -1},
	} {
		if notifier, err := newBarkNotifier(config); err == nil {
			if notifier != nil {
				notifier.Close()
			}
			t.Fatalf("config %#v unexpectedly succeeded", config)
		}
	}
}

func TestBarkAcceptsFullNotificationURL(t *testing.T) {
	requests := make(chan *url.URL, 1)
	barkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := *r.URL
		requests <- &clone
		w.WriteHeader(http.StatusOK)
	}))
	defer barkServer.Close()

	notifier, err := newBarkNotifier(barkConfig{
		Endpoint: barkServer.URL + "/device-key/example-title/example-body?group=example&ttl=600",
		Events:   eventConflict,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer notifier.Close()

	notifier.enqueue(barkMessage{Title: "Game Save conflict", Body: "current title", Key: "full-url"})
	select {
	case request := <-requests:
		if request.Path != "/device-key/Game Save conflict/current title" {
			t.Fatalf("Bark path = %q", request.Path)
		}
		if request.Query().Get("group") != "" || request.Query().Get("ttl") != "600" {
			t.Fatalf("Bark query = %q", request.RawQuery)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Bark notification")
	}
}

func TestBarkPathPartNeverCreatesExtraPathSegments(t *testing.T) {
	value := barkPathPart("A/B\nC")
	if value != "A-B C" || strings.Contains(value, "/") {
		t.Fatalf("barkPathPart = %q", value)
	}
}
