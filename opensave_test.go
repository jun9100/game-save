// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenSaveNativeFilesAreTypedAndTokenProtected(t *testing.T) {
	dataDir := t.TempDir()
	s, err := newServer(dataDir, "sync-token", 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	url := httpServer.URL + "/v1/opensave/files/switch/mario__main__snap_1.zip"
	unauthorized, err := http.NewRequest(http.MethodPut, url, bytes.NewReader([]byte("save")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upload status = %d, want 401", response.StatusCode)
	}

	request, err := http.NewRequest(http.MethodPut, url, bytes.NewReader([]byte("save")))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sync-token")
	request.Header.Set("X-OpenSave-Device-ID", "desktop-1")
	request.Header.Set("X-OpenSave-Device-Name", "Test Desktop")
	request.Header.Set("X-OpenSave-Platform", "opensave-windows")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", response.StatusCode)
	}
	stored, err := os.ReadFile(filepath.Join(dataDir, "opensave-native", "switch", "mario__main__snap_1.zip"))
	if err != nil || string(stored) != "save" {
		t.Fatalf("native archive = %q, err=%v", stored, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "opensave", "switch", "mario__main__snap_1.zip")); !os.IsNotExist(err) {
		t.Fatalf("native archive leaked into legacy WebDAV storage: %v", err)
	}

	list, err := http.NewRequest(http.MethodGet, httpServer.URL+"/v1/opensave/files", nil)
	if err != nil {
		t.Fatal(err)
	}
	list.Header.Set("Authorization", "Bearer sync-token")
	response, err = http.DefaultClient.Do(list)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Files []openSaveFile `json:"files"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 1 || body.Files[0].Type != "switch" || body.Files[0].Name != "mario__main__snap_1.zip" {
		t.Fatalf("list = %+v", body.Files)
	}
	wantHash := sha256.Sum256([]byte("save"))
	if body.Files[0].SHA256 != hex.EncodeToString(wantHash[:]) || body.Files[0].Device != "desktop-1" ||
		body.Files[0].DeviceName != "Test Desktop" || body.Files[0].UploadedAt.IsZero() {
		t.Fatalf("audit metadata = %+v", body.Files[0])
	}
	if device, ok := s.dashboard.Devices["desktop-1"]; !ok || device.Name != "Test Desktop" || device.Platform != "opensave-windows" {
		t.Fatalf("recorded device = %+v, known=%v", device, ok)
	}

	// Metadata is separate from archive bytes and must survive a service restart.
	restarted, err := newServer(dataDir, "sync-token", 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	files := restarted.listOpenSaveFiles()
	if len(files) != 1 || files[0].SHA256 != hex.EncodeToString(wantHash[:]) || files[0].Device != "desktop-1" {
		t.Fatalf("restarted archive metadata = %+v", files)
	}

	remove, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	remove.Header.Set("Authorization", "Bearer sync-token")
	response, err = http.DefaultClient.Do(remove)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", response.StatusCode)
	}
	contents, err := os.ReadFile(filepath.Join(dataDir, "opensave-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog openSaveCatalog
	if err := json.Unmarshal(contents, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Files) != 0 {
		t.Fatalf("catalog retained deleted archive: %+v", catalog.Files)
	}
}

func TestOpenSaveCatalogSupportsEmulatorNamespacesAndAdminAudit(t *testing.T) {
	dataDir := t.TempDir()
	s, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	url := httpServer.URL + "/v1/opensave/files/emulator-eden/odyssey__profile-1__snapshot.zip"
	request, err := http.NewRequest(http.MethodPut, url, bytes.NewReader([]byte("emulator save")))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-OpenSave-Device-ID", "odin2-1")
	request.Header.Set("X-OpenSave-Device-Name", "Odin2")
	request.Header.Set("X-OpenSave-Platform", "opensave-android")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", response.StatusCode)
	}

	response, body := adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/opensave/files", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin catalog status = %d: %s", response.StatusCode, body)
	}
	var catalog struct {
		Files []openSaveFile `json:"files"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Files) != 1 || catalog.Files[0].Type != "emulator-eden" ||
		catalog.Files[0].DeviceName != "Odin2" || catalog.Files[0].SHA256 == "" {
		t.Fatalf("admin catalog = %+v", catalog.Files)
	}

	overview := fetchOverview(t, httpServer.URL)
	if overview.Stats.OpenSaveFiles != 1 || overview.Stats.OpenSaveBytes != int64(len("emulator save")) {
		t.Fatalf("OpenSave overview statistics = %+v", overview.Stats)
	}
	response, metrics := adminDo(t, http.MethodGet, httpServer.URL+"/metrics", nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(metrics, []byte("game_save_opensave_archives 1")) ||
		!bytes.Contains(metrics, []byte("game_save_opensave_archive_bytes 13")) {
		t.Fatalf("OpenSave metrics = %d %s", response.StatusCode, metrics)
	}
}
