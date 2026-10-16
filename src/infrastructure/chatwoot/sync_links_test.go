package chatwoot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/chatwoot/pgimport"
)

type chatwootSyncLinkRepo struct {
	domainChatStorage.IChatStorageRepository
	links   map[string]*domainChatStorage.ChatwootMessageLink
	upserts int
}

func newChatwootSyncLinkTestRepo() *chatwootSyncLinkRepo {
	return &chatwootSyncLinkRepo{links: make(map[string]*domainChatStorage.ChatwootMessageLink)}
}

func chatwootSyncLinkKey(deviceID, waMessageID string) string {
	return deviceID + "\x00" + waMessageID
}

func (r *chatwootSyncLinkRepo) UpsertChatwootMessageLink(link *domainChatStorage.ChatwootMessageLink) error {
	r.upserts++
	cloned := *link
	r.links[chatwootSyncLinkKey(link.DeviceID, link.WhatsAppMessageID)] = &cloned
	return nil
}

func (r *chatwootSyncLinkRepo) GetChatwootMessageLinkByWhatsAppID(deviceID, waMessageID string) (*domainChatStorage.ChatwootMessageLink, error) {
	link := r.links[chatwootSyncLinkKey(deviceID, waMessageID)]
	if link == nil {
		return nil, nil
	}
	cloned := *link
	return &cloned, nil
}

// Synthetic scope shared by the link-producer tests below.
const (
	syncTestDevice       = "device-a@s.whatsapp.net"
	syncTestChat         = "peer-a@s.whatsapp.net"
	syncTestAccount      = 3
	syncTestInbox        = 5
	syncTestConversation = 7
	syncTestConfig       = int64(11)
)

// newSyncTestService builds the sync service the same way REST and auto-sync
// do (GetSyncServiceForDevice, legacy key and pg import only for config 0),
// bound to a Chatwoot client that targets server.
func newSyncTestService(t *testing.T, server *httptest.Server, repo domainChatStorage.IChatStorageRepository, configID int64) *SyncService {
	t.Helper()
	t.Cleanup(func() { _ = CloseAllSyncServices() })
	key := legacySyncServiceKey
	if configID != 0 {
		key = syncTestDevice
	}
	client := &Client{
		BaseURL:    server.URL,
		APIToken:   "token",
		AccountID:  syncTestAccount,
		InboxID:    syncTestInbox,
		HTTPClient: server.Client(),
	}
	svc := GetSyncServiceForDevice(key, client, repo, configID == 0, configID)
	if svc.client != client {
		t.Fatal("sync service is not bound to the test client")
	}
	return svc
}

