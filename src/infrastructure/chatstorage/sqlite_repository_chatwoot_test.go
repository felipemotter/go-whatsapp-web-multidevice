package chatstorage

import (
	"testing"
	"time"

	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
)

func TestSQLiteRepositoryStoresAndLooksUpChatwootMessageLinks(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	now := time.Date(2026, time.June, 6, 10, 0, 0, 0, time.UTC)

	link := &domainChatStorage.ChatwootMessageLink{
		DeviceID:                     "device-a@s.whatsapp.net",
		WhatsAppMessageID:            "wa-1",
		WhatsAppChatJID:              "628123456789@s.whatsapp.net",
		ChatwootMessageID:            101,
		ChatwootConversationID:       202,
		ChatwootInboxID:              303,
		ChatwootContactInboxSourceID: "628123456789@s.whatsapp.net",
		SourceID:                     "WAID:wa-1",
		Direction:                    "incoming",
		IsRead:                       false,
		CreatedAt:                    now,
		UpdatedAt:                    now,
	}

	if err := repo.UpsertChatwootMessageLink(link); err != nil {
		t.Fatalf("upsert link: %v", err)
	}

	byWA, err := repo.GetChatwootMessageLinkByWhatsAppID(link.DeviceID, link.WhatsAppMessageID)
	if err != nil {
		t.Fatalf("lookup by whatsapp id: %v", err)
	}
	if byWA == nil {
		t.Fatal("expected link by whatsapp id")
	}
	if byWA.ChatwootMessageID != 101 || byWA.ChatwootConversationID != 202 || byWA.SourceID != "WAID:wa-1" {
		t.Fatalf("unexpected link by whatsapp id: %+v", byWA)
	}

	byCW, err := repo.GetChatwootMessageLinkByChatwootID(link.DeviceID, link.ChatwootMessageID)
	if err != nil {
		t.Fatalf("lookup by chatwoot id: %v", err)
	}
	if byCW == nil {
		t.Fatal("expected link by chatwoot id")
	}
	if byCW.WhatsAppMessageID != "wa-1" || byCW.WhatsAppChatJID != link.WhatsAppChatJID {
		t.Fatalf("unexpected link by chatwoot id: %+v", byCW)
	}

	link.ChatwootConversationID = 404
	link.IsRead = true
	if err := repo.UpsertChatwootMessageLink(link); err != nil {
		t.Fatalf("update link: %v", err)
	}

	updated, err := repo.GetChatwootMessageLinkByWhatsAppID(link.DeviceID, link.WhatsAppMessageID)
	if err != nil {
		t.Fatalf("lookup updated link: %v", err)
	}
	if updated.ChatwootConversationID != 404 || !updated.IsRead {
		t.Fatalf("expected updated link, got %+v", updated)
	}
}

