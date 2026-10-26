// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// openSaveScopeCandidate is supplied by an OpenSave client after it has
// uploaded an immutable archive. The server verifies that the archive bytes
// match ContentDigest before it may become a cloud head.
type openSaveScopeCandidate struct {
	ID            string    `json:"id"`
	Archive       string    `json:"archive"`
	Parent        string    `json:"parent"`
	ContentDigest string    `json:"contentDigest"`
	CreatedAt     time.Time `json:"createdAt"`
}

// openSaveScopeSnapshot is the persisted, server-audited form of a candidate.
// Device fields come from authenticated request headers and are informational;
// the bearer token remains the authorization boundary.
type openSaveScopeSnapshot struct {
	openSaveScopeCandidate
	Device     string    `json:"device,omitempty"`
	DeviceName string    `json:"deviceName,omitempty"`
	Platform   string    `json:"platform,omitempty"`
	ReceivedAt time.Time `json:"receivedAt"`
	Conflict   bool      `json:"conflict,omitempty"`
	ConflictOf string    `json:"conflictOf,omitempty"`
}

type openSaveScopeState struct {
	Head      string                           `json:"head"`
	Snapshots map[string]openSaveScopeSnapshot `json:"snapshots"`
}

// openSaveNativeState is deliberately independent from Eden's encrypted scope
// state. OpenSave archives are opaque ZIPs and are only related by the common
// authenticated service and device inventory.
type openSaveNativeState struct {
	Scopes map[string]openSaveScopeState `json:"scopes"`
}

type openSaveScopeResponse struct {
	Type      string                  `json:"type"`
	GameID    string                  `json:"gameId"`
	Head      *openSaveScopeSnapshot  `json:"head"`
	Snapshots []openSaveScopeSnapshot `json:"snapshots"`
	Conflicts []openSaveScopeSnapshot `json:"conflicts"`
}

// openSaveScopeAudit is the administrator-facing representation of one
// native OpenSave namespace. It deliberately exposes lineage and source
// fields, rather than presenting a timestamp as an authority decision.
type openSaveScopeAudit struct {
	Type           string                  `json:"type"`
	GameID         string                  `json:"gameId"`
	Head           *openSaveScopeSnapshot  `json:"head"`
	Snapshots      []openSaveScopeSnapshot `json:"snapshots"`
	Conflicts      []openSaveScopeSnapshot `json:"conflicts"`
	SnapshotCount  int                     `json:"snapshotCount"`
	ConflictCount  int                     `json:"conflictCount"`
	LastReceivedAt time.Time               `json:"lastReceivedAt,omitzero"`
	HeadDevice     string                  `json:"headDevice,omitempty"`
	HeadDeviceName string                  `json:"headDeviceName,omitempty"`
	HeadPlatform   string                  `json:"headPlatform,omitempty"`
}

func (state openSaveNativeState) clone() openSaveNativeState {
	next := openSaveNativeState{Scopes: make(map[string]openSaveScopeState, len(state.Scopes)+1)}
	for key, value := range state.Scopes {
		next.Scopes[key] = value
	}
	return next
}

func (state openSaveScopeState) clone() openSaveScopeState {
	next := openSaveScopeState{Head: state.Head, Snapshots: make(map[string]openSaveScopeSnapshot, len(state.Snapshots)+1)}
	for key, value := range state.Snapshots {
		next.Snapshots[key] = value
	}
	return next
}

func validOpenSaveGameID(value string) bool {
	return validIdentifier(value, 128)
}

func validOpenSaveContentDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func openSaveScopeKey(kind, gameID string) string {
	return kind + "\x00" + gameID
}

// openSaveEventScope identifies a typed OpenSave scope in the shared activity
// stream. Eden scope identifiers cannot contain ':' or '/', so this format is
// unambiguous and lets the dashboard link to the matching OpenSave view.
func openSaveEventScope(kind, gameID string) string {
	return "opensave:" + kind + "/" + gameID
}