// Deduplication by WhatsApp link. An existing link stops the REST sync
// producer whatever configuration it carries; the sync never promotes a legacy
// link to the service config nor overwrites a delivered reply's link.
func TestSyncMessageSkipsExistingChatwootLink(t *testing.T) {
	legacyLink := domainChatStorage.ChatwootMessageLink{
		DeviceID:                     syncTestDevice,
		WhatsAppMessageID:            "WA-OLD",
		WhatsAppChatJID:              syncTestChat,
		ChatwootMessageID:            40,
		ChatwootConversationID:       syncTestConversation,
		ChatwootInboxID:              syncTestInbox,
		ChatwootContactInboxSourceID: syncTestChat,
		SourceID:                     "WAID:WA-OLD",
		Direction:                    "incoming",
		ChatwootConfigID:             0,
		ChatwootAccountID:            syncTestAccount,
	}
	forwardedLink := legacyLink
	forwardedLink.WhatsAppMessageID = "WA-FWD"
	forwardedLink.ChatwootMessageID = 41
	forwardedLink.SourceID = "WAID:WA-FWD"
	forwardedLink.ChatwootConfigID = syncTestConfig
	replyLink := legacyLink
	replyLink.WhatsAppMessageID = "WA-REPLY"
	replyLink.ChatwootMessageID = 42
	replyLink.SourceID = ""
	replyLink.Direction = "outgoing"
	replyLink.ChatwootConfigID = syncTestConfig

	seeds := []struct {
		name string
		link domainChatStorage.ChatwootMessageLink
	}{
		{"legacy link config 0", legacyLink},
		{"forwarded link config 11", forwardedLink},
		{"reply link config 11", replyLink},
	}
	services := []int64{0, syncTestConfig}
	producers := []struct {
		name         string
		requireMedia bool
	}{
		{"history sync", false},
		{"hybrid media pre-pass", true},
	}

	for _, seed := range seeds {
		for _, serviceConfig := range services {
			for _, producer := range producers {
				name := fmt.Sprintf("%s/service config %d/%s", seed.name, serviceConfig, producer.name)
				t.Run(name, func(t *testing.T) {
					repo := newChatwootSyncLinkTestRepo()
					if err := repo.UpsertChatwootMessageLink(&seed.link); err != nil {
						t.Fatalf("seed link: %v", err)
					}
					repo.upserts = 0

					var requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						w.WriteHeader(http.StatusInternalServerError)
					}))
					defer server.Close()
					svc := newSyncTestService(t, server, repo, serviceConfig)

					msg := &domainChatStorage.Message{
						ID:        seed.link.WhatsAppMessageID,
						DeviceID:  syncTestDevice,
						ChatJID:   syncTestChat,
						Content:   "already in Chatwoot",
						IsFromMe:  seed.link.Direction == "outgoing",
						Timestamp: time.Date(2026, time.June, 6, 10, 0, 0, 0, time.UTC),
					}
					if producer.requireMedia {
						msg.MediaType = "image"
						msg.URL = "https://mmg.whatsapp.net/file"
						msg.MediaKey = []byte("key")
					}

					err := svc.syncMessageWithOptions(context.Background(), syncTestConversation, msg, nil,
						SyncOptions{IncludeMedia: producer.requireMedia}, false, producer.requireMedia)
					if err != nil {
						t.Fatalf("syncMessageWithOptions: %v", err)
					}
					if got := requests.Load(); got != 0 {
						t.Fatalf("Chatwoot message creations = %d, want 0 for an existing link", got)
					}
					if repo.upserts != 0 {
						t.Fatalf("link upserts after seed = %d, want 0", repo.upserts)
					}
					got, err := repo.GetChatwootMessageLinkByWhatsAppID(syncTestDevice, seed.link.WhatsAppMessageID)
					if err != nil || got == nil {
						t.Fatalf("stored link = %+v, err = %v; want the seeded link", got, err)
					}
					if *got != seed.link {
						t.Fatalf("stored link = %+v, want unchanged %+v", *got, seed.link)
					}
				})
			}
		}
	}
}

// The REST history sync stamps the service's config on the links it
// creates, together with the client's account and inbox.
func TestSyncMessageStoresChatwootLinkAfterCreate(t *testing.T) {
	for _, configID := range []int64{0, syncTestConfig} {
		t.Run(fmt.Sprintf("service config %d", configID), func(t *testing.T) {
			repo := newChatwootSyncLinkTestRepo()
			msg := &domainChatStorage.Message{
				ID:        "WA-SYNC",
				DeviceID:  syncTestDevice,
				ChatJID:   syncTestChat,
				Content:   "new sync",
				Timestamp: time.Date(2026, time.June, 6, 10, 0, 0, 0, time.UTC),
			}

			var creates, unexpected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/accounts/3/conversations/7/messages" {
					unexpected.Add(1)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				creates.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":888}`))
			}))
			defer server.Close()
			svc := newSyncTestService(t, server, repo, configID)

			if err := svc.syncMessage(context.Background(), syncTestConversation, msg, nil, SyncOptions{}, false); err != nil {
				t.Fatalf("syncMessage: %v", err)
			}
			if got := creates.Load(); got != 1 {
				t.Fatalf("Chatwoot message creations = %d, want 1", got)
			}
			if got := unexpected.Load(); got != 0 {
				t.Fatalf("unexpected Chatwoot requests = %d, want 0", got)
			}
			if repo.upserts != 1 || len(repo.links) != 1 {
				t.Fatalf("link upserts = %d, stored links = %d; want 1 and 1", repo.upserts, len(repo.links))
			}

			link, err := repo.GetChatwootMessageLinkByWhatsAppID(syncTestDevice, "WA-SYNC")
			if err != nil || link == nil {
				t.Fatalf("stored link = %+v, err = %v; want a link", link, err)
			}
			want := domainChatStorage.ChatwootMessageLink{
				DeviceID:                     syncTestDevice,
				WhatsAppMessageID:            "WA-SYNC",
				WhatsAppChatJID:              syncTestChat,
				ChatwootMessageID:            888,
				ChatwootConversationID:       syncTestConversation,
				ChatwootInboxID:              syncTestInbox,
				ChatwootContactInboxSourceID: syncTestChat,
				SourceID:                     "WAID:WA-SYNC",
				Direction:                    "incoming",
				ChatwootConfigID:             configID,
				ChatwootAccountID:            syncTestAccount,
			}
			if *link != want {
				t.Fatalf("stored link = %+v, want %+v", *link, want)
			}
		})
	}
}

