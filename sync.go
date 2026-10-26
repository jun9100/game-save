// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Handlers for the /v1 API used by Eden clients. The request and response shapes here are part of
// the client protocol and must remain compatible with released Eden builds.

const (
	edenSaveSyncProtocolHeader    = "X-Eden-Save-Sync-Protocol"
	minimumEdenSaveSyncProtocol   = 2
	edenSaveSyncMinimumVersionKey = "X-Eden-Save-Sync-Min-Version"
)

// requireCurrentEdenSaveSyncProtocol blocks pre-baseline clients from writing. Older clients only
// track a remembered head and can otherwise advance it with stale local files after an interrupted
// session. Reads remain available so an upgraded client can recover normally.
func requireCurrentEdenSaveSyncProtocol(w http.ResponseWriter, r *http.Request) bool {
	version, err := strconv.Atoi(strings.TrimSpace(r.Header.Get(edenSaveSyncProtocolHeader)))
	if err == nil && version >= minimumEdenSaveSyncProtocol {
		return true
	}
	w.Header().Set(edenSaveSyncMinimumVersionKey, strconv.Itoa(minimumEdenSaveSyncProtocol))
	writeError(w, http.StatusUpgradeRequired,
		"Eden Save Sync client protocol version 2 or newer is required for writes")
	return false
}

func (s *server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenAuthorized(r) {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		s.recordDevice(r)
		next(w, r)
	}
}

// requireTokenReadOnly authenticates a request without recording a device heartbeat. Status
// inspection must not create activity records, mutate a scope, or make a failed read look like a
// successful synchronization attempt.
func (s *server) requireTokenReadOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenAuthorized(r) {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

func (s *server) tokenAuthorized(r *http.Request) bool {
	const prefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	return strings.HasPrefix(authorization, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authorization, prefix)), []byte(s.token)) == 1
}

func requestDeviceID(r *http.Request) string {
	deviceID := requestClientHeader(r, "X-Eden-Device-ID", "X-OpenSave-Device-ID")
	if !validIdentifier(deviceID, 128) {
		return ""
	}
	return deviceID
}

// requestClientHeader accepts the Eden headers used by released clients and
// the OpenSave-specific equivalents used by the native archive API. Device
// identity is informational only; the bearer token remains the credential.
func requestClientHeader(r *http.Request, edenHeader, openSaveHeader string) string {
	if value := strings.TrimSpace(r.Header.Get(openSaveHeader)); value != "" {
		return value
	}
	return strings.TrimSpace(r.Header.Get(edenHeader))
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *server) recordDevice(r *http.Request) {
	deviceID := requestDeviceID(r)
	if deviceID == "" {
		return
	}
	now := time.Now().UTC()
	device := deviceInfo{
		ID:        deviceID,
		Name:      trimDeviceField(requestClientHeader(r, "X-Eden-Device-Name", "X-OpenSave-Device-Name"), 80),
		Platform:  trimDeviceField(requestClientHeader(r, "X-Eden-Platform", "X-OpenSave-Platform"), 32),
		Version:   trimDeviceField(requestClientHeader(r, "X-Eden-Version", "X-OpenSave-Version"), 64),
		Address:   remoteHost(r),
		FirstSeen: now,
		LastSeen:  now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, known := s.dashboard.Devices[deviceID]
	if known {
		if device.Name == previous.Name && device.Platform == previous.Platform &&
			device.Version == previous.Version && device.Address == previous.Address &&
			now.Sub(previous.LastSeen) < 30*time.Second {
			return
		}
		device.Alias = previous.Alias
		if !previous.FirstSeen.IsZero() {
			device.FirstSeen = previous.FirstSeen
		}
	}
	next := s.dashboard.clone()
	next.Devices[deviceID] = device
	if err := s.persistDashboard(next); err != nil {
		slog.Warn("record device", "device", deviceID, "error", err)
		return
	}
	s.dashboard = next
	if !known {
		s.recordEventLocked(event{Kind: eventDeviceNew, Device: deviceID,
			Detail: strings.TrimSpace(device.Name + " " + device.Platform)})
	}
}

func trimDeviceField(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > maximum {
		return string([]rune(value)[:maximum])
	}
	return value
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

func (s *server) handleDeviceHeartbeat(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handlePutBlob(w http.ResponseWriter, r *http.Request) {
	if !requireCurrentEdenSaveSyncProtocol(w, r) {
		return
	}
	id := r.PathValue("id")
	if !validObjectID(id) {
		writeError(w, http.StatusBadRequest, "invalid blob id")
		return
	}

	destination := s.objectPath(id)
	if _, err := os.Stat(destination); err == nil {
		// Refresh the timestamp so a concurrent garbage collection treats it as freshly uploaded.
		now := time.Now()
		_ = os.Chtimes(destination, now, now)
		w.WriteHeader(http.StatusNoContent)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "cannot inspect blob")
		return
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create object directory")
		return
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create temporary object")
		return
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	hash := sha256.New()
	bytesWritten, copyErr := io.Copy(io.MultiWriter(temporary, hash), http.MaxBytesReader(w, r.Body, s.maxObjectBytes+1))
	if copyErr == nil {
		copyErr = temporary.Sync()
	}
	if closeErr := temporary.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(copyErr, &maxBytesError) || bytesWritten > s.maxObjectBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "blob exceeds configured size limit")
		} else {
			writeError(w, http.StatusBadRequest, "cannot read blob")
		}
		return
	}
	if bytesWritten > s.maxObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "blob exceeds configured size limit")
		return
	}
	if hex.EncodeToString(hash.Sum(nil)) != id {
		writeError(w, http.StatusUnprocessableEntity, "blob hash does not match id")
		return
	}

	if err := os.Rename(temporaryPath, destination); err != nil {
		if _, statErr := os.Stat(destination); statErr == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot publish blob")
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *server) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validObjectID(id) {
		writeError(w, http.StatusBadRequest, "invalid blob id")
		return
	}
	s.serveObject(w, r, id, "")
}