func openSaveEventScopeName(value string) (string, bool) {
	const prefix = "opensave:"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	kind, gameID, ok := strings.Cut(strings.TrimPrefix(value, prefix), "/")
	if !ok || !validOpenSaveType(kind) || !validOpenSaveGameID(gameID) {
		return "", false
	}
	return "OpenSave " + kind + " / " + gameID, true
}

// recordOpenSaveScopeEvent runs after a native scope write has committed and
// released openSaveMu. Keeping the locks separate prevents activity and Bark
// persistence from becoming part of the archive/state critical section.
func (s *server) recordOpenSaveScopeEvent(eventKind, kind, gameID string, snapshot openSaveScopeSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordEventLocked(event{
		Kind:     eventKind,
		Scope:    openSaveEventScope(kind, gameID),
		Snapshot: snapshot.ID,
		Device:   snapshot.Device,
		Detail:   snapshot.Archive,
	})
}

// openSaveArchiveMatchesScope keeps a client from attaching one game's ZIP
// to another game's head. game-save still treats ZIP contents as opaque; this
// only verifies the stable archive-name contract shared by OpenSave clients.
func openSaveArchiveMatchesScope(gameID, archive string) bool {
	base := strings.TrimSuffix(archive, ".zip")
	parts := strings.SplitN(base, "__", 3)
	return len(parts) == 3 && parts[0] == gameID && parts[1] != "" && parts[2] != ""
}

func (s *server) openSaveStatePath() string {
	return filepath.Join(s.dataDir, "opensave-native-state.json")
}

func (s *server) loadOpenSaveState() error {
	if err := readJSONFile(s.openSaveStatePath(), &s.openSaveState); err != nil {
		return err
	}
	if s.openSaveState.Scopes == nil {
		s.openSaveState.Scopes = make(map[string]openSaveScopeState)
	}
	for key, scope := range s.openSaveState.Scopes {
		if scope.Snapshots == nil {
			scope.Snapshots = make(map[string]openSaveScopeSnapshot)
			s.openSaveState.Scopes[key] = scope
		}
	}
	return nil
}

func (s *server) persistOpenSaveState(state openSaveNativeState) error {
	return writeJSONFile(s.openSaveStatePath(), state)
}

func openSaveScopeFromRequest(w http.ResponseWriter, r *http.Request) (string, string, string, bool) {
	kind, gameID := r.PathValue("type"), r.PathValue("game")
	if !validOpenSaveType(kind) || !validOpenSaveGameID(gameID) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave type or game id")
		return "", "", "", false
	}
	return kind, gameID, openSaveScopeKey(kind, gameID), true
}

func decodeOpenSaveScopeCandidate(w http.ResponseWriter, r *http.Request, snapshotID string) (openSaveScopeCandidate, bool) {
	var candidate openSaveScopeCandidate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		writeError(w, http.StatusBadRequest, "invalid OpenSave snapshot metadata")
		return openSaveScopeCandidate{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave snapshot metadata")
		return openSaveScopeCandidate{}, false
	}
	if candidate.ID != snapshotID || !validSnapshotID(candidate.ID) || !validOpenSaveName(candidate.Archive) ||
		(candidate.Parent != "" && !validSnapshotID(candidate.Parent)) ||
		!validOpenSaveContentDigest(candidate.ContentDigest) || candidate.CreatedAt.IsZero() {
		writeError(w, http.StatusBadRequest, "invalid OpenSave snapshot fields")
		return openSaveScopeCandidate{}, false
	}
	candidate.CreatedAt = candidate.CreatedAt.UTC()
	return candidate, true
}

func openSaveCandidateMatchesSnapshot(candidate openSaveScopeCandidate, stored openSaveScopeSnapshot) bool {
	return candidate.ID == stored.ID && candidate.Archive == stored.Archive &&
		candidate.Parent == stored.Parent && candidate.ContentDigest == stored.ContentDigest &&
		candidate.CreatedAt.Equal(stored.CreatedAt)
}

