package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/config"
	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/chatwoot"
)

// TestSyncPayloadToChatwootFailFast verifies the forward path skips silently
// (no error, no retry) when the device resolves to no Chatwoot config, and
// propagates a transient resolution error so the caller can retry.
func TestSyncPayloadToChatwootFailFast(t *testing.T) {
	orig := getChatwootClientFn
	t.Cleanup(func() { getChatwootClientFn = orig })

	payload := map[string]any{"payload": map[string]any{"id": "wa-1", "chat_id": "628@s.whatsapp.net"}}

	// No config for this device -> skip, return nil.
	getChatwootClientFn = func(string) (*chatwoot.ResolvedConfig, error) { return nil, nil }
	if err := syncPayloadToChatwoot(context.Background(), payload, "message", "dev-unmapped", nil); err != nil {
		t.Fatalf("unmapped device should skip with nil, got %v", err)
	}

	// Transient resolution error -> propagate (retryable).
	wantErr := errors.New("storage down")
	getChatwootClientFn = func(string) (*chatwoot.ResolvedConfig, error) { return nil, wantErr }
	if err := syncPayloadToChatwoot(context.Background(), payload, "message", "dev", nil); !errors.Is(err, wantErr) {
		t.Fatalf("resolution error should propagate, got %v", err)
	}
}

func TestChatwootMessageTypeFromPayload(t *testing.T) {
	tests := []struct {
		name     string
		isFromMe any
		expected string
	}{
		{
			name:     "incoming message",
			isFromMe: false,
			expected: "incoming",
		},
		{
			name:     "outgoing message",
			isFromMe: true,
			expected: "outgoing",
		},
		{
			name:     "missing field defaults to incoming",
			isFromMe: nil,
			expected: "incoming",
		},
		{
			name:     "non-bool field defaults to incoming",
			isFromMe: "true",
			expected: "incoming",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{}
			if tc.isFromMe != nil {
				payload["is_from_me"] = tc.isFromMe
			}

			if got := chatwootMessageTypeFromPayload(payload); got != tc.expected {
				t.Fatalf("expected %s, got %s", tc.expected, got)
			}
		})
	}
}

func TestBuildChatwootForwardMessageLink(t *testing.T) {
	data := map[string]any{
		"id":         "wa-live-1",
		"chat_id":    "628123456789@s.whatsapp.net",
		"is_from_me": false,
	}

	link := buildChatwootForwardMessageLink(
		"device-a@s.whatsapp.net",
		42, // configID
		3,  // accountID
		data,
		chatwoot.MessageOptions{SourceID: "WAID:wa-live-1"},
		&chatwootSyncResult{MessageID: 123, ConversationID: 456, InboxID: 789},
	)

	if link == nil {
		t.Fatal("expected chatwoot message link")
	}
	if link.DeviceID != "device-a@s.whatsapp.net" {
		t.Fatalf("DeviceID = %q", link.DeviceID)
	}
	if link.WhatsAppMessageID != "wa-live-1" || link.SourceID != "WAID:wa-live-1" {
		t.Fatalf("unexpected source mapping: %+v", link)
	}
	if link.ChatwootMessageID != 123 || link.ChatwootConversationID != 456 || link.ChatwootInboxID != 789 {
		t.Fatalf("unexpected chatwoot ids: %+v", link)
	}
	if link.ChatwootConfigID != 42 || link.ChatwootAccountID != 3 {
		t.Fatalf("unexpected scope ids: config=%d account=%d", link.ChatwootConfigID, link.ChatwootAccountID)
	}
	if link.Direction != "incoming" {
		t.Fatalf("Direction = %q, want incoming", link.Direction)
	}
}