// serveObject streams an encrypted object. When attachmentName is set the response is offered
// as a download.
func (s *server) serveObject(w http.ResponseWriter, r *http.Request, id, attachmentName string) {
	file, err := os.Open(s.objectPath(id))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot open blob")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot open blob")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+id+`"`)
	if attachmentName != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+attachmentName+`"`)
	}
	// Objects are content addressed, so ranges and conditional requests are always safe.
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func (s *server) handleGetHead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validScopeID(id) {
		writeError(w, http.StatusBadRequest, "invalid scope id")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	storedScope, ok := s.state.Scopes[id]
	if !ok || storedScope.Head == "" {
		writeError(w, http.StatusNotFound, "scope has no snapshots")
		return
	}
	writeJSON(w, http.StatusOK, storedScope.Snapshots[storedScope.Head].snapshot)
}

// scopeStatusDevice deliberately excludes the recorded network address and client version. The
// client only needs an identifiable source device for conflict resolution, not administrative
// inventory details.
type scopeStatusDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Platform string `json:"platform,omitempty"`
}

// scopeStatusSnapshot is the read-only, audit-friendly representation of an Eden snapshot. The
// server-selected head is authoritative because it was advanced with CAS; created_at is retained
// for display only and must not be used to select a winner.
type scopeStatusSnapshot struct {
	ID         string             `json:"id"`
	Blob       string             `json:"blob"`
	Parent     string             `json:"parent"`
	CreatedAt  time.Time          `json:"created_at"`
	ReceivedAt time.Time          `json:"received_at,omitzero"`
	Size       int64              `json:"size"`
	Device     *scopeStatusDevice `json:"device,omitempty"`
	Conflict   bool               `json:"conflict,omitempty"`
	ConflictOf string             `json:"conflict_of,omitempty"`
	RestoredOf string             `json:"restored_of,omitempty"`
}

type scopeStatusResponse struct {
	Scope             string                `json:"scope"`
	Title             string                `json:"title,omitempty"`
	UserName          string                `json:"user_name,omitempty"`
	Head              *scopeStatusSnapshot  `json:"head"`
	Snapshots         []scopeStatusSnapshot `json:"snapshots"`
	Conflicts         []scopeStatusSnapshot `json:"conflicts"`
	SnapshotCount     int                   `json:"snapshot_count"`
	ConflictCount     int                   `json:"conflict_count"`
	HeadObjectPresent bool                  `json:"head_object_present"`
}