func TestSQLiteRepositoryChatwootMessageLinksAreDeviceScoped(t *testing.T) {
	repo := newTestSQLiteRepository(t)

	if err := repo.UpsertChatwootMessageLink(&domainChatStorage.ChatwootMessageLink{
		DeviceID:          "device-a@s.whatsapp.net",
		WhatsAppMessageID: "same-wa-id",
		WhatsAppChatJID:   "628111111111@s.whatsapp.net",
		ChatwootMessageID: 10,
		SourceID:          "WAID:same-wa-id",
		Direction:         "incoming",
	}); err != nil {
		t.Fatalf("upsert device A link: %v", err)
	}
	if err := repo.UpsertChatwootMessageLink(&domainChatStorage.ChatwootMessageLink{
		DeviceID:          "device-b@s.whatsapp.net",
		WhatsAppMessageID: "same-wa-id",
		WhatsAppChatJID:   "628222222222@s.whatsapp.net",
		ChatwootMessageID: 20,
		SourceID:          "WAID:same-wa-id",
		Direction:         "incoming",
	}); err != nil {
		t.Fatalf("upsert device B link: %v", err)
	}

	linkA, err := repo.GetChatwootMessageLinkByWhatsAppID("device-a@s.whatsapp.net", "same-wa-id")
	if err != nil {
		t.Fatalf("lookup device A: %v", err)
	}
	linkB, err := repo.GetChatwootMessageLinkByWhatsAppID("device-b@s.whatsapp.net", "same-wa-id")
	if err != nil {
		t.Fatalf("lookup device B: %v", err)
	}

	if linkA.ChatwootMessageID != 10 {
		t.Fatalf("device A chatwoot id = %d, want 10", linkA.ChatwootMessageID)
	}
	if linkB.ChatwootMessageID != 20 {
		t.Fatalf("device B chatwoot id = %d, want 20", linkB.ChatwootMessageID)
	}

	if err := repo.DeleteDeviceData("device-a@s.whatsapp.net"); err != nil {
		t.Fatalf("delete device A data: %v", err)
	}
	linkA, err = repo.GetChatwootMessageLinkByWhatsAppID("device-a@s.whatsapp.net", "same-wa-id")
	if err != nil {
		t.Fatalf("lookup deleted device A: %v", err)
	}
	if linkA != nil {
		t.Fatalf("expected device A link to be deleted, got %+v", linkA)
	}

	linkB, err = repo.GetChatwootMessageLinkByWhatsAppID("device-b@s.whatsapp.net", "same-wa-id")
	if err != nil {
		t.Fatalf("lookup remaining device B: %v", err)
	}
	if linkB == nil || linkB.ChatwootMessageID != 20 {
		t.Fatalf("expected device B link to remain, got %+v", linkB)
	}
}

func TestSQLiteRepositoryGetsLatestUnreadIncomingChatwootMessageLinkByChat(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	deviceID := "device-a@s.whatsapp.net"
	chatJID := "628123456789@s.whatsapp.net"

	links := []*domainChatStorage.ChatwootMessageLink{
		{
			DeviceID:          deviceID,
			WhatsAppMessageID: "old-unread",
			WhatsAppChatJID:   chatJID,
			ChatwootMessageID: 11,
			Direction:         "incoming",
			IsRead:            false,
			UpdatedAt:         time.Date(2026, time.June, 6, 9, 0, 0, 0, time.UTC),
		},
		{
			DeviceID:          deviceID,
			WhatsAppMessageID: "latest-unread",
			WhatsAppChatJID:   chatJID,
			ChatwootMessageID: 12,
			Direction:         "incoming",
			IsRead:            false,
			UpdatedAt:         time.Date(2026, time.June, 6, 10, 0, 0, 0, time.UTC),
		},
		{
			DeviceID:          deviceID,
			WhatsAppMessageID: "outgoing",
			WhatsAppChatJID:   chatJID,
			ChatwootMessageID: 13,
			Direction:         "outgoing",
			IsRead:            false,
			UpdatedAt:         time.Date(2026, time.June, 6, 11, 0, 0, 0, time.UTC),
		},
		{
			DeviceID:          deviceID,
			WhatsAppMessageID: "read",
			WhatsAppChatJID:   chatJID,
			ChatwootMessageID: 14,
			Direction:         "incoming",
			IsRead:            true,
			UpdatedAt:         time.Date(2026, time.June, 6, 12, 0, 0, 0, time.UTC),
		},
	}
	for _, link := range links {
		if err := repo.UpsertChatwootMessageLink(link); err != nil {
			t.Fatalf("upsert %s: %v", link.WhatsAppMessageID, err)
		}
	}

	link, err := repo.GetLatestUnreadChatwootMessageLinkByChat(deviceID, chatJID)
	if err != nil {
		t.Fatalf("lookup latest unread: %v", err)
	}
	if link == nil || link.WhatsAppMessageID != "latest-unread" {
		t.Fatalf("latest unread = %+v, want latest-unread", link)
	}

	link.IsRead = true
	if err := repo.UpsertChatwootMessageLink(link); err != nil {
		t.Fatalf("mark latest read: %v", err)
	}
	next, err := repo.GetLatestUnreadChatwootMessageLinkByChat(deviceID, chatJID)
	if err != nil {
		t.Fatalf("lookup next unread: %v", err)
	}
	if next == nil || next.WhatsAppMessageID != "old-unread" {
		t.Fatalf("next unread = %+v, want old-unread", next)
	}
}