func TestExtractReceiptMessageIDs(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want []string
	}{
		{name: "string slice", in: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "any slice", in: []any{"a", "b", 3}, want: []string{"a", "b"}},
		{name: "single string", in: "a", want: []string{"a"}},
		{name: "empty", in: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractReceiptMessageIDs(map[string]any{"ids": tc.in})
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestDeleteTargetMessageID(t *testing.T) {
	cases := []struct {
		event string
		data  map[string]any
		want  string
	}{
		{"message.revoked", map[string]any{"revoked_message_id": "wa-1"}, "wa-1"},
		{"message.deleted", map[string]any{"deleted_message_id": "wa-2"}, "wa-2"},
		{"message.deleted", map[string]any{"id": "wa-current"}, "wa-current"},
		{"message", map[string]any{"id": "wa-current"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			if got := deleteTargetMessageID(tc.event, tc.data); got != tc.want {
				t.Fatalf("deleteTargetMessageID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShouldForwardEventToChatwoot(t *testing.T) {
	// message and message.reaction are always forwarded; edits and deletes are
	// gated by their config toggles. Save/restore the globals so other tests in
	// the package see the defaults.
	origEdits := config.ChatwootForwardEdits
	origDeletes := config.ChatwootForwardDeletes
	origRead := config.ChatwootMessageRead
	origMsgDelete := config.ChatwootMessageDelete
	defer func() {
		config.ChatwootForwardEdits = origEdits
		config.ChatwootForwardDeletes = origDeletes
		config.ChatwootMessageRead = origRead
		config.ChatwootMessageDelete = origMsgDelete
	}()

	t.Run("always-on and unrelated events", func(t *testing.T) {
		config.ChatwootMessageRead = false
		cases := map[string]bool{
			"message":          true,
			"message.reaction": true,
			"message.ack":      false,
			"chat_presence":    false,
		}
		for event, want := range cases {
			if got := shouldForwardEventToChatwoot(event); got != want {
				t.Errorf("shouldForwardEventToChatwoot(%q) = %v, want %v", event, got, want)
			}
		}
	})

	t.Run("read receipts follow toggle", func(t *testing.T) {
		config.ChatwootMessageRead = true
		if !shouldForwardEventToChatwoot("message.ack") {
			t.Fatal("message.ack should forward when ChatwootMessageRead is enabled")
		}
		config.ChatwootMessageRead = false
		if shouldForwardEventToChatwoot("message.ack") {
			t.Fatal("message.ack should not forward when ChatwootMessageRead is disabled")
		}
	})

	t.Run("edits and deletes follow their toggles", func(t *testing.T) {
		config.ChatwootMessageDelete = false
		config.ChatwootForwardEdits = true
		config.ChatwootForwardDeletes = true
		for _, e := range []string{"message.edited", "message.revoked", "message.deleted"} {
			if !shouldForwardEventToChatwoot(e) {
				t.Errorf("%q should forward when toggles enabled", e)
			}
		}

		config.ChatwootForwardEdits = false
		config.ChatwootForwardDeletes = false
		for _, e := range []string{"message.edited", "message.revoked", "message.deleted"} {
			if shouldForwardEventToChatwoot(e) {
				t.Errorf("%q should NOT forward when toggles disabled", e)
			}
		}
	})

	t.Run("deletes forward when only ChatwootMessageDelete is enabled", func(t *testing.T) {
		// The hard-delete feature must reach the forwarder even when tombstone-note
		// forwarding (ChatwootForwardDeletes) is off, so the two are independent.
		// Edits remain gated solely on ChatwootForwardEdits.
		config.ChatwootForwardEdits = false
		config.ChatwootForwardDeletes = false
		config.ChatwootMessageDelete = true
		for _, e := range []string{"message.revoked", "message.deleted"} {
			if !shouldForwardEventToChatwoot(e) {
				t.Errorf("%q should forward when ChatwootMessageDelete is enabled", e)
			}
		}
		if shouldForwardEventToChatwoot("message.edited") {
			t.Error("message.edited should NOT forward on ChatwootMessageDelete alone")
		}
	})
}

func TestIsRetryableChatwootForwardEvent(t *testing.T) {
	// Base messages and their sub-events carry a unique WhatsApp id and are
	// retry-eligible; read receipts are best-effort and must not be queued.
	for _, e := range []string{"message", "message.edited", "message.revoked", "message.deleted", "message.reaction"} {
		if !isRetryableChatwootForwardEvent(e) {
			t.Errorf("%q should be retry-eligible", e)
		}
	}
	for _, e := range []string{"message.ack", "chat_presence", ""} {
		if isRetryableChatwootForwardEvent(e) {
			t.Errorf("%q should NOT be retry-eligible", e)
		}
	}
}

func TestIsEventWhitelistedForChatwoot(t *testing.T) {
	originalEvents := config.WhatsappWebhookEvents
	defer func() { config.WhatsappWebhookEvents = originalEvents }()

	t.Run("empty whitelist allows all", func(t *testing.T) {
		config.WhatsappWebhookEvents = nil
		if !isEventWhitelistedForChatwoot("message.reaction") {
			t.Fatal("expected message.reaction to be allowed when whitelist is empty")
		}
	})

	t.Run("explicit reaction whitelist allowed", func(t *testing.T) {
		config.WhatsappWebhookEvents = []string{"message.reaction"}
		if !isEventWhitelistedForChatwoot("message.reaction") {
			t.Fatal("expected message.reaction to be allowed when explicitly whitelisted")
		}
	})

	t.Run("message whitelist also allows reactions for chatwoot", func(t *testing.T) {
		config.WhatsappWebhookEvents = []string{"message"}
		if !isEventWhitelistedForChatwoot("message.reaction") {
			t.Fatal("expected message.reaction to be allowed for Chatwoot when message is whitelisted")
		}
	})

	t.Run("unrelated whitelist blocks reaction", func(t *testing.T) {
		config.WhatsappWebhookEvents = []string{"message.ack"}
		if isEventWhitelistedForChatwoot("message.reaction") {
			t.Fatal("expected message.reaction to be blocked for Chatwoot when not covered by whitelist")
		}
	})
}

func TestExtractMediaPath(t *testing.T) {
	// extractMediaPath handles the two shapes buildAutoDownloadPayload emits
	// for image/video/document fields. The map branch is the regression-prone
	// one: prior to its introduction, captioned images silently dropped their
	// attachment because the string assertion failed and the loop moved on.
	tests := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "plain string path (audio/sticker shape)",
			in:   "/tmp/whatsapp/audio-123.oga",
			want: "/tmp/whatsapp/audio-123.oga",
		},
		{
			name: "map with path key (captioned image/video/document shape)",
			in:   map[string]any{"path": "/tmp/whatsapp/img-1.jpg", "caption": "look at this"},
			want: "/tmp/whatsapp/img-1.jpg",
		},
		{
			name: "map with only url (auto-download disabled) yields empty",
			in:   map[string]any{"url": "https://wa.example/blob.jpg", "caption": "x"},
			want: "",
		},
		{
			name: "nil yields empty",
			in:   nil,
			want: "",
		},
		{
			name: "unrelated type yields empty",
			in:   42,
			want: "",
		},
		{
			name: "empty string returns empty (caller treats as no-op)",
			in:   "",
			want: "",
		},
		{
			name: "map with non-string path yields empty",
			in:   map[string]any{"path": 12345},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractMediaPath(tc.in); got != tc.want {
				t.Fatalf("extractMediaPath(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestChatwootIdentifierForJID(t *testing.T) {
	// chatwootIdentifierForJID guards the boundary into the Chatwoot REST
	// client. The client picks its identifier-vs-phone search path by
	// checking HasSuffix(identifier, "@lid"); stripping that suffix here
	// would silently downgrade @lid contacts to phone-normalized garbage.
	// Each case below pins a specific JID server / suffix so a regression
	// (e.g. someone reintroducing ExtractPhoneFromJID at the call site)
	// surfaces as a focused test failure rather than a downstream search miss.
	tests := []struct {
		name string
		jid  string
		want string
	}{
		{
			name: "ordinary phone JID is stripped to digits",
			jid:  "628123456789@s.whatsapp.net",
			want: "628123456789",
		},
		{
			name: "lid JID is preserved end-to-end so chatwoot client takes identifier path",
			jid:  "1234567890abcd@lid",
			want: "1234567890abcd@lid",
		},
		{
			name: "raw digits without @ pass through untouched",
			jid:  "628123456789",
			want: "628123456789",
		},
		{
			name: "empty string yields empty",
			jid:  "",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatwootIdentifierForJID(tc.jid); got != tc.want {
				t.Fatalf("chatwootIdentifierForJID(%q) = %q, want %q", tc.jid, got, tc.want)
			}
		})
	}
}

func TestBuildReactionChatwootContent(t *testing.T) {
	tests := []struct {
		name     string
		payload  map[string]any
		fromName string
		expected string
	}{
		{
			// The target id is not repeated in the text: when reacted_message_id
			// is present, syncPayloadToChatwoot already threads this note onto
			// the reacted-to message via in_reply_to_external_id, so Chatwoot
			// nests it under that message's bubble.
			name: "reaction with sender name and target id",
			payload: map[string]any{
				"reaction":           "👍",
				"reacted_message_id": "wamid-123",
			},
			fromName: "Alice",
			expected: "Alice reacted 👍",
		},
		{
			name: "reaction falls back to phone",
			payload: map[string]any{
				"reaction":           "🔥",
				"reacted_message_id": "wamid-456",
				"from":               "628123456789@s.whatsapp.net",
			},
			fromName: "",
			expected: "628123456789 reacted 🔥",
		},
		{
			name: "reaction removal",
			payload: map[string]any{
				"reaction":           "",
				"reacted_message_id": "wamid-789",
			},
			fromName: "Bob",
			expected: "Bob removed a reaction",
		},
		{
			name: "reaction falls back to sender jid when pushname missing",
			payload: map[string]any{
				"reaction":           "😂",
				"reacted_message_id": "wamid-999",
				"from":               "628777000111@s.whatsapp.net",
			},
			fromName: "",
			expected: "628777000111 reacted 😂",
		},
		{
			name: "missing target id still produces readable text",
			payload: map[string]any{
				"reaction": "❤️",
			},
			fromName: "Carol",
			expected: "Carol reacted ❤️",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildReactionChatwootContent(tc.payload, tc.fromName); got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

// chatwootLinkMemoryRepo is an in-memory stand-in for the link, config and
// retry-queue parts of the chat-storage repository (the SQLite implementation
// imports this package, so it cannot be used here). Upserts store exactly the
// object received, so a caller that rewrites a link without preserving its
// configuration or creation time is visible in the stored row.
type chatwootLinkMemoryRepo struct {
	domainChatStorage.IChatStorageRepository
	mu                sync.Mutex
	links             []domainChatStorage.ChatwootMessageLink
	upserts           int
	configs           map[string]*domainChatStorage.ChatwootDeviceConfig
	records           []*domainChatStorage.DeviceRecord
	configLinkDeletes []int64
	deviceDataDeletes []string
	due               []*domainChatStorage.ChatwootForwardEvent
	doneIDs           []int64
	failedIDs         []int64
	enqueued          int
}

func newChatwootLinkMemoryRepo(seed ...domainChatStorage.ChatwootMessageLink) *chatwootLinkMemoryRepo {
	return &chatwootLinkMemoryRepo{
		links:   append([]domainChatStorage.ChatwootMessageLink(nil), seed...),
		configs: map[string]*domainChatStorage.ChatwootDeviceConfig{},
	}
}

func (r *chatwootLinkMemoryRepo) find(deviceID, waMessageID string) (int, bool) {
	for i, link := range r.links {
		if link.DeviceID == deviceID && link.WhatsAppMessageID == waMessageID {
			return i, true
		}
	}
	return -1, false
}

// stored returns a copy of the stored link, or nil when it is absent.
func (r *chatwootLinkMemoryRepo) stored(deviceID, waMessageID string) *domainChatStorage.ChatwootMessageLink {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i, ok := r.find(deviceID, waMessageID); ok {
		link := r.links[i]
		return &link
	}
	return nil
}

func (r *chatwootLinkMemoryRepo) UpsertChatwootMessageLink(link *domainChatStorage.ChatwootMessageLink) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserts++
	if i, ok := r.find(link.DeviceID, link.WhatsAppMessageID); ok {
		r.links[i] = *link
		return nil
	}
	r.links = append(r.links, *link)
	return nil
}

func (r *chatwootLinkMemoryRepo) GetChatwootMessageLinkByWhatsAppID(deviceID, waMessageID string) (*domainChatStorage.ChatwootMessageLink, error) {
	return r.stored(deviceID, waMessageID), nil
}

func (r *chatwootLinkMemoryRepo) DeleteDeviceData(deviceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deviceDataDeletes = append(r.deviceDataDeletes, deviceID)
	kept := r.links[:0]
	for _, link := range r.links {
		if link.DeviceID != deviceID {
			kept = append(kept, link)
		}
	}
	r.links = kept
	return nil
}

func (r *chatwootLinkMemoryRepo) GetChatwootDeviceConfig(deviceID string) (*domainChatStorage.ChatwootDeviceConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg, ok := r.configs[deviceID]; ok {
		cloned := *cfg
		return &cloned, nil
	}
	return nil, nil
}

func (r *chatwootLinkMemoryRepo) DeleteChatwootDeviceConfig(deviceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.configs, deviceID)
	return nil
}

// DeleteChatwootMessageLinksByConfig mirrors the SQLite contract: config 0
// (legacy links) is refused, any other id removes exactly its links.
func (r *chatwootLinkMemoryRepo) DeleteChatwootMessageLinksByConfig(configID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configLinkDeletes = append(r.configLinkDeletes, configID)
	if configID == 0 {
		return errors.New("refusing to delete legacy (config id 0) chatwoot message links")
	}
	kept := r.links[:0]
	for _, link := range r.links {
		if link.ChatwootConfigID != configID {
			kept = append(kept, link)
		}
	}
	r.links = kept
	return nil
}

func (r *chatwootLinkMemoryRepo) ListDeviceRecords() ([]*domainChatStorage.DeviceRecord, error) {
	return r.records, nil
}

func (r *chatwootLinkMemoryRepo) DeleteDeviceRecord(string) error { return nil }

func (r *chatwootLinkMemoryRepo) EnqueueChatwootForwardEvent(*domainChatStorage.ChatwootForwardEvent) error {
	r.enqueued++
	return nil
}

func (r *chatwootLinkMemoryRepo) ListDueChatwootForwardEvents(time.Time, int) ([]*domainChatStorage.ChatwootForwardEvent, error) {
	return r.due, nil
}

func (r *chatwootLinkMemoryRepo) MarkChatwootForwardEventDone(id int64) error {
	r.doneIDs = append(r.doneIDs, id)
	return nil
}

func (r *chatwootLinkMemoryRepo) MarkChatwootForwardEventFailed(id int64, _ string, _ time.Time) error {
	r.failedIDs = append(r.failedIDs, id)
	return nil
}

// fakeChatwootAPI answers the Chatwoot endpoints the forwarder uses for
// account 3 / inbox 5: contact 9 has the open conversation 7, and every created
// message gets the configured id. Each request is recorded as "METHOD path".
type fakeChatwootAPI struct {
	mu          sync.Mutex
	requests    []string
	sourceIDs   []string
	nextMessage int
}

func newFakeChatwootAPI(t *testing.T, nextMessage int) (*fakeChatwootAPI, *chatwoot.Client) {
	t.Helper()
	api := &fakeChatwootAPI{nextMessage: nextMessage}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	client := &chatwoot.Client{
		BaseURL:         srv.URL,
		APIToken:        "token-a",
		AccountID:       3,
		InboxID:         5,
		InboxIdentifier: "inbox-a",
		HTTPClient:      srv.Client(),
	}
	return api, client
}

func (a *fakeChatwootAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/accounts/3/contacts/search":
		_, _ = w.Write([]byte(`{"payload":[]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/accounts/3/contacts":
		_, _ = w.Write([]byte(`{"payload":{"contact":{"id":9}}}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/accounts/3/contacts/9/conversations":
		_, _ = w.Write([]byte(`{"payload":[{"id":7,"inbox_id":5,"status":"open"}]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/accounts/3/conversations/7/messages":
		var body struct {
			SourceID string `json:"source_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.sourceIDs = append(a.sourceIDs, body.SourceID)
		_, _ = fmt.Fprintf(w, `{"id":%d}`, a.nextMessage)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/accounts/3/conversations/7/messages/"):
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/update_last_seen"):
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func (a *fakeChatwootAPI) recorded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func (a *fakeChatwootAPI) count(request string) int {
	n := 0
	for _, got := range a.recorded() {
		if got == request {
			n++
		}
	}
	return n
}

const (
	fwDevice = "device-a@s.whatsapp.net"
	fwChat   = "peer-a@s.whatsapp.net"
)

var (
	fwCreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fwUpdatedAt = time.Date(2026, 1, 2, 3, 9, 5, 0, time.UTC)
)

// fwLink builds a stored link of conversation 7 / inbox 5 on device-a.
func fwLink(waMessageID string, chatwootMessageID int, configID int64, accountID int, sourceID, direction string) domainChatStorage.ChatwootMessageLink {
	return domainChatStorage.ChatwootMessageLink{
		DeviceID:                     fwDevice,
		WhatsAppMessageID:            waMessageID,
		WhatsAppChatJID:              fwChat,
		ChatwootMessageID:            chatwootMessageID,
		ChatwootConversationID:       7,
		ChatwootInboxID:              5,
		ChatwootContactInboxSourceID: fwChat,
		SourceID:                     sourceID,
		Direction:                    direction,
		CreatedAt:                    fwCreatedAt,
		UpdatedAt:                    fwUpdatedAt,
		ChatwootConfigID:             configID,
		ChatwootAccountID:            accountID,
	}
}

// stubChatwootResolution points the forwarder at client under configID and
// records the device identifiers it is resolved with.
func stubChatwootResolution(t *testing.T, client *chatwoot.Client, configID int64) *[]string {
	t.Helper()
	orig := getChatwootClientFn
	t.Cleanup(func() { getChatwootClientFn = orig })
	var resolvedWith []string
	getChatwootClientFn = func(deviceID string) (*chatwoot.ResolvedConfig, error) {
		resolvedWith = append(resolvedWith, deviceID)
		return &chatwoot.ResolvedConfig{ConfigID: configID, DeviceID: "slot-a", Client: client}, nil
	}
	return &resolvedWith
}

func fwMessagePayload(waMessageID string, fromMe bool) map[string]any {
	return map[string]any{"payload": map[string]any{
		"id":         waMessageID,
		"chat_id":    fwChat,
		"from":       fwChat,
		"from_name":  "Peer A",
		"body":       "Oi",
		"is_from_me": fromMe,
	}}
}

// deliverForwardEvent runs a "message" event through the live forwarder (with
// the slot's scoped storage in the context) or through the retry worker.
func deliverForwardEvent(t *testing.T, path string, repo *chatwootLinkMemoryRepo, payload map[string]any) {
	t.Helper()
	switch path {
	case "live":
		instance := &DeviceInstance{id: "slot-a", jid: fwDevice, chatStorageRepo: newDeviceChatStorage(fwDevice, repo)}
		forwardToChatwoot(ContextWithDevice(context.Background(), instance), payload, "message")
		if repo.enqueued != 0 {
			t.Fatalf("live forward queued %d retries, want 0", repo.enqueued)
		}
	case "retry":
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal retry payload: %v", err)
		}
		repo.due = []*domainChatStorage.ChatwootForwardEvent{{ID: 1, DeviceID: fwDevice, EventName: "message", PayloadJSON: string(raw)}}
		processDueChatwootForwardRetries(repo)
		if !slices.Equal(repo.doneIDs, []int64{1}) || len(repo.failedIDs) != 0 {
			t.Fatalf("retry done=%v failed=%v, want done=[1] failed=[]", repo.doneIDs, repo.failedIDs)
		}
	default:
		t.Fatalf("unknown path %q", path)
	}
}

// The forwarder (live and retry) persists the link under the
// configuration the registry resolved for the device — 0 in legacy mode, the
// positive id in per-device mode — together with account, inbox and the
// WAID source id it sent to Chatwoot.
func TestChatwootForwardStoresResolvedConfigOnLink(t *testing.T) {
	cases := []struct {
		path     string
		configID int64
	}{
		{path: "live", configID: 0},
		{path: "live", configID: 11},
		{path: "retry", configID: 0},
		{path: "retry", configID: 11},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s config %d", tc.path, tc.configID), func(t *testing.T) {
			api, client := newFakeChatwootAPI(t, 41)
			resolvedWith := stubChatwootResolution(t, client, tc.configID)
			repo := newChatwootLinkMemoryRepo()

			deliverForwardEvent(t, tc.path, repo, fwMessagePayload("WA-FWD", false))

			if !slices.Equal(*resolvedWith, []string{fwDevice}) {
				t.Fatalf("resolved with %q, want [%q]", *resolvedWith, fwDevice)
			}
			if !slices.Equal(api.sourceIDs, []string{"WAID:WA-FWD"}) {
				t.Fatalf("created messages source ids = %q, want [WAID:WA-FWD]", api.sourceIDs)
			}
			if repo.upserts != 1 {
				t.Fatalf("upserts = %d, want 1", repo.upserts)
			}
			link := repo.stored(fwDevice, "WA-FWD")
			if link == nil {
				t.Fatal("forwarded message link was not stored")
			}
			if link.ChatwootConfigID != tc.configID || link.ChatwootAccountID != 3 || link.ChatwootInboxID != 5 || link.SourceID != "WAID:WA-FWD" {
				t.Fatalf("link scope = config %d account %d inbox %d source %q, want config %d account 3 inbox 5 source WAID:WA-FWD",
					link.ChatwootConfigID, link.ChatwootAccountID, link.ChatwootInboxID, link.SourceID, tc.configID)
			}
			if link.ChatwootMessageID != 41 || link.ChatwootConversationID != 7 || link.WhatsAppChatJID != fwChat {
				t.Fatalf("link ids = message %d conversation %d chat %q, want 41 / 7 / %q",
					link.ChatwootMessageID, link.ChatwootConversationID, link.WhatsAppChatJID, fwChat)
			}
		})
	}
}

// A message whose WhatsApp id already has a link is not mirrored again,
// whatever configuration the link or the current resolution carries. The
// legacy link is not promoted to 11 and the echo of the delivered reply does
// not overwrite the reply link.
func TestChatwootForwardSkipsAlreadyLinkedMessage(t *testing.T) {
	legacy := fwLink("WA-OLD", 40, 0, 3, "WAID:WA-OLD", "incoming")
	forwarded := fwLink("WA-FWD", 41, 11, 3, "WAID:WA-FWD", "incoming")
	reply := fwLink("WA-REPLY", 42, 11, 3, "", "outgoing")

	targets := []struct {
		waMessageID string
		fromMe      bool
	}{
		{waMessageID: "WA-OLD", fromMe: false},
		{waMessageID: "WA-FWD", fromMe: false},
		{waMessageID: "WA-REPLY", fromMe: true},
	}
	for _, target := range targets {
		for _, configID := range []int64{0, 11} {
			for _, path := range []string{"live", "retry"} {
				t.Run(fmt.Sprintf("%s resolved %d %s", target.waMessageID, configID, path), func(t *testing.T) {
					api, client := newFakeChatwootAPI(t, 99)
					stubChatwootResolution(t, client, configID)
					repo := newChatwootLinkMemoryRepo(legacy, forwarded, reply)

					deliverForwardEvent(t, path, repo, fwMessagePayload(target.waMessageID, target.fromMe))

					if got := api.recorded(); len(got) != 0 {
						t.Fatalf("Chatwoot requests = %q, want none", got)
					}
					if repo.upserts != 0 {
						t.Fatalf("upserts after seed = %d, want 0", repo.upserts)
					}
					for _, want := range []domainChatStorage.ChatwootMessageLink{legacy, forwarded, reply} {
						got := repo.stored(fwDevice, want.WhatsAppMessageID)
						if got == nil || *got != want {
							t.Fatalf("link %s = %+v, want unchanged %+v", want.WhatsAppMessageID, got, want)
						}
					}
				})
			}
		}
	}
}

// On the forwarder side, a read receipt posts update_last_seen for the linked
// conversation and rewrites the whole link with only IsRead flipped, so the
// configuration and creation time survive for 0 and 11. A link of another
// account is neither posted nor touched.
func TestChatwootReadReceiptPreservesLinkConfig(t *testing.T) {
	cases := []struct {
		name      string
		link      domainChatStorage.ChatwootMessageLink
		wantPosts int
	}{
		{name: "legacy link with account 0", link: fwLink("WA-OLD", 40, 0, 0, "WAID:WA-OLD", "incoming"), wantPosts: 1},
		{name: "legacy link with account 3", link: fwLink("WA-OLD", 40, 0, 3, "WAID:WA-OLD", "incoming"), wantPosts: 1},
		{name: "config 11 link", link: fwLink("WA-REPLY", 42, 11, 3, "", "outgoing"), wantPosts: 1},
		{name: "legacy link of another account", link: fwLink("WA-OLD", 40, 0, 4, "WAID:WA-OLD", "incoming"), wantPosts: 0},
		{name: "config 11 link of another account", link: fwLink("WA-REPLY", 42, 11, 4, "", "outgoing"), wantPosts: 0},
	}
	const lastSeen = "POST /public/api/v1/inboxes/inbox-a/contacts/" + fwChat + "/conversations/7/update_last_seen"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, client := newFakeChatwootAPI(t, 99)
			stubChatwootResolution(t, client, 11)
			repo := newChatwootLinkMemoryRepo(tc.link)
			payload := map[string]any{"payload": map[string]any{
				"receipt_type": "read",
				"ids":          []any{tc.link.WhatsAppMessageID},
			}}

			if err := syncPayloadToChatwoot(context.Background(), payload, "message.ack", fwDevice, repo); err != nil {
				t.Fatalf("syncPayloadToChatwoot: %v", err)
			}

			want := tc.link
			if tc.wantPosts == 1 {
				want.IsRead = true
				if got := api.recorded(); !slices.Equal(got, []string{lastSeen}) {
					t.Fatalf("Chatwoot requests = %q, want [%q]", got, lastSeen)
				}
			} else if got := api.recorded(); len(got) != 0 {
				t.Fatalf("Chatwoot requests = %q, want none", got)
			}
			if repo.upserts != tc.wantPosts {
				t.Fatalf("upserts = %d, want %d", repo.upserts, tc.wantPosts)
			}
			got := repo.stored(fwDevice, tc.link.WhatsAppMessageID)
			if got == nil || *got != want {
				t.Fatalf("stored link = %+v, want %+v", got, want)
			}
		})
	}
}

// On the forwarder side, revoking a linked message deletes its Chatwoot
// message for links of configuration 0 and 11 alike; the only gate is
// chatwootLinkMatchesClient, which compares accounts and ignores config.
func TestDeleteLinkedChatwootMessageIgnoresLinkConfig(t *testing.T) {
	origDelete := config.ChatwootMessageDelete
	t.Cleanup(func() { config.ChatwootMessageDelete = origDelete })
	config.ChatwootMessageDelete = true

	cases := []struct {
		name      string
		configID  int64
		accountID int
		wantMatch bool
	}{
		{name: "config 0 legacy account", configID: 0, accountID: 0, wantMatch: true},
		{name: "config 0 same account", configID: 0, accountID: 3, wantMatch: true},
		{name: "config 11 same account", configID: 11, accountID: 3, wantMatch: true},
		{name: "config 0 other account", configID: 0, accountID: 4, wantMatch: false},
		{name: "config 11 other account", configID: 11, accountID: 4, wantMatch: false},
	}
	const deleteRequest = "DELETE /api/v1/accounts/3/conversations/7/messages/42"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, client := newFakeChatwootAPI(t, 99)
			link := fwLink("WA-REPLY", 42, tc.configID, tc.accountID, "", "outgoing")
			repo := newChatwootLinkMemoryRepo(link)

			if got := chatwootLinkMatchesClient(&link, client); got != tc.wantMatch {
				t.Fatalf("chatwootLinkMatchesClient = %t, want %t", got, tc.wantMatch)
			}
			deleted := deleteLinkedChatwootMessage(client, fwDevice, repo, "message.revoked", map[string]any{"revoked_message_id": "WA-REPLY"})

			if deleted != tc.wantMatch {
				t.Fatalf("deleteLinkedChatwootMessage = %t, want %t", deleted, tc.wantMatch)
			}
			wantDeletes := 0
			if tc.wantMatch {
				wantDeletes = 1
			}
			if got := api.count(deleteRequest); got != wantDeletes || len(api.recorded()) != wantDeletes {
				t.Fatalf("Chatwoot requests = %q, want %d x %q", api.recorded(), wantDeletes, deleteRequest)
			}
			if got := repo.stored(fwDevice, "WA-REPLY"); got == nil || *got != link || repo.upserts != 0 {
				t.Fatalf("stored link = %+v (upserts %d), want unchanged %+v", got, repo.upserts, link)
			}
		})
	}
}
