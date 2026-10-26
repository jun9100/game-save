// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// OpenSave archives are ordinary ZIP files, unlike Eden's encrypted blobs.
// Keep native archives out of both objects/ and the legacy WebDAV collection so
// retention, garbage collection, and older clients cannot mistake one for another.
type openSaveFile struct {
	Type        string    `json:"type"`
	Name        string    `json:"name"`
	SizeBytes   int64     `json:"sizeBytes"`
	CreatedTime time.Time `json:"createdTime"`
	SHA256      string    `json:"sha256,omitempty"`
	Device      string    `json:"device,omitempty"`
	DeviceName  string    `json:"deviceName,omitempty"`
	UploadedAt  time.Time `json:"uploadedAt,omitzero"`
}

// openSaveCatalog records only transport-level metadata for native OpenSave
// archives. Save contents remain opaque to game-save; the checksum lets an
// administrator audit which exact archive a client uploaded without opening it.
type openSaveCatalog struct {
	Files map[string]openSaveFileMetadata `json:"files"`
}

type openSaveFileMetadata struct {
	SHA256     string    `json:"sha256,omitempty"`
	Device     string    `json:"device,omitempty"`
	DeviceName string    `json:"device_name,omitempty"`
	UploadedAt time.Time `json:"uploaded_at,omitzero"`
}

func (c openSaveCatalog) clone() openSaveCatalog {
	next := openSaveCatalog{Files: make(map[string]openSaveFileMetadata, len(c.Files)+1)}
	for key, value := range c.Files {
		next.Files[key] = value
	}
	return next
}

func validOpenSaveType(value string) bool {
	if value == "pc" || value == "switch" {
		return true
	}
	return strings.HasPrefix(value, "emulator-") && validIdentifier(value, 64)
}

func validOpenSaveName(value string) bool {
	return validIdentifier(value, 255) && strings.HasSuffix(strings.ToLower(value), ".zip")
}

func (s *server) openSaveNativeDirectory() string {
	return filepath.Join(s.dataDir, "opensave-native")
}

func (s *server) openSaveCatalogPath() string {
	return filepath.Join(s.dataDir, "opensave-native.json")
}

func openSaveCatalogKey(kind, name string) string {
	return kind + "\x00" + name
}

func (s *server) loadOpenSaveCatalog() error {
	if err := readJSONFile(s.openSaveCatalogPath(), &s.openSaveCatalog); err != nil {
		return err
	}
	if s.openSaveCatalog.Files == nil {
		s.openSaveCatalog.Files = make(map[string]openSaveFileMetadata)
	}
	return nil
}

func (s *server) persistOpenSaveCatalog(catalog openSaveCatalog) error {
	return writeJSONFile(s.openSaveCatalogPath(), catalog)
}

func (s *server) openSavePath(kind, name string) (string, bool) {
	if !validOpenSaveType(kind) || !validOpenSaveName(name) {
		return "", false
	}
	return filepath.Join(s.openSaveNativeDirectory(), kind, name), true
}

// openSaveArchiveDigestLocked returns the digest of the bytes currently stored
// on disk. The caller holds openSaveMu, so a successful comparison remains
// true until it has finished the associated state transition.
func (s *server) openSaveArchiveDigestLocked(kind, name string) (string, error) {
	path, ok := s.openSavePath(kind, name)
	if !ok {
		return "", errors.New("invalid OpenSave archive path")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *server) handleOpenSaveList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"files": s.listOpenSaveFiles()})
}

// listOpenSaveFiles returns only files currently present on disk. A manually
// removed archive therefore never remains visible because of stale catalog
// metadata, while older archives pre-dating the catalog remain listable.
func (s *server) listOpenSaveFiles() []openSaveFile {
	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	return s.listOpenSaveFilesLocked()
}