func TestSQLiteRepositoryGetsLatestChatwootMessageLinkByConversation(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	conversationID := 202

	links := []*domainChatStorage.ChatwootMessageLink{
		{
			DeviceID:               "device-a@s.whatsapp.net",
			WhatsAppMessageID:      "old",
			WhatsAppChatJID:        "628111111111@s.whatsapp.net",
			ChatwootMessageID:      11,
			ChatwootConversationID: conversationID,
			UpdatedAt:              time.Date(2026, time.June, 6, 9, 0, 0, 0, time.UTC),
		},
		{
			DeviceID:               "device-b@s.whatsapp.net",
			WhatsAppMessageID:      "latest",
			WhatsAppChatJID:        "628222222222@s.whatsapp.net",
			ChatwootMessageID:      12,
			ChatwootConversationID: conversationID,
			UpdatedAt:              time.Date(2026, time.June, 6, 10, 0, 0, 0, time.UTC),
		},
		{
			DeviceID:               "device-c@s.whatsapp.net",
			WhatsAppMessageID:      "other-conversation",
			WhatsAppChatJID:        "628333333333@s.whatsapp.net",
			ChatwootMessageID:      13,
			ChatwootConversationID: 303,
			UpdatedAt:              time.Date(2026, time.June, 6, 11, 0, 0, 0, time.UTC),
		},
	}
	for _, link := range links {
		if err := repo.UpsertChatwootMessageLink(link); err != nil {
			t.Fatalf("upsert %s: %v", link.WhatsAppMessageID, err)
		}
	}

	link, err := repo.GetLatestChatwootMessageLinkByConversation(conversationID, 0, true, 0)
	if err != nil {
		t.Fatalf("lookup latest conversation link: %v", err)
	}
	if link == nil || link.WhatsAppMessageID != "latest" || link.DeviceID != "device-b@s.whatsapp.net" {
		t.Fatalf("latest conversation link = %+v, want device-b/latest", link)
	}
}

func TestSQLiteRepositoryConversationLookupExcludesLegacyZeroInPerDeviceMode(t *testing.T) {
	repo := newTestSQLiteRepository(t)

	// A pre-migration legacy link (account id 0) for conversation 9. Chatwoot
	// numbers conversations per account, so account 7 can also have a conversation
	// 9 — the legacy row must not be allowed to satisfy that account's reply.
	legacy := &domainChatStorage.ChatwootMessageLink{
		DeviceID:               "legacy-dev",
		WhatsAppMessageID:      "legacy-1",
		WhatsAppChatJID:        "628000000000@s.whatsapp.net",
		ChatwootConversationID: 9,
		ChatwootAccountID:      0,
		Direction:              "incoming",
	}
	if err := repo.UpsertChatwootMessageLink(legacy); err != nil {
		t.Fatalf("upsert legacy link: %v", err)
	}

	// Per-device mode: account 7 webhook must NOT match the account-0 legacy row.
	got, err := repo.GetLatestChatwootMessageLinkByConversation(9, 7, false, 0)
	if err != nil {
		t.Fatalf("scoped lookup: %v", err)
	}
	if got != nil {
		t.Fatalf("per-device lookup must not match legacy account-0 link, got %+v", got)
	}

	// Legacy mode: the account-0 wildcard still resolves the historical link.
	got, err = repo.GetLatestChatwootMessageLinkByConversation(9, 7, true, 0)
	if err != nil {
		t.Fatalf("legacy lookup: %v", err)
	}
	if got == nil || got.DeviceID != "legacy-dev" {
		t.Fatalf("legacy-mode lookup should match account-0 link, got %+v", got)
	}
}