// expectedOpenSaveHead parses a strong ETag in the same form returned by the
// head endpoint. An empty quoted tag ("") is the only valid precondition for
// creating a scope's first head.
func expectedOpenSaveHead(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", false
	}
	value = value[1 : len(value)-1]
	return value, value == "" || validSnapshotID(value)
}

func (s *server) openSaveCandidateArchiveMatchesLocked(kind string, candidate openSaveScopeCandidate) (int, string, bool) {
	digest, err := s.openSaveArchiveDigestLocked(kind, candidate.Archive)
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusFailedDependency, "OpenSave archive is missing", false
	}
	if err != nil {
		return http.StatusInternalServerError, "cannot inspect OpenSave archive", false
	}
	if digest != candidate.ContentDigest {
		return http.StatusUnprocessableEntity, "OpenSave archive digest does not match snapshot", false
	}
	return 0, "", true
}

func openSaveSnapshotFromRequest(candidate openSaveScopeCandidate, r *http.Request, now time.Time) openSaveScopeSnapshot {
	return openSaveScopeSnapshot{
		openSaveScopeCandidate: candidate,
		Device:                 requestDeviceID(r),
		DeviceName:             trimDeviceField(requestClientHeader(r, "X-Eden-Device-Name", "X-OpenSave-Device-Name"), 80),
		Platform:               trimDeviceField(requestClientHeader(r, "X-Eden-Platform", "X-OpenSave-Platform"), 32),
		ReceivedAt:             now.UTC(),
	}
}

func writeOpenSaveHeadConflict(w http.ResponseWriter, message, head string) {
	writeJSON(w, http.StatusConflict, map[string]string{"error": message, "head": head})
}

func (s *server) handleOpenSaveScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}

	response := openSaveScopeResponse{
		Type:      kind,
		GameID:    gameID,
		Snapshots: []openSaveScopeSnapshot{},
		Conflicts: []openSaveScopeSnapshot{},
	}
	s.openSaveMu.Lock()
	stored, exists := s.openSaveState.Scopes[key]
	if exists {
		for _, item := range stored.Snapshots {
			response.Snapshots = append(response.Snapshots, item)
			if item.Conflict {
				response.Conflicts = append(response.Conflicts, item)
			}
		}
		if stored.Head != "" {
			if item, found := stored.Snapshots[stored.Head]; found {
				itemCopy := item
				response.Head = &itemCopy
			}
		}
	}
	s.openSaveMu.Unlock()
	sort.Slice(response.Snapshots, func(i, j int) bool {
		if !response.Snapshots[i].ReceivedAt.Equal(response.Snapshots[j].ReceivedAt) {
			return response.Snapshots[i].ReceivedAt.After(response.Snapshots[j].ReceivedAt)
		}
		return response.Snapshots[i].ID > response.Snapshots[j].ID
	})
	sort.Slice(response.Conflicts, func(i, j int) bool {
		if !response.Conflicts[i].ReceivedAt.Equal(response.Conflicts[j].ReceivedAt) {
			return response.Conflicts[i].ReceivedAt.After(response.Conflicts[j].ReceivedAt)
		}
		return response.Conflicts[i].ID > response.Conflicts[j].ID
	})
	writeJSON(w, http.StatusOK, response)
}

func (s *server) listOpenSaveScopeAudits() []openSaveScopeAudit {
	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	audits := make([]openSaveScopeAudit, 0, len(s.openSaveState.Scopes))
	for key, stored := range s.openSaveState.Scopes {
		kind, gameID, ok := strings.Cut(key, "\x00")
		if !ok || !validOpenSaveType(kind) || !validOpenSaveGameID(gameID) {
			continue // an invalid persisted key is not a safe dashboard entry
		}
		audits = append(audits, s.openSaveScopeAuditLocked(kind, gameID, stored))
	}
	sort.Slice(audits, func(i, j int) bool {
		if !audits[i].LastReceivedAt.Equal(audits[j].LastReceivedAt) {
			return audits[i].LastReceivedAt.After(audits[j].LastReceivedAt)
		}
		if audits[i].Type != audits[j].Type {
			return audits[i].Type < audits[j].Type
		}
		return audits[i].GameID < audits[j].GameID
	})
	return audits
}