func scopeStatusDeviceFor(deviceID string, devices map[string]deviceInfo) *scopeStatusDevice {
	if deviceID == "" {
		return nil
	}
	device := devices[deviceID]
	name := device.Name
	if device.Alias != "" {
		name = device.Alias
	}
	return &scopeStatusDevice{ID: deviceID, Name: name, Platform: device.Platform}
}

func scopeStatusSnapshotFor(record snapshotRecord, devices map[string]deviceInfo) scopeStatusSnapshot {
	return scopeStatusSnapshot{
		ID:         record.ID,
		Blob:       record.Blob,
		Parent:     record.Parent,
		CreatedAt:  record.CreatedAt,
		ReceivedAt: record.ReceivedAt,
		Size:       record.Size,
		Device:     scopeStatusDeviceFor(record.Device, devices),
		Conflict:   record.Conflict,
		ConflictOf: record.ConflictOf,
		RestoredOf: record.RestoredOf,
	}
}

func sortScopeStatusSnapshots(snapshots []scopeStatusSnapshot) {
	sort.Slice(snapshots, func(left, right int) bool {
		if !snapshots[left].ReceivedAt.Equal(snapshots[right].ReceivedAt) {
			return snapshots[left].ReceivedAt.After(snapshots[right].ReceivedAt)
		}
		return snapshots[left].ID > snapshots[right].ID
	})
}

// handleGetScopeStatus returns all recoverable versions and retained conflicts for one Eden
// scope. Unknown scopes intentionally return a successful empty state so a new client can detect
// support without mistaking its first backup for a legacy-server failure.
func (s *server) handleGetScopeStatus(w http.ResponseWriter, r *http.Request) {
	scopeID := r.PathValue("id")
	if !validScopeID(scopeID) {
		writeError(w, http.StatusBadRequest, "invalid scope id")
		return
	}

	response := scopeStatusResponse{
		Scope:     scopeID,
		Snapshots: []scopeStatusSnapshot{},
		Conflicts: []scopeStatusSnapshot{},
	}
	var headBlob string
	s.mu.Lock()
	if stored, exists := s.state.Scopes[scopeID]; exists {
		metadata := s.dashboard.Scopes[scopeID]
		response.Title = metadata.Title
		response.UserName = metadata.UserName
		for _, record := range stored.Snapshots {
			item := scopeStatusSnapshotFor(record, s.dashboard.Devices)
			response.Snapshots = append(response.Snapshots, item)
			response.SnapshotCount++
			if item.Conflict {
				response.Conflicts = append(response.Conflicts, item)
				response.ConflictCount++
			}
		}
		if stored.Head != "" {
			if record, found := stored.Snapshots[stored.Head]; found {
				head := scopeStatusSnapshotFor(record, s.dashboard.Devices)
				response.Head = &head
				headBlob = head.Blob
			}
		}
	}
	s.mu.Unlock()

	sortScopeStatusSnapshots(response.Snapshots)
	sortScopeStatusSnapshots(response.Conflicts)
	if headBlob != "" {
		if info, err := os.Stat(s.objectPath(headBlob)); err == nil && !info.IsDir() {
			response.HeadObjectPresent = true
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func decodeSnapshotCandidate(w http.ResponseWriter, r *http.Request, snapshotID string) (snapshot, bool) {
	var candidate snapshot
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot metadata")
		return snapshot{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid snapshot metadata")
		return snapshot{}, false
	}
	if candidate.ID != snapshotID || !validObjectID(candidate.Blob) ||
		(candidate.Parent != "" && !validSnapshotID(candidate.Parent)) || candidate.CreatedAt.IsZero() {
		writeError(w, http.StatusBadRequest, "invalid snapshot fields")
		return snapshot{}, false
	}
	return candidate, true
}

func (s *server) snapshotBlobSize(w http.ResponseWriter, candidate snapshot) (int64, bool) {
	info, err := os.Stat(s.objectPath(candidate.Blob))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusFailedDependency, "snapshot blob is missing")
		} else {
			writeError(w, http.StatusInternalServerError, "cannot inspect snapshot blob")
		}
		return 0, false
	}
	return info.Size(), true
}