// The startup backfill stamps the env account onto account-0 rows only. It
// never reclassifies their config (0 stays 0, 11 stays 11), never touches
// timestamps, and leaves rows of another account alone.
func TestSQLiteRepositoryBackfillChatwootMessageLinkAccount(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	base := time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)

	legacyZero := replyLinkFixture("WA-ACCT0-CFG0", base)
	legacyZero.ChatwootAccountID = 0
	legacyZero.ChatwootConfigID = 0
	positiveConfig := replyLinkFixture("WA-ACCT0-CFG11", base.Add(time.Minute))
	positiveConfig.ChatwootAccountID = 0
	otherAccount := replyLinkFixture("WA-ACCT4", base.Add(2*time.Minute))
	otherAccount.ChatwootAccountID = 4
	otherAccount.ChatwootConfigID = 22
	upsertReplyLinks(t, repo, legacyZero, positiveConfig, otherAccount)

	load := func(waMessageID string) *domainChatStorage.ChatwootMessageLink {
		t.Helper()
		link, err := repo.GetChatwootMessageLinkByWhatsAppID("device-a@s.whatsapp.net", waMessageID)
		if err != nil || link == nil {
			t.Fatalf("load %s: %+v, %v", waMessageID, link, err)
		}
		return link
	}
	ids := []string{"WA-ACCT0-CFG0", "WA-ACCT0-CFG11", "WA-ACCT4"}
	before := map[string]*domainChatStorage.ChatwootMessageLink{}
	for _, id := range ids {
		before[id] = load(id)
	}

	if n, err := repo.BackfillChatwootMessageLinkAccount(0); err != nil || n != 0 {
		t.Fatalf("backfill(0) = %d, %v; want 0, nil", n, err)
	}

	n, err := repo.BackfillChatwootMessageLinkAccount(3)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Fatalf("backfill rows affected = %d, want 2 (only the account-0 rows)", n)
	}

	want := map[string]struct {
		account int
		config  int64
	}{
		"WA-ACCT0-CFG0":  {3, 0},
		"WA-ACCT0-CFG11": {3, 11},
		"WA-ACCT4":       {4, 22},
	}
	for _, id := range ids {
		got := load(id)
		if got.ChatwootAccountID != want[id].account || got.ChatwootConfigID != want[id].config {
			t.Fatalf("%s account/config = %d/%d, want %d/%d", id,
				got.ChatwootAccountID, got.ChatwootConfigID, want[id].account, want[id].config)
		}
		if !got.CreatedAt.Equal(before[id].CreatedAt) || !got.UpdatedAt.Equal(before[id].UpdatedAt) {
			t.Fatalf("%s timestamps changed: created %v -> %v, updated %v -> %v", id,
				before[id].CreatedAt, got.CreatedAt, before[id].UpdatedAt, got.UpdatedAt)
		}
	}

	// Idempotent: a second run finds nothing left at 0.
	n, err = repo.BackfillChatwootMessageLinkAccount(3)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if n != 0 {
		t.Fatalf("second backfill rows affected = %d, want 0", n)
	}
}

