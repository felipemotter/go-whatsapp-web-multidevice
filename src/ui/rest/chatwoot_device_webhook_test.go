package rest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/config"
	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/chatwoot"
	"github.com/gofiber/fiber/v3"
)

// deviceWebhookTestRepo satisfies the few repo methods the ClientRegistry and
// route resolver call; the rest of the interface is the embedded (nil) value.
type deviceWebhookTestRepo struct {
	domainChatStorage.IChatStorageRepository
	cfg   *domainChatStorage.ChatwootDeviceConfig
	count int
}

func (r *deviceWebhookTestRepo) GetChatwootDeviceConfigByIdentifier(identifier string) (*domainChatStorage.ChatwootDeviceConfig, error) {
	if r.cfg != nil && (identifier == r.cfg.DeviceID || identifier == r.cfg.DeviceJID) {
		return r.cfg, nil
	}
	return nil, nil
}

func (r *deviceWebhookTestRepo) GetChatwootDeviceConfigByInbox(accountID, inboxID int) (*domainChatStorage.ChatwootDeviceConfig, error) {
	return nil, nil
}

func (r *deviceWebhookTestRepo) GetLatestChatwootMessageLinkByConversation(conversationID, accountID int, allowLegacyZero bool, configID int64) (*domainChatStorage.ChatwootMessageLink, error) {
	return nil, nil
}

func (r *deviceWebhookTestRepo) CountChatwootDeviceConfigs() (int, error) {
	return r.count, nil
}

// TestHandleDeviceWebhookValidatesAccountInbox proves the per-device endpoint is
// route-by-config: a payload whose account/inbox does not match the device's
// configured account/inbox is rejected (401), while a matching payload passes
// the gate (here a non-action event, so it returns 200 without sending).
func TestHandleDeviceWebhookValidatesAccountInbox(t *testing.T) {
	repo := &deviceWebhookTestRepo{
		cfg:   &domainChatStorage.ChatwootDeviceConfig{ID: 1, DeviceID: "d", ChatwootURL: "https://chat.example.com", AccountID: 1, InboxID: 2, APIToken: "t", Enabled: true},
		count: 1,
	}
	chatwoot.InitClientRegistry(repo)
	t.Cleanup(func() { chatwoot.InitClientRegistry(nil) })

	handler := &ChatwootHandler{ChatStorageRepo: repo}
	app := fiber.New()
	app.Post("/chatwoot/webhook/:device_id", handler.HandleDeviceWebhook)

	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/chatwoot/webhook/d", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		return resp.StatusCode
	}

	// Mismatched account -> rejected.
	if got := post(`{"event":"message_created","message_type":"outgoing","account":{"id":999},"conversation":{"id":5,"inbox_id":2}}`); got != fiber.StatusUnauthorized {
		t.Fatalf("account mismatch status = %d, want 401", got)
	}
	// Mismatched inbox -> rejected.
	if got := post(`{"event":"message_created","message_type":"outgoing","account":{"id":1},"conversation":{"id":5,"inbox_id":77}}`); got != fiber.StatusUnauthorized {
		t.Fatalf("inbox mismatch status = %d, want 401", got)
	}
	// Matching account+inbox passes the gate (incoming message -> 200, no send).
	if got := post(`{"event":"message_created","message_type":"incoming","account":{"id":1},"conversation":{"id":5,"inbox_id":2}}`); got != fiber.StatusOK {
		t.Fatalf("matching status = %d, want 200", got)
	}
	// Non-message events lack the fields the gate checks; they must be
	// acknowledged, not 401-ed, even when account/inbox don't match.
	if got := post(`{"event":"conversation_updated","account":{"id":999}}`); got != fiber.StatusOK {
		t.Fatalf("non-message event status = %d, want 200", got)
	}
}