func (s *server) handlePutSnapshot(w http.ResponseWriter, r *http.Request) {
	if !requireCurrentEdenSaveSyncProtocol(w, r) {
		return
	}
	scopeID := r.PathValue("id")
	snapshotID := r.PathValue("snapshot")
	if !validScopeID(scopeID) || !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid scope or snapshot id")
		return
	}
	candidate, ok := decodeSnapshotCandidate(w, r, snapshotID)
	if !ok {
		return
	}
	blobSize, ok := s.snapshotBlobSize(w, candidate)
	if !ok {
		return
	}

	deviceID := requestDeviceID(r)
	expectedHead := strings.Trim(r.Header.Get("If-Match"), "\"")
	s.mu.Lock()
	defer s.mu.Unlock()
	storedScope := s.state.Scopes[scopeID]
	if existing, ok := storedScope.Snapshots[snapshotID]; ok {
		if existing.snapshot == candidate && storedScope.Head == snapshotID {
			writeJSON(w, http.StatusOK, existing.snapshot)
			return
		}
		writeError(w, http.StatusConflict, "snapshot id already exists")
		return
	}
	if storedScope.Head != expectedHead || candidate.Parent != storedScope.Head {
		s.recordEventLocked(event{Kind: eventConflict, Scope: scopeID, Snapshot: snapshotID,
			Device: deviceID, Detail: "head " + storedScope.Head})
		writeJSON(w, http.StatusConflict, map[string]string{"head": storedScope.Head})
		return
	}

	nextScope := storedScope.clone()
	nextScope.Head = snapshotID
	nextScope.Snapshots[snapshotID] = snapshotRecord{
		snapshot:   candidate,
		Size:       blobSize,
		Device:     deviceID,
		ReceivedAt: time.Now().UTC(),
	}
	nextState := s.state.clone()
	nextState.Scopes[scopeID] = nextScope
	if err := s.persistState(nextState); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist snapshot metadata")
		return
	}
	s.state = nextState
	s.recordEventLocked(event{Kind: eventUpload, Scope: scopeID, Snapshot: snapshotID,
		Device: deviceID, Detail: formatBytes(blobSize)})
	if s.settings.Retention.enabled() {
		if removed, err := s.pruneScopeLocked(scopeID, time.Now().UTC()); err != nil {
			slog.Warn("apply retention", "scope", scopeID, "error", err)
		} else if removed > 0 {
			s.recordEventLocked(event{Kind: eventPrune, Scope: scopeID,
				Detail: "removed " + itoa(removed) + " snapshots"})
		}
	}
	writeJSON(w, http.StatusCreated, candidate)
}

// handlePutConflictSnapshot persists a rejected client branch without moving the scope head. The
// record is pinned until an administrator explicitly deletes it, so a retention policy cannot erase
// the only copy of progress that needs review.
func (s *server) handlePutConflictSnapshot(w http.ResponseWriter, r *http.Request) {
	if !requireCurrentEdenSaveSyncProtocol(w, r) {
		return
	}
	scopeID, snapshotID := r.PathValue("id"), r.PathValue("snapshot")
	if !validScopeID(scopeID) || !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid scope or snapshot id")
		return
	}
	candidate, ok := decodeSnapshotCandidate(w, r, snapshotID)
	if !ok {
		return
	}
	blobSize, ok := s.snapshotBlobSize(w, candidate)
	if !ok {
		return
	}

	deviceID := requestDeviceID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	storedScope, exists := s.state.Scopes[scopeID]
	if !exists || storedScope.Head == "" {
		writeError(w, http.StatusNotFound, "scope has no current snapshot")
		return
	}
	if existing, exists := storedScope.Snapshots[snapshotID]; exists {
		if existing.snapshot == candidate && existing.Conflict {
			writeJSON(w, http.StatusOK, existing.snapshot)
			return
		}
		writeError(w, http.StatusConflict, "snapshot id already exists")
		return
	}
	if candidate.Parent == storedScope.Head {
		writeError(w, http.StatusConflict, "snapshot still matches the current head")
		return
	}

	now := time.Now().UTC()
	nextScope := storedScope.clone()
	nextScope.Snapshots[snapshotID] = snapshotRecord{
		snapshot:   candidate,
		Size:       blobSize,
		Device:     deviceID,
		ReceivedAt: now,
		Pinned:     true,
		Conflict:   true,
		ConflictOf: storedScope.Head,
	}
	nextState := s.state.clone()
	nextState.Scopes[scopeID] = nextScope
	if err := s.persistState(nextState); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist conflict snapshot")
		return
	}
	s.state = nextState
	s.recordEventLocked(event{Kind: eventConflict, Scope: scopeID, Snapshot: snapshotID,
		Device: deviceID, Detail: "retained against " + storedScope.Head})
	writeJSON(w, http.StatusCreated, candidate)
}