func TestSQLiteRepositoryChatwootForwardQueueLifecycle(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	now := time.Date(2026, time.June, 6, 10, 0, 0, 0, time.UTC)

	err := repo.EnqueueChatwootForwardEvent(&domainChatStorage.ChatwootForwardEvent{
		DeviceID:          "device-a@s.whatsapp.net",
		EventName:         "message",
		WhatsAppMessageID: "wa-queue-1",
		PayloadJSON:       `{"payload":{"id":"wa-queue-1"}}`,
		LastError:         "chatwoot down",
		NextAttemptAt:     now,
	})
	if err != nil {
		t.Fatalf("enqueue retry: %v", err)
	}

	due, err := repo.ListDueChatwootForwardEvents(now.Add(-time.Second), 10)
	if err != nil {
		t.Fatalf("list early due events: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("early due len = %d, want 0", len(due))
	}

	due, err = repo.ListDueChatwootForwardEvents(now, 10)
	if err != nil {
		t.Fatalf("list due events: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due len = %d, want 1", len(due))
	}
	if due[0].DeviceID != "device-a@s.whatsapp.net" || due[0].WhatsAppMessageID != "wa-queue-1" || due[0].Attempts != 0 {
		t.Fatalf("unexpected due event: %+v", due[0])
	}

	nextAttempt := now.Add(2 * time.Minute)
	if err := repo.MarkChatwootForwardEventFailed(due[0].ID, "still down", nextAttempt); err != nil {
		t.Fatalf("mark retry failed: %v", err)
	}

	due, err = repo.ListDueChatwootForwardEvents(nextAttempt.Add(-time.Second), 10)
	if err != nil {
		t.Fatalf("list before next attempt: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due before next len = %d, want 0", len(due))
	}

	due, err = repo.ListDueChatwootForwardEvents(nextAttempt, 10)
	if err != nil {
		t.Fatalf("list next due events: %v", err)
	}
	if len(due) != 1 || due[0].Attempts != 1 || due[0].LastError != "still down" {
		t.Fatalf("unexpected retried event: %+v", due)
	}

	if err := repo.MarkChatwootForwardEventDone(due[0].ID); err != nil {
		t.Fatalf("mark retry done: %v", err)
	}
	due, err = repo.ListDueChatwootForwardEvents(nextAttempt.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("list after done: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due after done len = %d, want 0", len(due))
	}
}

// replyLinkFixture is a link inside the synthetic reply scope: device-a,
// conversation 7, account 3, inbox 5, config 11, quoted Chatwoot message 41.
func replyLinkFixture(waMessageID string, createdAt time.Time) *domainChatStorage.ChatwootMessageLink {
	return &domainChatStorage.ChatwootMessageLink{
		DeviceID:               "device-a@s.whatsapp.net",
		WhatsAppMessageID:      waMessageID,
		WhatsAppChatJID:        "peer-a@s.whatsapp.net",
		ChatwootMessageID:      41,
		ChatwootConversationID: 7,
		ChatwootInboxID:        5,
		Direction:              "outgoing",
		CreatedAt:              createdAt,
		ChatwootConfigID:       11,
		ChatwootAccountID:      3,
	}
}

func replyLinkFilter() domainChatStorage.ChatwootReplyLinkFilter {
	return domainChatStorage.ChatwootReplyLinkFilter{
		DeviceID:               "device-a@s.whatsapp.net",
		ChatwootMessageID:      41,
		ChatwootConversationID: 7,
		ChatwootAccountID:      3,
		ChatwootInboxID:        5,
		ChatwootConfigID:       11,
	}
}

func upsertReplyLinks(t *testing.T, repo *SQLiteRepository, links ...*domainChatStorage.ChatwootMessageLink) {
	t.Helper()
	for _, link := range links {
		if err := repo.UpsertChatwootMessageLink(link); err != nil {
			t.Fatalf("upsert %s: %v", link.WhatsAppMessageID, err)
		}
	}
}

func replyLinkWAID(t *testing.T, repo *SQLiteRepository, filter domainChatStorage.ChatwootReplyLinkFilter) string {
	t.Helper()
	link, err := repo.GetChatwootMessageLinkForReply(filter)
	if err != nil {
		t.Fatalf("GetChatwootMessageLinkForReply(%+v): %v", filter, err)
	}
	if link == nil {
		return ""
	}
	return link.WhatsAppMessageID
}

// The scope is applied before LIMIT 1. Every competitor is older and sorts
// first by wa_message_id, so it would win if its differing predicate were
// dropped or if a row were picked before its scope was checked.
func TestSQLiteRepositoryReplyLinkLookupScopesBeforeLimit(t *testing.T) {
	base := time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)

	t.Run("legacy config zero inserted before current config", func(t *testing.T) {
		repo := newTestSQLiteRepository(t)
		legacy := replyLinkFixture("WA-A-LEGACY", base)
		legacy.ChatwootConfigID = 0
		upsertReplyLinks(t, repo, legacy, replyLinkFixture("WA-CURRENT", base.Add(time.Minute)))

		if got := replyLinkWAID(t, repo, replyLinkFilter()); got != "WA-CURRENT" {
			t.Fatalf("reply link = %q, want WA-CURRENT", got)
		}

		if _, err := repo.db.Exec(`DELETE FROM chatwoot_message_links WHERE wa_message_id = ?`, "WA-CURRENT"); err != nil {
			t.Fatalf("delete current link: %v", err)
		}
		if got := replyLinkWAID(t, repo, replyLinkFilter()); got != "" {
			t.Fatalf("reply link after removing current = %q, want nil", got)
		}
	})

	competitors := []struct {
		name   string
		mutate func(*domainChatStorage.ChatwootMessageLink)
	}{
		{"device", func(l *domainChatStorage.ChatwootMessageLink) { l.DeviceID = "device-b@s.whatsapp.net" }},
		{"message", func(l *domainChatStorage.ChatwootMessageLink) { l.ChatwootMessageID = 40 }},
		{"conversation", func(l *domainChatStorage.ChatwootMessageLink) { l.ChatwootConversationID = 8 }},
		{"account", func(l *domainChatStorage.ChatwootMessageLink) { l.ChatwootAccountID = 4 }},
		{"inbox", func(l *domainChatStorage.ChatwootMessageLink) { l.ChatwootInboxID = 6 }},
		{"config", func(l *domainChatStorage.ChatwootMessageLink) { l.ChatwootConfigID = 22 }},
	}
	for _, tc := range competitors {
		t.Run("competitor differs in "+tc.name, func(t *testing.T) {
			repo := newTestSQLiteRepository(t)
			competitor := replyLinkFixture("WA-A-OTHER", base)
			tc.mutate(competitor)
			upsertReplyLinks(t, repo, competitor, replyLinkFixture("WA-CURRENT", base.Add(time.Minute)))

			if got := replyLinkWAID(t, repo, replyLinkFilter()); got != "WA-CURRENT" {
				t.Fatalf("reply link = %q, want WA-CURRENT", got)
			}

			if _, err := repo.db.Exec(`DELETE FROM chatwoot_message_links WHERE wa_message_id = ?`, "WA-CURRENT"); err != nil {
				t.Fatalf("delete current link: %v", err)
			}
			if got := replyLinkWAID(t, repo, replyLinkFilter()); got != "" {
				t.Fatalf("reply link with only the %s competitor = %q, want nil", tc.name, got)
			}
		})
	}
}