// TestProcessChatwootWebhookDropsUnroutablePayload proves the fail-fast is
// enforced at delivery: in per-device mode an unmapped conversation must be
// acknowledged WITHOUT resolving a device — an empty DeviceID would otherwise
// fall through to the default device and send from the wrong WhatsApp account.
func TestProcessChatwootWebhookDropsUnroutablePayload(t *testing.T) {
	chatwoot.InitClientRegistry(nil)
	t.Cleanup(func() { chatwoot.InitClientRegistry(nil) })

	// Non-empty config table, no link/attr/inbox mapping for the payload.
	handler := &ChatwootHandler{ChatStorageRepo: &deviceWebhookTestRepo{count: 2}}
	route := handler.resolveChatwootWebhookRoute(chatwoot.WebhookPayload{
		Account:      chatwoot.Account{ID: 1},
		Conversation: chatwoot.ConversationWebhook{ID: 5, InboxID: 9},
	}, nil)
	if !route.Unroutable {
		t.Fatalf("unmapped payload in per-device mode must be marked unroutable, got %+v", route)
	}

	// End-to-end: the webhook handler must return 200 without touching the
	// device manager (nil here — a delivery attempt would resolve the default
	// device and panic on the nil usecases before responding).
	app := fiber.New()
	app.Post("/chatwoot/webhook", handler.HandleWebhook)
	body := `{"event":"message_created","message_type":"outgoing","account":{"id":1},"conversation":{"id":5,"inbox_id":9,"meta":{"sender":{"phone_number":"+628999999999"}}},"content":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/chatwoot/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("unroutable payload status = %d, want 200", resp.StatusCode)
	}

	// Legacy mode stays deliverable: the same payload resolves the env device.
	prev := config.ChatwootDeviceID
	config.ChatwootDeviceID = "env-default-device"
	t.Cleanup(func() { config.ChatwootDeviceID = prev })
	legacy := &ChatwootHandler{ChatStorageRepo: &deviceWebhookTestRepo{count: 0}}
	route = legacy.resolveChatwootWebhookRoute(chatwoot.WebhookPayload{
		Account:      chatwoot.Account{ID: 1},
		Conversation: chatwoot.ConversationWebhook{ID: 5, InboxID: 9},
	}, nil)
	if route.Unroutable {
		t.Fatalf("legacy mode must not mark payloads unroutable, got %+v", route)
	}
}

// TestResolveChatwootWebhookRouteFailsFastWhenConfigsExist proves that once any
// per-device config exists, an unmapped conversation does NOT fall back to the
// env ChatwootDeviceID (which would mis-deliver across inboxes).
func TestResolveChatwootWebhookRouteFailsFastWhenConfigsExist(t *testing.T) {
	prev := config.ChatwootDeviceID
	config.ChatwootDeviceID = "env-default-device"
	t.Cleanup(func() { config.ChatwootDeviceID = prev })
	chatwoot.InitClientRegistry(nil)
	t.Cleanup(func() { chatwoot.InitClientRegistry(nil) })

	// Repo: no conversation link, but the config table is non-empty.
	handler := &ChatwootHandler{ChatStorageRepo: &deviceWebhookTestRepo{count: 2}}

	route := handler.resolveChatwootWebhookRoute(chatwoot.WebhookPayload{
		Account:      chatwoot.Account{ID: 1},
		Conversation: chatwoot.ConversationWebhook{ID: 5, InboxID: 9},
	}, nil)
	if route.DeviceID != "" {
		t.Fatalf("expected fail-fast (empty device), got %q", route.DeviceID)
	}

	// Sanity: with an empty config table, env fallback applies.
	handlerLegacy := &ChatwootHandler{ChatStorageRepo: &deviceWebhookTestRepo{count: 0}}
	route = handlerLegacy.resolveChatwootWebhookRoute(chatwoot.WebhookPayload{
		Account:      chatwoot.Account{ID: 1},
		Conversation: chatwoot.ConversationWebhook{ID: 5, InboxID: 9},
	}, nil)
	if route.DeviceID != "env-default-device" {
		t.Fatalf("legacy mode should use env device, got %q", route.DeviceID)
	}
}

// A per-device reply routed by the gowa_device_id override, or by an older
// config-0 link that is still the latest in the conversation, records the
// device's confirmed config (11) on its new link. The old link stays config 0
// and is never quoted as if it were config 11; the new link can be quoted.
func TestChatwootWebhookCompletesConfigScopeOnPerDeviceRoutes(t *testing.T) {
	t.Run("contact override without a previous link", func(t *testing.T) {
		env := newReplyEnv(t, true)
		override := withContactAttr("gowa_device_id", replyTestSlot)

		if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 100, override)); got != fiber.StatusOK {
			t.Fatalf("first POST status = %d, want 200", got)
		}
		reply := env.link("WA-SENT-1")
		if reply == nil || reply.ChatwootConfigID != 11 || reply.ChatwootAccountID != 3 || reply.ChatwootInboxID != 5 || reply.ChatwootConversationID != 7 {
			t.Fatalf("override link = %+v, want config 11 / account 3 / inbox 5 / conversation 7", reply)
		}

		if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 101, override, withQuotedID(100))); got != fiber.StatusOK {
			t.Fatalf("second POST status = %d, want 200", got)
		}
		calls := env.sender.snapshot()
		if len(calls) != 2 || calls[0].Reply != nil || replyValue(calls[1].Reply) != "WA-SENT-1" {
			t.Fatalf("calls = %+v, want [plain, quoting WA-SENT-1]", calls)
		}
	})

	t.Run("older config-0 link returns the route early", func(t *testing.T) {
		env := newReplyEnv(t, true)
		env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-OLD", ChatwootMessageID: 40, ChatwootConfigID: 0})

		if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 102)); got != fiber.StatusOK {
			t.Fatalf("first POST status = %d, want 200", got)
		}
		reply := env.link("WA-SENT-1")
		if reply == nil || reply.ChatwootConfigID != 11 || reply.ChatwootAccountID != 3 || reply.ChatwootInboxID != 5 {
			t.Fatalf("new link = %+v, want config 11 / account 3 / inbox 5", reply)
		}
		if old := env.link("WA-OLD"); old == nil || old.ChatwootConfigID != 0 {
			t.Fatalf("old link = %+v, want unchanged config 0", old)
		}

		if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 103, withQuotedID(40))); got != fiber.StatusOK {
			t.Fatalf("quote-old POST status = %d, want 200", got)
		}
		if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 104, withQuotedID(102))); got != fiber.StatusOK {
			t.Fatalf("quote-new POST status = %d, want 200", got)
		}
		calls := env.sender.snapshot()
		if len(calls) != 3 || calls[0].Reply != nil || calls[1].Reply != nil || replyValue(calls[2].Reply) != "WA-SENT-1" {
			t.Fatalf("calls = %+v, want [plain, plain (old link not quoted), quoting WA-SENT-1]", calls)
		}
		for _, lookup := range env.repo.lookups() {
			if lookup.ChatwootConfigID != 11 || lookup.AllowLegacyAccountZero {
				t.Fatalf("lookup = %+v, want config 11 without legacy flag", lookup)
			}
		}
		if old := env.link("WA-OLD"); old == nil || old.ChatwootConfigID != 0 {
			t.Fatalf("old link after replies = %+v, want config 0", old)
		}
	})
}

// In per-device mode an unconfirmable scope (config bound to another account
// or inbox, disabled config, or a failing config count) neither enables the
// legacy lookup nor writes a config-0 link; the reply is still delivered. On
// the per-device endpoint a foreign account/inbox is rejected before any send
// or lookup, and a valid payload uses the forced config.
func TestChatwootWebhookUnconfirmedScopeAndDeviceEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *replyEnv)
		quoted int
		links  int // seeded links, none written by the reply
	}{
		{"config bound to another inbox", func(env *replyEnv) { env.insertConfig(11, 3, 99, true) }, 40, 1},
		// The quoted message 41 has a config-11 link of account 4: confirming
		// the config without checking its account would look it up and quote it.
		{"config bound to another account", func(env *replyEnv) {
			env.insertConfig(11, 4, 5, true)
			env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ACCOUNT-4", ChatwootMessageID: 41,
				ChatwootConfigID: 11, ChatwootAccountID: 4})
		}, 41, 2},
		{"config disabled", func(env *replyEnv) { env.insertConfig(11, 3, 5, false) }, 40, 1},
		{"config count fails", func(env *replyEnv) { env.repo.countErr = errors.New("database is locked") }, 40, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReplyEnv(t, false)
			// The config-0 link routes the reply to device-a / peer-a.
			env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-OLD", ChatwootMessageID: 40, ChatwootConfigID: 0})
			tc.setup(env)

			if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 110, withQuotedID(tc.quoted))); got != fiber.StatusOK {
				t.Fatalf("status = %d, want 200", got)
			}
			calls := env.sender.snapshot()
			if len(calls) != 1 || calls[0].Phone != "peer-a" || calls[0].Reply != nil || calls[0].Device != env.instance {
				t.Fatalf("calls = %+v, want one plain send from slot-a to peer-a", calls)
			}
			if n := len(env.repo.lookups()); n != 0 {
				t.Fatalf("lookups = %d, want 0", n)
			}
			if n := len(env.repo.writes()); n != 0 {
				t.Fatalf("link writes = %d, want 0", n)
			}
			if n := env.linkCount(); n != tc.links {
				t.Fatalf("links = %d, want only the %d seeded", n, tc.links)
			}
		})
	}

	t.Run("per-device endpoint rejects a foreign account or inbox", func(t *testing.T) {
		env := newReplyEnv(t, true)
		env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ORIG", ChatwootMessageID: 41, ChatwootConfigID: 11})
		for _, body := range []string{
			replyWebhookBody(t, 111, withQuotedID(41), withAccountInbox(4, 5)),
			replyWebhookBody(t, 112, withQuotedID(41), withAccountInbox(3, 6)),
		} {
			if got := env.post("/chatwoot/webhook/"+replyTestSlot, body); got != fiber.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", got)
			}
		}
		if calls := env.sender.snapshot(); len(calls) != 0 {
			t.Fatalf("calls = %+v, want none", calls)
		}
		if n := len(env.repo.lookups()); n != 0 {
			t.Fatalf("lookups = %d, want 0", n)
		}
	})

	t.Run("per-device endpoint uses the forced config", func(t *testing.T) {
		env := newReplyEnv(t, true)
		env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ORIG", ChatwootMessageID: 41, ChatwootConfigID: 11})

		if got := env.post("/chatwoot/webhook/"+replyTestSlot, replyWebhookBody(t, 113, withQuotedID(41))); got != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
		calls := env.sender.snapshot()
		if len(calls) != 1 || replyValue(calls[0].Reply) != "WA-ORIG" {
			t.Fatalf("calls = %+v, want one send quoting WA-ORIG", calls)
		}
		lookups := env.repo.lookups()
		if len(lookups) != 1 || lookups[0].ChatwootConfigID != 11 {
			t.Fatalf("lookups = %+v, want one in config 11", lookups)
		}
		if link := env.link("WA-SENT-1"); link == nil || link.ChatwootConfigID != 11 {
			t.Fatalf("new link = %+v, want config 11", link)
		}
	})
}

// --- links that coexist in one conversation ----------------------------------
//
// Links of one conversation (conversation 7, account 3, inbox 5):
//
//	legacy link     WA-OLD    Chatwoot 40  config 0
//	forwarded link  WA-FWD    Chatwoot 41  config 11
//	reply link      WA-REPLY  Chatwoot 42  config 11 (written by the webhook)

var (
	linkFixtureBase = time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC)
	linkFixtureAt   = func(hours int) time.Time { return linkFixtureBase.Add(time.Duration(hours) * time.Hour) }
)

func legacyConversationLink() domainChatStorage.ChatwootMessageLink {
	return domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-OLD", ChatwootMessageID: 40, ChatwootConfigID: 0, CreatedAt: linkFixtureAt(0)}
}

func forwardedConversationLink() domainChatStorage.ChatwootMessageLink {
	return domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-FWD", ChatwootMessageID: 41, ChatwootConfigID: 11,
		Direction: "incoming", SourceID: "WAID:WA-FWD", CreatedAt: linkFixtureAt(0)}
}

// conversationRoutePayload is the conversation-7 payload the route resolver sees.
func conversationRoutePayload() chatwoot.WebhookPayload {
	return chatwoot.WebhookPayload{
		Account: chatwoot.Account{ID: 3},
		Conversation: chatwoot.ConversationWebhook{ID: 7, InboxID: 5, Meta: chatwoot.ConversationMeta{
			Sender: chatwoot.Contact{CustomAttributes: map[string]any{"gowa_whatsapp_jid": replyTestChatJID}},
		}},
	}
}

// withoutTimes drops the wall-clock timestamps so a stored link can be compared
// field by field.
func withoutTimes(link domainChatStorage.ChatwootMessageLink) domainChatStorage.ChatwootMessageLink {
	link.CreatedAt, link.UpdatedAt = time.Time{}, time.Time{}
	return link
}

// The link of a delivered reply records the confirmed scope. On per-device
// routes that reach delivery with config 0 (the gowa_device_id override, or an
// old config-0 link that returns the route early) the reply link gets config 11
// and the legacy link stays 0; in confirmed legacy mode the reply link gets 0. The unknown-scope branch (no outbound
// link, same send and status) is TestChatwootWebhookUnconfirmedScopeAndDeviceEndpoint.
func TestChatwootWebhookReplyLinkRecordsConfirmedScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		perDevice   bool
		oldLink     bool
		override    bool
		routeDevice string
		wantConfig  int64
	}{
		{"per-device first reply via gowa_device_id", true, false, true, replyTestSlot, 11},
		{"per-device route returned by old config-0 link", true, true, false, replyTestDeviceJID, 11},
		{"confirmed legacy with old link", false, true, false, replyTestDeviceJID, 0},
		{"confirmed legacy without previous link", false, false, false, replyTestSlot, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReplyEnv(t, tc.perDevice)
			if tc.oldLink {
				env.seedLink(legacyConversationLink())
			}
			var opts []replyBodyOpt
			payload := conversationRoutePayload()
			if tc.override {
				opts = append(opts, withContactAttr("gowa_device_id", replyTestSlot))
				payload.Conversation.Meta.Sender.CustomAttributes["gowa_device_id"] = replyTestSlot
			}

			// The route itself reaches delivery with config 0: the scope is
			// completed afterwards, without changing the device.
			route := env.handler.resolveChatwootWebhookRoute(payload, nil)
			if route.DeviceID != tc.routeDevice || route.ConfigID != 0 || route.Unroutable {
				t.Fatalf("route = %+v, want device %s with config 0", route, tc.routeDevice)
			}

			reply := env.createReply("/chatwoot/webhook", opts...)
			want := domainChatStorage.ChatwootMessageLink{
				DeviceID: replyTestDeviceJID, WhatsAppMessageID: "WA-REPLY", WhatsAppChatJID: replyTestChatJID,
				ChatwootMessageID: 42, ChatwootConversationID: 7, ChatwootInboxID: 5,
				ChatwootContactInboxSourceID: replyTestChatJID, Direction: "outgoing", IsRead: true,
				ChatwootConfigID: tc.wantConfig, ChatwootAccountID: 3,
			}
			if got := withoutTimes(*reply); got != want {
				t.Fatalf("reply link = %+v\nwant %+v", got, want)
			}
			if tc.oldLink {
				if old := env.link("WA-OLD"); old == nil || old.ChatwootConfigID != 0 || old.ChatwootMessageID != 40 {
					t.Fatalf("old link = %+v, want unchanged config 0", old)
				}
			}
			calls := env.sender.snapshot()
			if len(calls) != 1 || calls[0].Phone != "peer-a" || calls[0].Device != env.instance {
				t.Fatalf("calls = %+v, want one send from slot-a to peer-a", calls)
			}
		})
	}
}

// Once the scope is confirmed, the reply link matches the existing conversation
// route under config 11. With the reply link the latest the route carries
// config 11; when a read update makes the legacy link the latest again the route
// carries config 0, yet the delivery confirms 11, quotes the reply link and
// writes its new link with 11 while the legacy link stays 0. On the forced
// per-device route (config 11) a newer config-22 link never wins, while the
// shared lookup keeps config 0 as "no config filter".
func TestChatwootWebhookConversationRouteAfterConfirmedReply(t *testing.T) {
	setup := func(t *testing.T) *replyEnv {
		t.Helper()
		env := newReplyEnv(t, true)
		env.seedLink(legacyConversationLink())
		if reply := env.createReply("/chatwoot/webhook"); reply.ChatwootConfigID != 11 {
			t.Fatalf("reply link = %+v, want config 11", reply)
		}
		env.setLinkTimes("WA-OLD", linkFixtureAt(0), linkFixtureAt(0))
		env.setLinkTimes("WA-REPLY", linkFixtureAt(1), linkFixtureAt(1))
		return env
	}

	for _, tc := range []struct {
		name            string
		oldUpdated      time.Time
		wantRouteConfig int64
	}{
		{"reply link is the latest", linkFixtureAt(0), 11},
		{"old config-0 link is the latest again", linkFixtureAt(2), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setup(t)
			env.setLinkTimes("WA-OLD", linkFixtureAt(0), tc.oldUpdated)

			route := env.handler.resolveChatwootWebhookRoute(conversationRoutePayload(), nil)
			if route.DeviceID != replyTestDeviceJID || route.Destination != replyTestChatJID ||
				route.ConfigID != tc.wantRouteConfig || route.AccountID != 3 || route.Unroutable {
				t.Fatalf("route = %+v, want device %s, chat %s, config %d, account 3",
					route, replyTestDeviceJID, replyTestChatJID, tc.wantRouteConfig)
			}

			if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 43, withQuotedID(42))); got != fiber.StatusOK {
				t.Fatalf("quoting POST status = %d, want 200", got)
			}
			calls := env.sender.snapshot()
			if len(calls) != 2 || calls[1].Method != "SendText" || replyValue(calls[1].Reply) != "WA-REPLY" ||
				calls[1].Phone != "peer-a" || calls[1].Device != env.instance {
				t.Fatalf("calls = %+v, want the second from slot-a to peer-a quoting WA-REPLY", calls)
			}
			wantLookup := domainChatStorage.ChatwootReplyLinkFilter{
				DeviceID: replyTestDeviceJID, ChatwootMessageID: 42, ChatwootConversationID: 7,
				ChatwootAccountID: 3, ChatwootInboxID: 5, ChatwootConfigID: 11,
			}
			if lookups := env.repo.lookups(); len(lookups) != 1 || lookups[0] != wantLookup {
				t.Fatalf("lookups = %+v, want [%+v]", lookups, wantLookup)
			}
			next := env.link("WA-SENT-2")
			if next == nil || next.ChatwootMessageID != 43 || next.ChatwootConfigID != 11 || next.ChatwootAccountID != 3 ||
				next.ChatwootInboxID != 5 || next.ChatwootConversationID != 7 {
				t.Fatalf("new link = %+v, want Chatwoot 43 / config 11 / account 3 / inbox 5 / conversation 7", next)
			}
			if old := env.link("WA-OLD"); old == nil || old.ChatwootConfigID != 0 {
				t.Fatalf("old link = %+v, want config 0", old)
			}
		})
	}

	t.Run("forced config 11 ignores a newer config-22 link", func(t *testing.T) {
		env := setup(t)
		env.seedLink(domainChatStorage.ChatwootMessageLink{DeviceID: "device-b@s.whatsapp.net", WhatsAppMessageID: "WA-CFG22",
			WhatsAppChatJID: "peer-b@s.whatsapp.net", ChatwootMessageID: 46, ChatwootConfigID: 22})
		env.setLinkTimes("WA-CFG22", linkFixtureAt(3), linkFixtureAt(3))

		forced := &chatwootWebhookRoute{DeviceID: replyTestSlot, ConfigID: 11, AccountID: 3, InboxID: 5}
		route := env.handler.resolveChatwootWebhookRoute(conversationRoutePayload(), forced)
		if route.DeviceID != replyTestDeviceJID || route.Destination != replyTestChatJID || route.ConfigID != 11 {
			t.Fatalf("forced route = %+v, want the reply link's (device-a, peer-a, config 11)", route)
		}
		shared := env.handler.resolveChatwootWebhookRoute(conversationRoutePayload(), nil)
		if shared.Destination != "peer-b@s.whatsapp.net" || shared.ConfigID != 22 {
			t.Fatalf("shared route = %+v, want the newest link (config 0 disables the config filter)", shared)
		}

		if got := env.post("/chatwoot/webhook/"+replyTestSlot, replyWebhookBody(t, 43, withQuotedID(42))); got != fiber.StatusOK {
			t.Fatalf("per-device POST status = %d, want 200", got)
		}
		calls := env.sender.snapshot()
		if len(calls) != 2 || calls[1].Phone != "peer-a" || replyValue(calls[1].Reply) != "WA-REPLY" || calls[1].Device != env.instance {
			t.Fatalf("calls = %+v, want the second from slot-a to peer-a quoting WA-REPLY", calls)
		}
	})
}

// On the webhook side, marking the latest inbound message read rewrites the whole link,
// so the config it was stored with (0 or 11) and its CreatedAt survive, with
// exactly one MarkAsRead for the expected id and chat from the routed device.
func TestChatwootWebhookMarkAsReadPreservesLinkConfig(t *testing.T) {
	for _, tc := range []struct {
		name      string
		perDevice bool
		configID  int64
	}{
		{"legacy config 0", false, 0},
		{"per-device config 0", true, 0},
		{"per-device config 11", true, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReplyEnv(t, tc.perDevice)
			config.ChatwootMessageRead = true // restored by newReplyEnv
			messages := &replyMessageStub{}
			env.handler.MessageUsecase = messages
			env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-IN", ChatwootMessageID: 39,
				Direction: "incoming", SourceID: "WAID:WA-IN", ChatwootConfigID: tc.configID, CreatedAt: linkFixtureAt(0)})
			before := env.link("WA-IN")

			if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 42)); got != fiber.StatusOK {
				t.Fatalf("status = %d, want 200", got)
			}
			calls := messages.snapshot()
			want := replyMessageCall{Method: "MarkAsRead", Device: env.instance, MessageID: "WA-IN", Phone: replyTestChatJID}
			if len(calls) != 1 || calls[0] != want {
				t.Fatalf("message calls = %+v, want [%+v]", calls, want)
			}
			after := env.link("WA-IN")
			if after == nil || !after.IsRead || after.ChatwootConfigID != tc.configID || !after.CreatedAt.Equal(linkFixtureAt(0)) {
				t.Fatalf("read link = %+v, want is_read, config %d, created_at %v", after, tc.configID, linkFixtureAt(0))
			}
			wantAfter := withoutTimes(*before)
			wantAfter.IsRead = true
			if got := withoutTimes(*after); got != wantAfter {
				t.Fatalf("read link = %+v\nwant %+v (only is_read changed)", got, wantAfter)
			}
		})
	}
}

// On the webhook side, deleting an agent reply in Chatwoot revokes its WhatsApp
// message whatever config the reply link carries; the revoke lookup does not
// filter by config, so a confirmed config-11 link and a config-0 link are both
// revoked once, in
// the expected chat, from the routed device.
func TestChatwootWebhookDeleteRevokesReplyOfAnyConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		perDevice  bool
		seedZero   bool // reply link left over with config 0 instead of written by the webhook
		path       string
		wantConfig int64
	}{
		{"legacy reply link", false, false, "/chatwoot/webhook", 0},
		{"per-device reply link", true, false, "/chatwoot/webhook", 11},
		{"per-device reply link on the device endpoint", true, false, "/chatwoot/webhook/" + replyTestSlot, 11},
		{"per-device leftover config-0 reply link", true, true, "/chatwoot/webhook", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReplyEnv(t, tc.perDevice)
			prevDelete := config.ChatwootMessageDelete
			config.ChatwootMessageDelete = true
			t.Cleanup(func() { config.ChatwootMessageDelete = prevDelete })
			messages := &replyMessageStub{}
			env.handler.MessageUsecase = messages

			if tc.seedZero {
				env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-REPLY", ChatwootMessageID: 42, ChatwootConfigID: 0})
			} else {
				env.createReply("/chatwoot/webhook")
			}
			if reply := env.link("WA-REPLY"); reply == nil || reply.ChatwootConfigID != tc.wantConfig {
				t.Fatalf("reply link = %+v, want config %d", reply, tc.wantConfig)
			}
			sendsBefore := len(env.sender.snapshot())

			deleted := replyWebhookBody(t, 42, withReplyField("event", "message_updated"), withReplyAttrs(map[string]any{"deleted": true}))
			if got := env.post(tc.path, deleted); got != fiber.StatusOK {
				t.Fatalf("delete status = %d, want 200", got)
			}
			want := replyMessageCall{Method: "RevokeMessage", Device: env.instance, MessageID: "WA-REPLY", Phone: replyTestChatJID}
			if calls := messages.snapshot(); len(calls) != 1 || calls[0] != want {
				t.Fatalf("message calls = %+v, want [%+v]", calls, want)
			}
			if n := len(env.sender.snapshot()); n != sendsBefore {
				t.Fatalf("sends after delete = %d, want %d", n, sendsBefore)
			}
			if env.noteCount() != 0 {
				t.Fatalf("failure notes = %d, want 0", env.noteCount())
			}
		})
	}
}