// openSaveScopeAuditLocked must be called while openSaveMu is held.
func (s *server) openSaveScopeAuditLocked(kind, gameID string, stored openSaveScopeState) openSaveScopeAudit {
	audit := openSaveScopeAudit{
		Type:      kind,
		GameID:    gameID,
		Snapshots: []openSaveScopeSnapshot{},
		Conflicts: []openSaveScopeSnapshot{},
	}
	for _, item := range stored.Snapshots {
		audit.SnapshotCount++
		audit.Snapshots = append(audit.Snapshots, item)
		if item.Conflict {
			audit.ConflictCount++
			audit.Conflicts = append(audit.Conflicts, item)
		}
		if item.ReceivedAt.After(audit.LastReceivedAt) {
			audit.LastReceivedAt = item.ReceivedAt
		}
	}
	if stored.Head != "" {
		if item, found := stored.Snapshots[stored.Head]; found {
			itemCopy := item
			audit.Head = &itemCopy
			audit.HeadDevice = item.Device
			audit.HeadDeviceName = item.DeviceName
			audit.HeadPlatform = item.Platform
		}
	}
	sort.Slice(audit.Snapshots, func(i, j int) bool {
		if !audit.Snapshots[i].ReceivedAt.Equal(audit.Snapshots[j].ReceivedAt) {
			return audit.Snapshots[i].ReceivedAt.After(audit.Snapshots[j].ReceivedAt)
		}
		return audit.Snapshots[i].ID > audit.Snapshots[j].ID
	})
	sort.Slice(audit.Conflicts, func(i, j int) bool {
		if !audit.Conflicts[i].ReceivedAt.Equal(audit.Conflicts[j].ReceivedAt) {
			return audit.Conflicts[i].ReceivedAt.After(audit.Conflicts[j].ReceivedAt)
		}
		return audit.Conflicts[i].ID > audit.Conflicts[j].ID
	})
	return audit
}

func (s *server) handleOpenSaveScopeHead(w http.ResponseWriter, r *http.Request) {
	_, _, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	s.openSaveMu.Lock()
	stored, exists := s.openSaveState.Scopes[key]
	if !exists || stored.Head == "" {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope has no current snapshot")
		return
	}
	head, exists := stored.Snapshots[stored.Head]
	s.openSaveMu.Unlock()
	if !exists {
		writeError(w, http.StatusInternalServerError, "OpenSave scope state is invalid")
		return
	}
	w.Header().Set("ETag", `"`+head.ID+`"`)
	writeJSON(w, http.StatusOK, head)
}