// Account 0 is a legacy row, eligible only in confirmed legacy mode
// (config 0 plus the explicit flag); config 0 is equality, never a wildcard.
func TestSQLiteRepositoryReplyLinkLookupRestrictsLegacyZero(t *testing.T) {
	base := time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)
	legacyFilter := func(allow bool) domainChatStorage.ChatwootReplyLinkFilter {
		filter := replyLinkFilter()
		filter.ChatwootConfigID = 0
		filter.AllowLegacyAccountZero = allow
		return filter
	}

	t.Run("legacy account zero needs the legacy flag", func(t *testing.T) {
		repo := newTestSQLiteRepository(t)
		legacy := replyLinkFixture("WA-LEGACY", base)
		legacy.ChatwootConfigID = 0
		legacy.ChatwootAccountID = 0
		upsertReplyLinks(t, repo, legacy)

		if got := replyLinkWAID(t, repo, legacyFilter(true)); got != "WA-LEGACY" {
			t.Fatalf("legacy lookup with flag = %q, want WA-LEGACY", got)
		}
		if got := replyLinkWAID(t, repo, legacyFilter(false)); got != "" {
			t.Fatalf("legacy lookup without flag = %q, want nil", got)
		}
	})

	t.Run("config zero of another account never matches", func(t *testing.T) {
		repo := newTestSQLiteRepository(t)
		otherAccount := replyLinkFixture("WA-ACCOUNT-4", base)
		otherAccount.ChatwootConfigID = 0
		otherAccount.ChatwootAccountID = 4
		upsertReplyLinks(t, repo, otherAccount)

		if got := replyLinkWAID(t, repo, legacyFilter(true)); got != "" {
			t.Fatalf("legacy lookup with flag = %q, want nil", got)
		}
		if got := replyLinkWAID(t, repo, legacyFilter(false)); got != "" {
			t.Fatalf("legacy lookup without flag = %q, want nil", got)
		}
	})

	t.Run("account zero never matches a positive config even with the flag", func(t *testing.T) {
		repo := newTestSQLiteRepository(t)
		zeroAccount := replyLinkFixture("WA-ZERO-ACCOUNT", base)
		zeroAccount.ChatwootAccountID = 0
		upsertReplyLinks(t, repo, zeroAccount)

		filter := replyLinkFilter()
		filter.AllowLegacyAccountZero = true
		if got := replyLinkWAID(t, repo, filter); got != "" {
			t.Fatalf("config 11 lookup with flag = %q, want nil", got)
		}
	})

	t.Run("config zero never selects a positive config", func(t *testing.T) {
		repo := newTestSQLiteRepository(t)
		upsertReplyLinks(t, repo, replyLinkFixture("WA-CURRENT", base))

		if got := replyLinkWAID(t, repo, legacyFilter(true)); got != "" {
			t.Fatalf("config 0 lookup = %q, want nil", got)
		}
		if got := replyLinkWAID(t, repo, legacyFilter(false)); got != "" {
			t.Fatalf("config 0 lookup without flag = %q, want nil", got)
		}
	})

	t.Run("exact account wins over an older legacy row", func(t *testing.T) {
		repo := newTestSQLiteRepository(t)
		legacy := replyLinkFixture("WA-A-LEGACY", base)
		legacy.ChatwootConfigID = 0
		legacy.ChatwootAccountID = 0
		exact := replyLinkFixture("WA-EXACT", base.Add(time.Minute))
		exact.ChatwootConfigID = 0
		upsertReplyLinks(t, repo, legacy, exact)

		if got := replyLinkWAID(t, repo, legacyFilter(true)); got != "WA-EXACT" {
			t.Fatalf("legacy-mode lookup = %q, want WA-EXACT", got)
		}
	})
}