func (s *server) handlePutScopeMetadata(w http.ResponseWriter, r *http.Request) {
	scopeID := r.PathValue("id")
	if !validScopeID(scopeID) {
		writeError(w, http.StatusBadRequest, "invalid scope")
		return
	}
	var metadata struct {
		Title      string `json:"title"`
		LogoURL    string `json:"logo_url"`
		LogoData   string `json:"logo_data"`
		UserName   string `json:"user_name"`
		Descriptor string `json:"descriptor"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		writeError(w, http.StatusBadRequest, "invalid metadata")
		return
	}
	metadata.UserName = strings.TrimSpace(metadata.UserName)
	if len([]rune(metadata.Title)) > 120 || !validLogoData(metadata.LogoData) ||
		len([]rune(metadata.UserName)) > maxUserNameRunes || !validDescriptor(metadata.Descriptor) {
		writeError(w, http.StatusBadRequest, "invalid metadata")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.dashboard.Scopes[scopeID]
	updated := current
	if metadata.Title != "" {
		updated.Title = metadata.Title
	}
	if metadata.LogoData != "" {
		updated.LogoData = metadata.LogoData
		updated.LogoURL = ""
	}
	if metadata.UserName != "" {
		updated.UserName = metadata.UserName
	}
	if metadata.Descriptor != "" {
		updated.Descriptor = metadata.Descriptor
	}
	if updated == current {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	next := s.dashboard.clone()
	next.Scopes[scopeID] = updated
	if err := s.persistDashboard(next); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist metadata")
		return
	}
	s.dashboard = next
	w.WriteHeader(http.StatusNoContent)
}

func validDescriptor(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > maxDescriptorBytes {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	return err == nil && len(decoded) > 0
}

// catalogEntry describes one scope to clients choosing which cloud saves to use. Clients decrypt
// the descriptor to learn the owning user and game; scopes written by other sync keys stay opaque.
type catalogEntry struct {
	ID         string    `json:"id"`
	Head       string    `json:"head"`
	UpdatedAt  time.Time `json:"updated_at"`
	Size       int64     `json:"size"`
	Title      string    `json:"title,omitempty"`
	UserName   string    `json:"user_name,omitempty"`
	Descriptor string    `json:"descriptor,omitempty"`
}

func (s *server) handleListScopes(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	entries := make([]catalogEntry, 0, len(s.state.Scopes))
	for scopeID, stored := range s.state.Scopes {
		head, ok := stored.Snapshots[stored.Head]
		if !ok {
			continue
		}
		metadata := s.dashboard.Scopes[scopeID]
		entries = append(entries, catalogEntry{
			ID:         scopeID,
			Head:       stored.Head,
			UpdatedAt:  head.CreatedAt,
			Size:       head.Size,
			Title:      metadata.Title,
			UserName:   metadata.UserName,
			Descriptor: metadata.Descriptor,
		})
	}
	s.mu.Unlock()
	sort.Slice(entries, func(left, right int) bool { return entries[left].ID < entries[right].ID })
	writeJSON(w, http.StatusOK, map[string]any{"scopes": entries})
}