func (s *server) handlePutOpenSaveScopeSnapshot(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave snapshot id")
		return
	}
	candidate, ok := decodeOpenSaveScopeCandidate(w, r, snapshotID)
	if !ok {
		return
	}
	if !openSaveArchiveMatchesScope(gameID, candidate.Archive) {
		writeError(w, http.StatusBadRequest, "OpenSave archive does not belong to this game scope")
		return
	}
	expectedHead, ok := expectedOpenSaveHead(r)
	if !ok {
		writeError(w, http.StatusPreconditionRequired, "If-Match must contain the confirmed OpenSave head")
		return
	}

	s.openSaveMu.Lock()
	if status, message, matches := s.openSaveCandidateArchiveMatchesLocked(kind, candidate); !matches {
		s.openSaveMu.Unlock()
		writeError(w, status, message)
		return
	}
	stored := s.openSaveState.Scopes[key]
	if existing, exists := stored.Snapshots[snapshotID]; exists {
		s.openSaveMu.Unlock()
		if openSaveCandidateMatchesSnapshot(candidate, existing) && !existing.Conflict && stored.Head == snapshotID {
			w.Header().Set("ETag", `"`+existing.ID+`"`)
			writeJSON(w, http.StatusOK, existing)
			return
		}
		writeOpenSaveHeadConflict(w, "OpenSave snapshot id already exists", stored.Head)
		return
	}
	if stored.Head != expectedHead || candidate.Parent != stored.Head {
		s.openSaveMu.Unlock()
		writeOpenSaveHeadConflict(w, "OpenSave scope head changed", stored.Head)
		return
	}

	nextScope := stored.clone()
	recorded := openSaveSnapshotFromRequest(candidate, r, time.Now())
	nextScope.Head = snapshotID
	nextScope.Snapshots[snapshotID] = recorded
	nextState := s.openSaveState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistOpenSaveState(nextState); err != nil {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave scope state")
		return
	}
	s.openSaveState = nextState
	s.openSaveMu.Unlock()
	s.recordOpenSaveScopeEvent(eventUpload, kind, gameID, recorded)
	w.Header().Set("ETag", `"`+recorded.ID+`"`)
	writeJSON(w, http.StatusCreated, recorded)
}

// handlePutOpenSaveScopeConflict retains a divergent archive after a CAS
// failure. Conflict snapshots never advance Head and are never auto-pruned.
func (s *server) handlePutOpenSaveScopeConflict(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave snapshot id")
		return
	}
	candidate, ok := decodeOpenSaveScopeCandidate(w, r, snapshotID)
	if !ok {
		return
	}
	if !openSaveArchiveMatchesScope(gameID, candidate.Archive) {
		writeError(w, http.StatusBadRequest, "OpenSave archive does not belong to this game scope")
		return
	}

	s.openSaveMu.Lock()
	if status, message, matches := s.openSaveCandidateArchiveMatchesLocked(kind, candidate); !matches {
		s.openSaveMu.Unlock()
		writeError(w, status, message)
		return
	}
	stored, exists := s.openSaveState.Scopes[key]
	if !exists || stored.Head == "" {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope has no current snapshot")
		return
	}
	if existing, exists := stored.Snapshots[snapshotID]; exists {
		s.openSaveMu.Unlock()
		if openSaveCandidateMatchesSnapshot(candidate, existing) && existing.Conflict {
			writeJSON(w, http.StatusOK, existing)
			return
		}
		writeOpenSaveHeadConflict(w, "OpenSave snapshot id already exists", stored.Head)
		return
	}
	if candidate.Parent == stored.Head {
		s.openSaveMu.Unlock()
		writeOpenSaveHeadConflict(w, "OpenSave snapshot still matches the current head", stored.Head)
		return
	}

	nextScope := stored.clone()
	recorded := openSaveSnapshotFromRequest(candidate, r, time.Now())
	recorded.Conflict = true
	recorded.ConflictOf = stored.Head
	nextScope.Snapshots[snapshotID] = recorded
	nextState := s.openSaveState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistOpenSaveState(nextState); err != nil {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave conflict snapshot")
		return
	}
	s.openSaveState = nextState
	s.openSaveMu.Unlock()
	s.recordOpenSaveScopeEvent(eventConflict, kind, gameID, recorded)
	writeJSON(w, http.StatusCreated, recorded)
}

// openSaveArchiveReferencedLocked prevents deleting a head, conflict, or
// historical recovery archive that is still named by persisted scope state.
func (s *server) openSaveArchiveReferencedLocked(kind, name string) bool {
	prefix := kind + "\x00"
	for key, scope := range s.openSaveState.Scopes {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		for _, snapshot := range scope.Snapshots {
			if snapshot.Archive == name {
				return true
			}
		}
	}
	return false
}