// The representative of a multi-attachment message is stable: oldest
// created_at, then lowest wa_message_id; updated_at never decides.
func TestSQLiteRepositoryReplyLinkLookupPicksStableRepresentative(t *testing.T) {
	base := time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)

	for _, order := range []string{"oldest first", "newest first"} {
		t.Run(order, func(t *testing.T) {
			repo := newTestSQLiteRepository(t)
			oldest := replyLinkFixture("WA-Z-OLDEST", base)
			newest := replyLinkFixture("WA-A-NEWEST", base.Add(time.Minute))
			if order == "oldest first" {
				upsertReplyLinks(t, repo, oldest, newest)
			} else {
				upsertReplyLinks(t, repo, newest, oldest)
			}
			if got := replyLinkWAID(t, repo, replyLinkFilter()); got != "WA-Z-OLDEST" {
				t.Fatalf("reply link = %q, want WA-Z-OLDEST", got)
			}
		})
	}

	t.Run("created_at tie picks lowest wa_message_id", func(t *testing.T) {
		repo := newTestSQLiteRepository(t)
		upsertReplyLinks(t, repo, replyLinkFixture("WA-B", base), replyLinkFixture("WA-A", base))
		if got := replyLinkWAID(t, repo, replyLinkFilter()); got != "WA-A" {
			t.Fatalf("reply link = %q, want WA-A", got)
		}
	})

	t.Run("updated_at does not change the representative", func(t *testing.T) {
		repo := newTestSQLiteRepository(t)
		upsertReplyLinks(t, repo, replyLinkFixture("WA-OLDEST", base), replyLinkFixture("WA-NEWER", base.Add(time.Minute)))

		if _, err := repo.db.Exec(`UPDATE chatwoot_message_links SET updated_at = ? WHERE wa_message_id = ?`,
			base.Add(-time.Hour), "WA-NEWER"); err != nil {
			t.Fatalf("age newer link updated_at: %v", err)
		}
		if got := replyLinkWAID(t, repo, replyLinkFilter()); got != "WA-OLDEST" {
			t.Fatalf("after aging updated_at: reply link = %q, want WA-OLDEST", got)
		}

		if _, err := repo.db.Exec(`UPDATE chatwoot_message_links SET updated_at = ? WHERE wa_message_id = ?`,
			base.Add(-2*time.Hour), "WA-OLDEST"); err != nil {
			t.Fatalf("age oldest link updated_at: %v", err)
		}
		newer, err := repo.GetChatwootMessageLinkByWhatsAppID("device-a@s.whatsapp.net", "WA-NEWER")
		if err != nil || newer == nil {
			t.Fatalf("load newer link: %+v, %v", newer, err)
		}
		newer.IsRead = true
		if err := repo.UpsertChatwootMessageLink(newer); err != nil {
			t.Fatalf("touch newer link: %v", err)
		}
		if got := replyLinkWAID(t, repo, replyLinkFilter()); got != "WA-OLDEST" {
			t.Fatalf("after touching newer link: reply link = %q, want WA-OLDEST", got)
		}
	})
}