func (s *server) listOpenSaveFilesLocked() []openSaveFile {
	root := s.openSaveNativeDirectory()
	entries := []openSaveFile{}
	for _, kind := range []string{"pc", "switch"} {
		entries = append(entries, s.openSaveFilesForType(root, kind)...)
	}
	// Emulator types are extensible. Only immediate directories are accepted.
	if dirs, err := os.ReadDir(root); err == nil {
		for _, dir := range dirs {
			if dir.IsDir() && strings.HasPrefix(dir.Name(), "emulator-") && validOpenSaveType(dir.Name()) {
				entries = append(entries, s.openSaveFilesForType(root, dir.Name())...)
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Type != entries[j].Type {
			return entries[i].Type < entries[j].Type
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func (s *server) openSaveFilesForType(root, kind string) []openSaveFile {
	dir := filepath.Join(root, kind)
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	files := make([]openSaveFile, 0, len(items))
	for _, item := range items {
		if item.IsDir() || !validOpenSaveName(item.Name()) {
			continue
		}
		if info, err := item.Info(); err == nil {
			file := openSaveFile{Type: kind, Name: item.Name(), SizeBytes: info.Size(), CreatedTime: info.ModTime().UTC()}
			if metadata, ok := s.openSaveCatalog.Files[openSaveCatalogKey(kind, item.Name())]; ok {
				file.SHA256 = metadata.SHA256
				file.Device = metadata.Device
				file.DeviceName = metadata.DeviceName
				file.UploadedAt = metadata.UploadedAt
			}
			files = append(files, file)
		}
	}
	return files
}

func (s *server) handleOpenSaveNativePut(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("type"), r.PathValue("name")
	destination, ok := s.openSavePath(kind, name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid OpenSave type or file name")
		return
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create OpenSave storage")
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create OpenSave upload")
		return
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), http.MaxBytesReader(w, r.Body, s.maxObjectBytes+1))
	if copyErr == nil {
		copyErr = temporary.Sync()
	}
	if closeErr := temporary.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil || written > s.maxObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "OpenSave archive exceeds configured size limit")
		return
	}
	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	digest := hex.EncodeToString(hash.Sum(nil))
	// Link publication is create-only: a second uploader can never replace an
	// archive that another request has already finished. The temporary file and
	// destination live in the same directory, so a successful link is atomic.
	// We deliberately fail closed on filesystems that cannot provide this
	// guarantee instead of falling back to a truncating write.
	if err := os.Link(temporaryPath, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			writeError(w, http.StatusInternalServerError, "cannot publish OpenSave archive")
			return
		}
		existingDigest, digestErr := s.openSaveArchiveDigestLocked(kind, name)
		if digestErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot verify existing OpenSave archive")
			return
		}
		if existingDigest == digest {
			// A lost response may make a client retry the same immutable archive.
			// Treat that as success without changing its original audit metadata.
			w.WriteHeader(http.StatusOK)
			return
		}
		writeError(w, http.StatusConflict, "OpenSave archive name already has different content")
		return
	}
	// The destination now has its own hard link. The deferred removal is a
	// second safeguard if this unlink fails during a transient filesystem error.
	_ = os.Remove(temporaryPath)

	metadata := openSaveFileMetadata{
		SHA256:     digest,
		Device:     requestDeviceID(r),
		DeviceName: trimDeviceField(requestClientHeader(r, "X-Eden-Device-Name", "X-OpenSave-Device-Name"), 80),
		UploadedAt: time.Now().UTC(),
	}
	next := s.openSaveCatalog.clone()
	next.Files[openSaveCatalogKey(kind, name)] = metadata
	if err := s.persistOpenSaveCatalog(next); err != nil {
		// The archive itself is safely published. Do not turn a completed backup
		// into a client failure just because its advisory audit record is delayed.
		slog.Warn("persist OpenSave archive metadata", "type", kind, "name", name, "error", err)
	} else {
		s.openSaveCatalog = next
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *server) handleOpenSaveNativeGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, ok := s.openSavePath(r.PathValue("type"), name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid OpenSave type or file name")
		return
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "OpenSave archive not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot open OpenSave archive")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot inspect OpenSave archive")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (s *server) handleOpenSaveNativeDelete(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("type"), r.PathValue("name")
	path, ok := s.openSavePath(kind, name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid OpenSave type or file name")
		return
	}
	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	if s.openSaveArchiveReferencedLocked(kind, name) {
		writeError(w, http.StatusConflict, "OpenSave archive is referenced by a recoverable snapshot")
		return
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "OpenSave archive not found")
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete OpenSave archive")
	} else {
		next := s.openSaveCatalog.clone()
		delete(next.Files, openSaveCatalogKey(kind, name))
		if err := s.persistOpenSaveCatalog(next); err != nil {
			slog.Warn("persist OpenSave archive deletion", "type", kind, "name", name, "error", err)
		} else {
			s.openSaveCatalog = next
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
