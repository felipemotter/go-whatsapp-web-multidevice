package chatwoot

import (
	"context"
	"testing"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/config"
)

func TestPerDeviceSyncServicesAreDistinct(t *testing.T) {
	t.Cleanup(func() { _ = CloseAllSyncServices() })

	c1 := NewClientFromConfig("https://a.example.com", "t", 1, 1)
	c2 := NewClientFromConfig("https://b.example.com", "t", 2, 2)

	s1 := GetSyncServiceForDevice("devA", c1, nil, false, 1)
	s2 := GetSyncServiceForDevice("devB", c2, nil, false, 2)

	if s1 == s2 {
		t.Fatal("expected distinct sync services per device")
	}
	if s1.client != c1 || s2.client != c2 {
		t.Fatal("each sync service must hold its own device client")
	}
	// configID must be stamped so REST history-sync links are account/config
	// scoped (else reverse routing could cross accounts).
	if s1.configID != 1 || s2.configID != 2 {
		t.Fatalf("configID not threaded: s1=%d s2=%d", s1.configID, s2.configID)
	}
	// Same key returns the cached instance.
	if again := GetSyncServiceForDevice("devA", c1, nil, false, 1); again != s1 {
		t.Fatal("GetSyncServiceForDevice should be idempotent per key")
	}
	// Per-device services must not enable direct-Postgres import; legacy may.
	if s1.allowPgImport {
		t.Fatal("per-device service must not allow pg import")
	}
	legacy := GetSyncServiceForDevice(legacySyncServiceKey, c1, nil, true, 0)
	if !legacy.allowPgImport {
		t.Fatal("legacy service should allow pg import")
	}
	if LookupSyncServiceForDevice(legacySyncServiceKey) != legacy {
		t.Fatal("legacy-keyed lookup should return the legacy sync service")
	}
}

func TestSyncServiceRebuiltOnClientChange(t *testing.T) {
	t.Cleanup(func() { _ = CloseAllSyncServices() })

	orig := GetSyncServiceForDevice("devRot", NewClientFromConfig("https://a.example.com", "old-token", 1, 1), nil, false, 1)

	// An equivalent client (same destination + credentials, different pointer)
	// must reuse the cached service — identity is by value, not pointer.
	same := GetSyncServiceForDevice("devRot", NewClientFromConfig("https://a.example.com", "old-token", 1, 1), nil, false, 1)
	if same != orig {
		t.Fatal("equivalent client should reuse the cached sync service")
	}

	// Simulate an in-flight run on the original service before rotation.
	running := NewSyncProgress("628@s.whatsapp.net")
	orig.progressMu.Lock()
	orig.progressMap["628@s.whatsapp.net"] = running
	orig.progressMu.Unlock()
	running.SetRunning()

	// A rotated token is a different client identity: the cached service must be
	// rebuilt rather than continuing to use the stale (revoked) token.
	rotated := NewClientFromConfig("https://a.example.com", "new-token", 1, 1)
	got := GetSyncServiceForDevice("devRot", rotated, nil, false, 1)
	if got == orig {
		t.Fatal("rotated token should rebuild the sync service")
	}
	if got.client != rotated {
		t.Fatal("rebuilt sync service must bind the new client")
	}
	if LookupSyncServiceForDevice("devRot") != got {
		t.Fatal("cache should now hold the rebuilt service")
	}

	// The in-flight run must survive the rebuild: still visible in progress and
	// still blocking a concurrent second run for the same device.
	if !got.IsRunning("628@s.whatsapp.net") {
		t.Fatal("rebuilt service lost the in-flight run guard")
	}
	if p := got.GetProgress("628@s.whatsapp.net"); p == nil || p.Status != "running" {
		t.Fatalf("rebuilt service lost progress visibility: %+v", p)
	}
	running.SetCompleted()
	if got.IsRunning("628@s.whatsapp.net") {
		t.Fatal("completed run should unblock the device (shared progress pointer)")
	}
}

// With the global DSN configured, only the legacy service
// reaches the direct-Postgres importer. The per-device service, built as REST
// and auto-sync build it, returns (nil, nil) and keeps using REST. The legacy
// contrast proves the DSN is live for the test: it is dialed and refused.
func TestPgImporterGatedByAllowPgImport(t *testing.T) {
	prevURI := config.ChatwootImportDBURI
	prevAccount := config.ChatwootAccountID
	prevInbox := config.ChatwootInboxID
	t.Cleanup(func() {
		config.ChatwootImportDBURI = prevURI
		config.ChatwootAccountID = prevAccount
		config.ChatwootInboxID = prevInbox
		_ = CloseAllSyncServices()
	})
	// Port 1 on loopback refuses immediately, so opening the importer fails fast.
	config.ChatwootImportDBURI = "postgres://chatwoot:secret@127.0.0.1:1/chatwoot?sslmode=disable&connect_timeout=1"
	config.ChatwootAccountID = 3
	config.ChatwootInboxID = 5

	tests := []struct {
		name     string
		key      string
		configID int64
		wantErr  bool
	}{
		{name: "per-device config 11", key: "device-a@s.whatsapp.net", configID: 11, wantErr: false},
		{name: "legacy config 0", key: legacySyncServiceKey, configID: 0, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClientFromConfig("https://chatwoot.example.com", "t", 3, 5)
			s := GetSyncServiceForDevice(tc.key, client, nil, tc.configID == 0, tc.configID)
			imp, err := s.pgImporterForSync(context.Background())
			if imp != nil {
				t.Fatalf("importer = %v, want nil", imp)
			}
			if s.pgImporter != nil {
				t.Fatalf("cached importer = %v, want nil", s.pgImporter)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("legacy service error = nil, want the configured DSN to be dialed and fail")
				}
				return
			}
			if err != nil {
				t.Fatalf("per-device service error = %v, want nil (REST-only even with ChatwootImportDBURI set)", err)
			}
		})
	}
}

func TestSyncServiceKeyFor(t *testing.T) {
	if k := SyncServiceKeyFor(nil); k != legacySyncServiceKey {
		t.Fatalf("nil -> %q, want legacy key", k)
	}
	if k := SyncServiceKeyFor(&ResolvedConfig{ConfigID: 0, DeviceID: "d"}); k != legacySyncServiceKey {
		t.Fatalf("legacy config -> %q, want legacy key", k)
	}
	if k := SyncServiceKeyFor(&ResolvedConfig{ConfigID: 5, DeviceID: "d"}); k != "d" {
		t.Fatalf("per-device config -> %q, want device id", k)
	}
}