// GetChatwootMessageLinkForReply refuses an incomplete scope instead of
// running an unscoped query.
func TestSQLiteRepositoryReplyLinkLookupRequiresScope(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	upsertReplyLinks(t, repo, replyLinkFixture("WA-CURRENT", time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)))

	cases := map[string]func(*domainChatStorage.ChatwootReplyLinkFilter){
		"device":       func(f *domainChatStorage.ChatwootReplyLinkFilter) { f.DeviceID = " " },
		"message":      func(f *domainChatStorage.ChatwootReplyLinkFilter) { f.ChatwootMessageID = 0 },
		"conversation": func(f *domainChatStorage.ChatwootReplyLinkFilter) { f.ChatwootConversationID = 0 },
		"account":      func(f *domainChatStorage.ChatwootReplyLinkFilter) { f.ChatwootAccountID = 0 },
		"inbox":        func(f *domainChatStorage.ChatwootReplyLinkFilter) { f.ChatwootInboxID = 0 },
	}
	for name, mutate := range cases {
		filter := replyLinkFilter()
		mutate(&filter)
		link, err := repo.GetChatwootMessageLinkForReply(filter)
		if err == nil || link != nil {
			t.Fatalf("missing %s: link=%+v err=%v, want nil link and an error", name, link, err)
		}
	}
}

// coexistingLinks are the links that share conversation 7 (device-a, peer-a,
// account 3, inbox 5): an old legacy link (config 0), the current forwarded
// link and the delivered reply link (both config 11).
func coexistingLinks() []*domainChatStorage.ChatwootMessageLink {
	base := time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)
	legacyLink := replyLinkFixture("WA-OLD", base)
	legacyLink.ChatwootMessageID = 40
	legacyLink.ChatwootConfigID = 0
	legacyLink.Direction = "incoming"
	legacyLink.SourceID = "WAID:WA-OLD"
	forwardedLink := replyLinkFixture("WA-FWD", base.Add(time.Minute))
	forwardedLink.ChatwootMessageID = 41
	forwardedLink.Direction = "incoming"
	forwardedLink.SourceID = "WAID:WA-FWD"
	replyLink := replyLinkFixture("WA-REPLY", base.Add(2*time.Minute))
	replyLink.ChatwootMessageID = 42
	return []*domainChatStorage.ChatwootMessageLink{legacyLink, forwardedLink, replyLink}
}

// The reply lookup treats config as a mandatory equality. The reply link
// resolves only in scope 11; config 0 never reaches it and config 11 never
// reaches the legacy link.
func TestSQLiteRepositoryReplyLinkLookupConfigEquality(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	upsertReplyLinks(t, repo, coexistingLinks()...)

	cases := []struct {
		name      string
		configID  int64
		messageID int
		want      string
	}{
		{"config 11 resolves the reply link", 11, 42, "WA-REPLY"},
		{"config 11 resolves the forwarded link", 11, 41, "WA-FWD"},
		{"config 11 does not reach the legacy link", 11, 40, ""},
		{"config 0 does not reach the reply link", 0, 42, ""},
		{"config 0 resolves the legacy link by equality", 0, 40, "WA-OLD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := replyLinkFilter()
			filter.ChatwootConfigID = tc.configID
			filter.ChatwootMessageID = tc.messageID
			link, err := repo.GetChatwootMessageLinkForReply(filter)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if tc.want == "" {
				if link != nil {
					t.Fatalf("link = %+v, want nil", link)
				}
				return
			}
			if link == nil || link.WhatsAppMessageID != tc.want || link.ChatwootConfigID != tc.configID ||
				link.ChatwootMessageID != tc.messageID || link.WhatsAppChatJID != "peer-a@s.whatsapp.net" {
				t.Fatalf("link = %+v, want %s (config %d, message %d)", link, tc.want, tc.configID, tc.messageID)
			}
		})
	}
}