func TestSyncMessageWithRequiredMediaDoesNotCreatePlaceholderWhenDownloadFails(t *testing.T) {
	repo := newChatwootSyncLinkTestRepo()
	msg := &domainChatStorage.Message{
		ID:        "wa-media",
		DeviceID:  "device-a@s.whatsapp.net",
		ChatJID:   "628123456789@s.whatsapp.net",
		MediaType: "image",
		URL:       "https://mmg.whatsapp.net/file",
		MediaKey:  []byte("key"),
		Timestamp: time.Date(2026, time.June, 6, 10, 0, 0, 0, time.UTC),
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":888}`))
	}))
	defer server.Close()

	svc := NewSyncService(&Client{
		BaseURL:    server.URL,
		APIToken:   "token",
		AccountID:  1,
		InboxID:    2,
		HTTPClient: server.Client(),
	}, repo)

	err := svc.syncMessageWithOptions(context.Background(), 42, msg, nil, SyncOptions{IncludeMedia: true}, false, true)
	if err == nil {
		t.Fatal("syncMessageWithOptions error = nil, want media download failure")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("Chatwoot received %d requests, want 0 when required media cannot be attached", got)
	}
}

// Links returned by the legacy direct-Postgres importer
// carry config 0 and the importer's account; the legacy service persists them
// exactly as returned, without stamping its own config.
func TestStoreChatwootImportLinksPersistsReturnedLinks(t *testing.T) {
	repo := newChatwootSyncLinkTestRepo()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Chatwoot request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	svc := newSyncTestService(t, server, repo, 0)
	if !svc.allowPgImport || svc.configID != 0 {
		t.Fatalf("legacy service allowPgImport=%v configID=%d, want true/0", svc.allowPgImport, svc.configID)
	}

	imported := domainChatStorage.ChatwootMessageLink{
		DeviceID:                     syncTestDevice,
		WhatsAppMessageID:            "WA-IMPORTED",
		WhatsAppChatJID:              syncTestChat,
		ChatwootMessageID:            909,
		ChatwootConversationID:       syncTestConversation,
		ChatwootInboxID:              syncTestInbox,
		ChatwootContactInboxSourceID: syncTestChat,
		SourceID:                     "WAID:WA-IMPORTED",
		Direction:                    "incoming",
		ChatwootAccountID:            syncTestAccount,
	}
	result := &pgimport.ImportResult{Links: []domainChatStorage.ChatwootMessageLink{imported}}

	if err := svc.storeChatwootImportLinks(result); err != nil {
		t.Fatalf("storeChatwootImportLinks: %v", err)
	}
	if repo.upserts != 1 {
		t.Fatalf("link upserts = %d, want 1", repo.upserts)
	}

	link, err := repo.GetChatwootMessageLinkByWhatsAppID(syncTestDevice, "WA-IMPORTED")
	if err != nil || link == nil {
		t.Fatalf("stored link = %+v, err = %v; want the imported link", link, err)
	}
	if link.ChatwootConfigID != 0 || link.ChatwootAccountID != syncTestAccount {
		t.Fatalf("stored link config=%d account=%d, want 0/%d", link.ChatwootConfigID, link.ChatwootAccountID, syncTestAccount)
	}
	if *link != imported {
		t.Fatalf("stored link = %+v, want the importer result unchanged %+v", *link, imported)
	}
}
