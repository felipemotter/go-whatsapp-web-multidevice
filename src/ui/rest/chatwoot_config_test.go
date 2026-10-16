package rest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/chatwoot"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/whatsapp"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/utils"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/ui/rest/middleware"
	"github.com/gofiber/fiber/v3"
)

func TestMaskAPIToken(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"abc":               "****",
		"abcd":              "****",
		"secret-token-1234": "****1234",
	}
	for in, want := range cases {
		if got := maskAPIToken(in); got != want {
			t.Errorf("maskAPIToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeConfigStore is an in-memory IChatStorageRepository covering the methods
// the config handlers use; the rest is the embedded (nil) interface.
type fakeConfigStore struct {
	domainChatStorage.IChatStorageRepository
	configs            map[string]*domainChatStorage.ChatwootDeviceConfig
	linkCount          map[int64]int
	nextID             int64
	deletedLinkConfigs []int64
}

func newFakeConfigStore() *fakeConfigStore {
	return &fakeConfigStore{configs: map[string]*domainChatStorage.ChatwootDeviceConfig{}, linkCount: map[int64]int{}}
}

func (f *fakeConfigStore) SaveChatwootDeviceConfig(cfg *domainChatStorage.ChatwootDeviceConfig) error {
	if cfg.ID == 0 {
		f.nextID++
		cfg.ID = f.nextID
	}
	clone := *cfg
	f.configs[cfg.DeviceID] = &clone
	return nil
}

func (f *fakeConfigStore) GetChatwootDeviceConfig(deviceID string) (*domainChatStorage.ChatwootDeviceConfig, error) {
	if cfg, ok := f.configs[deviceID]; ok {
		clone := *cfg
		return &clone, nil
	}
	return nil, nil
}

func (f *fakeConfigStore) GetChatwootDeviceConfigByIdentifier(identifier string) (*domainChatStorage.ChatwootDeviceConfig, error) {
	for _, cfg := range f.configs {
		if cfg.DeviceID == identifier || (cfg.DeviceJID != "" && cfg.DeviceJID == identifier) {
			clone := *cfg
			return &clone, nil
		}
	}
	return nil, nil
}

func (f *fakeConfigStore) ListChatwootDeviceConfigs() ([]*domainChatStorage.ChatwootDeviceConfig, error) {
	out := make([]*domainChatStorage.ChatwootDeviceConfig, 0, len(f.configs))
	for _, cfg := range f.configs {
		clone := *cfg
		out = append(out, &clone)
	}
	return out, nil
}

func (f *fakeConfigStore) DeleteChatwootDeviceConfig(deviceID string) error {
	delete(f.configs, deviceID)
	return nil
}

func (f *fakeConfigStore) CountChatwootMessageLinksByConfig(configID int64) (int, error) {
	return f.linkCount[configID], nil
}

func (f *fakeConfigStore) DeleteChatwootMessageLinksByConfig(configID int64) error {
	f.deletedLinkConfigs = append(f.deletedLinkConfigs, configID)
	delete(f.linkCount, configID)
	return nil
}

func (f *fakeConfigStore) CountChatwootDeviceConfigs() (int, error) {
	return len(f.configs), nil
}

func newConfigTestApp(t *testing.T, store *fakeConfigStore) *fiber.App {
	t.Helper()
	dm := whatsapp.NewDeviceManager(nil, nil, nil)
	dm.AddDevice(whatsapp.NewDeviceInstance("dev", nil, nil))
	chatwoot.InitClientRegistry(store)
	t.Cleanup(func() { chatwoot.InitClientRegistry(nil) })

	h := &ChatwootHandler{DeviceManager: dm, ChatStorageRepo: store}
	app := fiber.New()
	app.Get("/chatwoot/configs", h.ListChatwootConfigs)
	app.Get("/devices/:device_id/chatwoot/config", h.GetChatwootConfig)
	app.Put("/devices/:device_id/chatwoot/config", h.UpsertChatwootConfig)
	app.Delete("/devices/:device_id/chatwoot/config", h.DeleteChatwootConfig)
	return app
}

func doJSON(t *testing.T, app *fiber.App, method, path, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestChatwootConfigCRUDFlow(t *testing.T) {
	store := newFakeConfigStore()
	app := newConfigTestApp(t, store)

	// Create.
	resp, body := doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10/","account_id":1,"inbox_id":5,"api_token":"super-secret-token"}`)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("create status = %d body=%s", resp.StatusCode, body)
	}
	stored := store.configs["dev"]
	if stored == nil || stored.APIToken != "super-secret-token" {
		t.Fatalf("token not stored raw: %+v", stored)
	}
	if stored.ChatwootURL != "https://203.0.113.10" { // canonicalized (trailing slash removed)
		t.Fatalf("url not canonicalized: %q", stored.ChatwootURL)
	}

	// Read masks the token and never leaks the raw secret.
	resp, body = doJSON(t, app, http.MethodGet, "/devices/dev/chatwoot/config", "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	if strings.Contains(body, "super-secret-token") {
		t.Fatalf("GET leaked raw token: %s", body)
	}
	if !strings.Contains(body, "****oken") {
		t.Fatalf("GET should show masked token, got %s", body)
	}

	// Update with empty token keeps the stored secret.
	resp, body = doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":5,"enabled":false}`)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("update status = %d body=%s", resp.StatusCode, body)
	}
	if store.configs["dev"].APIToken != "super-secret-token" {
		t.Fatal("empty token on update must keep the stored token")
	}
	if store.configs["dev"].Enabled {
		t.Fatal("enabled=false should have been applied")
	}

	// Delete removes the config AND its message links: stale links would keep
	// winning the account-scoped reverse lookup after a delete-and-recreate
	// rebind and hijack reply destinations.
	configID := store.configs["dev"].ID
	resp, _ = doJSON(t, app, http.MethodDelete, "/devices/dev/chatwoot/config", "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if _, ok := store.configs["dev"]; ok {
		t.Fatal("config not deleted")
	}
	if len(store.deletedLinkConfigs) != 1 || store.deletedLinkConfigs[0] != configID {
		t.Fatalf("delete must remove the config's message links, got %v", store.deletedLinkConfigs)
	}
}

// TestChatwootConfigWriteInvalidatesRegistry proves the PR's "registry
// invalidated on write" claim end to end: after a PUT rotates the token (or a
// DELETE removes the config), the registry must hand out a freshly built
// client instead of the cached one.
func TestChatwootConfigWriteInvalidatesRegistry(t *testing.T) {
	store := newFakeConfigStore()
	app := newConfigTestApp(t, store)

	doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":5,"api_token":"token-one"}`)

	reg := chatwoot.GetClientRegistry()
	rc, err := reg.Resolve("dev")
	if err != nil || rc == nil || rc.Client == nil || rc.Client.APIToken != "token-one" {
		t.Fatalf("initial resolve: rc=%+v err=%v", rc, err)
	}

	// Rotate the token via PUT: the cached client must not survive.
	doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":5,"api_token":"token-two"}`)
	rc, err = reg.Resolve("dev")
	if err != nil || rc == nil || rc.Client == nil {
		t.Fatalf("resolve after rotation: rc=%+v err=%v", rc, err)
	}
	if rc.Client.APIToken != "token-two" {
		t.Fatalf("registry served stale client after PUT: token=%q, want token-two", rc.Client.APIToken)
	}

	// DELETE: the per-device client must be evicted (table empty again, so the
	// next resolve falls back to the env config with ConfigID 0).
	doJSON(t, app, http.MethodDelete, "/devices/dev/chatwoot/config", "")
	rc, err = reg.Resolve("dev")
	if err != nil {
		t.Fatalf("resolve after delete: %v", err)
	}
	if rc != nil && rc.ConfigID != 0 {
		t.Fatalf("registry served deleted per-device client: %+v", rc)
	}
}

// TestChatwootConfigListEndpoint smoke-tests GET /chatwoot/configs: it must
// return every config with the token masked.
func TestChatwootConfigListEndpoint(t *testing.T) {
	store := newFakeConfigStore()
	app := newConfigTestApp(t, store)

	doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":5,"api_token":"super-secret-token"}`)

	resp, body := doJSON(t, app, http.MethodGet, "/chatwoot/configs", "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("list status = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"device_id":"dev"`) {
		t.Fatalf("list missing config: %s", body)
	}
	if strings.Contains(body, "super-secret-token") {
		t.Fatalf("list leaked raw token: %s", body)
	}
	if !strings.Contains(body, "****oken") {
		t.Fatalf("list should show masked token: %s", body)
	}
}

// TestChatwootConfigMaskedTokenRoundTrip proves a client that echoes the masked
// token from GET back into PUT keeps the stored secret — the mask must never be
// stored as the credential itself.
func TestChatwootConfigMaskedTokenRoundTrip(t *testing.T) {
	store := newFakeConfigStore()
	app := newConfigTestApp(t, store)

	doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":5,"api_token":"super-secret-token"}`)

	// GET shows "****oken"; a naive client PUTs that value straight back.
	resp, body := doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":5,"api_token":"****oken"}`)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("masked-echo update status = %d body=%s", resp.StatusCode, body)
	}
	if got := store.configs["dev"].APIToken; got != "super-secret-token" {
		t.Fatalf("masked token was stored as the credential: %q", got)
	}
}

func TestChatwootConfigRejectsSSRFURL(t *testing.T) {
	store := newFakeConfigStore()
	app := newConfigTestApp(t, store)
	resp, _ := doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"http://127.0.0.1:3000","account_id":1,"inbox_id":5,"api_token":"t"}`)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("SSRF URL status = %d, want 400", resp.StatusCode)
	}
}

func TestChatwootConfigRejectsRoutingEditWithLinks(t *testing.T) {
	store := newFakeConfigStore()
	app := newConfigTestApp(t, store)

	// Create config (id 1) and pretend it has linked conversations.
	doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":5,"api_token":"t"}`)
	store.linkCount[store.configs["dev"].ID] = 3

	// Changing the inbox (routing identity) must be rejected with 409.
	resp, body := doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":9,"api_token":"t"}`)
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("routing edit status = %d body=%s, want 409", resp.StatusCode, body)
	}

	// Same for the URL and account dimensions of the routing identity.
	resp, body = doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.99","account_id":1,"inbox_id":5,"api_token":"t"}`)
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("url edit status = %d body=%s, want 409", resp.StatusCode, body)
	}
	resp, body = doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":2,"inbox_id":5,"api_token":"t"}`)
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("account edit status = %d body=%s, want 409", resp.StatusCode, body)
	}

	// Rotating only the token (same routing identity) is allowed.
	resp, _ = doJSON(t, app, http.MethodPut, "/devices/dev/chatwoot/config",
		`{"chatwoot_url":"https://203.0.113.10","account_id":1,"inbox_id":5,"api_token":"rotated"}`)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("token rotation status = %d, want 200", resp.StatusCode)
	}
	if store.configs["dev"].APIToken != "rotated" {
		t.Fatal("token should have been rotated")
	}
}

// TestChatwootConfigMultiDeviceNoDeviceMiddlewareLeak verifies that in a multi-device setup
// (where DeviceManager disambiguation fails when no device id is provided),
// Chatwoot config routes registered before headerDeviceGroup are not intercepted
// by DeviceMiddleware, allowing GET /chatwoot/configs and PUT /devices/:device_id/chatwoot/config
// without X-Device-Id header or device_id query param.
func TestChatwootConfigMultiDeviceNoDeviceMiddlewareLeak(t *testing.T) {
	store := newFakeConfigStore()
	dm := whatsapp.NewDeviceManager(nil, nil, nil)
	dm.AddDevice(whatsapp.NewDeviceInstance("dev1", nil, nil))
	dm.AddDevice(whatsapp.NewDeviceInstance("dev2", nil, nil))
	chatwoot.InitClientRegistry(store)
	t.Cleanup(func() { chatwoot.InitClientRegistry(nil) })

	h := &ChatwootHandler{DeviceManager: dm, ChatStorageRepo: store}

	t.Run("routes registered before empty prefix group are not intercepted", func(t *testing.T) {
		app := fiber.New()

		// Registered before headerDeviceGroup so they are not intercepted by DeviceMiddleware
		app.Get("/chatwoot/configs", h.ListChatwootConfigs)
		app.Get("/devices/:device_id/chatwoot/config", h.GetChatwootConfig)
		app.Put("/devices/:device_id/chatwoot/config", h.UpsertChatwootConfig)
		app.Delete("/devices/:device_id/chatwoot/config", h.DeleteChatwootConfig)

		// Device-scoped operations (header-based) with empty prefix group
		headerDeviceGroup := app.Group("", middleware.DeviceMiddleware(dm))
		headerDeviceGroup.Get("/device-scoped-probe", func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		})

		// 1. GET /chatwoot/configs without X-Device-Id responds with OK, NOT DEVICE_ID_REQUIRED
		resp, body := doJSON(t, app, http.MethodGet, "/chatwoot/configs", "")
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("GET /chatwoot/configs status = %d body = %s, want %d", resp.StatusCode, body, fiber.StatusOK)
		}

		// 2. PUT /devices/dev1/chatwoot/config without X-Device-Id responds with OK, NOT DEVICE_ID_REQUIRED
		resp, body = doJSON(t, app, http.MethodPut, "/devices/dev1/chatwoot/config",
			`{"chatwoot_url":"https://203.0.113.10/","account_id":1,"inbox_id":5,"api_token":"super-secret-token"}`)
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("PUT /devices/dev1/chatwoot/config status = %d body = %s, want %d", resp.StatusCode, body, fiber.StatusOK)
		}

		// 3. GET /devices/dev1/chatwoot/config without X-Device-Id responds with OK, NOT DEVICE_ID_REQUIRED
		resp, body = doJSON(t, app, http.MethodGet, "/devices/dev1/chatwoot/config", "")
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("GET /devices/dev1/chatwoot/config status = %d body = %s, want %d", resp.StatusCode, body, fiber.StatusOK)
		}

		// 4. Device-scoped probe without X-Device-Id must be intercepted by DeviceMiddleware and fail with DEVICE_ID_REQUIRED
		resp, body = doJSON(t, app, http.MethodGet, "/device-scoped-probe", "")
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("GET /device-scoped-probe status = %d body = %s, want %d", resp.StatusCode, body, fiber.StatusBadRequest)
		}
		if !strings.Contains(body, "DEVICE_ID_REQUIRED") {
			t.Fatalf("GET /device-scoped-probe body = %s, want DEVICE_ID_REQUIRED", body)
		}
	})

	t.Run("routes registered after empty prefix group mistakenly inherit middleware", func(t *testing.T) {
		app := fiber.New()

		// Buggy ordering: empty-prefix group with DeviceMiddleware registered first
		headerDeviceGroup := app.Group("", middleware.DeviceMiddleware(dm))
		headerDeviceGroup.Get("/device-scoped-probe", func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		})

		// Chatwoot routes registered after: Fiber v3 leaks DeviceMiddleware to subsequent routes
		app.Get("/chatwoot/configs", h.ListChatwootConfigs)

		resp, body := doJSON(t, app, http.MethodGet, "/chatwoot/configs", "")
		if resp.StatusCode != fiber.StatusBadRequest || !strings.Contains(body, "DEVICE_ID_REQUIRED") {
			t.Fatalf("expected leaked middleware to reject with DEVICE_ID_REQUIRED, got status=%d body=%s", resp.StatusCode, body)
		}
	})
}

// assertReplyEnvConfig checks the stored config 11 of slot-a field by field.
func assertReplyEnvConfig(t *testing.T, env *replyEnv, wantURL, wantToken string) {
	t.Helper()
	cfg, err := env.base.GetChatwootDeviceConfig(replyTestSlot)
	if err != nil || cfg == nil {
		t.Fatalf("stored config = %+v, err = %v; want config 11", cfg, err)
	}
	if cfg.ID != 11 || cfg.ChatwootURL != wantURL || cfg.AccountID != 3 || cfg.InboxID != 5 || cfg.APIToken != wantToken || !cfg.Enabled {
		t.Fatalf("stored config = {id %d url %q account %d inbox %d enabled %v}, want {11 %q 3 5 true} with token %q",
			cfg.ID, cfg.ChatwootURL, cfg.AccountID, cfg.InboxID, cfg.Enabled, wantURL, wantToken)
	}
}

// The reconfiguration guard counts the reply link the webhook itself wrote
// under config 11, in real SQLite, with only a legacy link (config 0) beside it
// and no forwarded link. Every PUT that changes URL, account or inbox is a 409
// CONFIG_HAS_LINKED_CONVERSATIONS that keeps the stored values; rotating only
// the token is a 200 that keeps the config id and the reply link.
func TestChatwootConfigRoutingEditBlockedByWebhookReplyLink(t *testing.T) {
	env := newReplyEnv(t, true)
	env.seedLink(legacyConversationLink())
	if reply := env.createReply("/chatwoot/webhook"); reply.ChatwootConfigID != 11 {
		t.Fatalf("reply link = %+v, want config 11", reply)
	}
	if n11, n0 := env.countByConfig(11), env.countByConfig(0); n11 != 1 || n0 != 1 {
		t.Fatalf("Count(11) = %d, Count(0) = %d; want 1 and 1", n11, n0)
	}
	storedURL := env.server.URL
	const path = "/devices/" + replyTestSlot + "/chatwoot/config"

	for _, tc := range []struct {
		name string
		body string
	}{
		{"url", fmt.Sprintf(`{"chatwoot_url":%q,"account_id":3,"inbox_id":5}`, storedURL+"/other")},
		{"account", fmt.Sprintf(`{"chatwoot_url":%q,"account_id":4,"inbox_id":5}`, storedURL)},
		{"inbox", fmt.Sprintf(`{"chatwoot_url":%q,"account_id":3,"inbox_id":6}`, storedURL)},
	} {
		t.Run("changing "+tc.name, func(t *testing.T) {
			status, body := env.request(http.MethodPut, path, tc.body)
			var resp utils.ResponseData
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatalf("decode %q: %v", body, err)
			}
			if status != fiber.StatusConflict || resp.Code != "CONFIG_HAS_LINKED_CONVERSATIONS" {
				t.Fatalf("status = %d code = %q, want 409 CONFIG_HAS_LINKED_CONVERSATIONS", status, resp.Code)
			}
			assertReplyEnvConfig(t, env, storedURL, "token-a")
		})
	}

	t.Run("rotating only the token", func(t *testing.T) {
		body := fmt.Sprintf(`{"chatwoot_url":%q,"account_id":3,"inbox_id":5,"api_token":"token-b"}`, storedURL)
		if status, raw := env.request(http.MethodPut, path, body); status != fiber.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", status, raw)
		}
		assertReplyEnvConfig(t, env, storedURL, "token-b")
		if reply := env.link("WA-REPLY"); reply == nil || reply.ChatwootConfigID != 11 {
			t.Fatalf("reply link after rotation = %+v, want config 11", reply)
		}
		if n := env.countByConfig(11); n != 1 {
			t.Fatalf("Count(11) after rotation = %d, want 1", n)
		}
	})
}

// DELETE of config 11 removes the links it owns, the forwarded link and the
// webhook-written reply link, and keeps the legacy link (config 0) and a control
// link of config 22. The conversation lookup filtered by 11 then finds nothing,
// and the by-config delete refuses config 0 without touching the legacy link.
func TestChatwootConfigDeleteRemovesForwarderAndReplyLinks(t *testing.T) {
	env := newReplyEnv(t, true)
	const controlDevice = "device-b@s.whatsapp.net"
	env.insertDeviceConfig(22, "slot-b", controlDevice, 3, 6, true)
	env.seedLink(legacyConversationLink())
	env.seedLink(forwardedConversationLink())
	env.seedLink(domainChatStorage.ChatwootMessageLink{DeviceID: controlDevice, WhatsAppMessageID: "WA-CTRL",
		WhatsAppChatJID: "peer-b@s.whatsapp.net", ChatwootMessageID: 43, ChatwootConversationID: 8, ChatwootInboxID: 6, ChatwootConfigID: 22})
	// The legacy link is the latest of the conversation, so the route reaches
	// delivery with config 0: the reply link only gets config 11 if the scope
	// is completed there.
	env.setLinkTimes("WA-FWD", linkFixtureAt(0), linkFixtureAt(0))
	env.setLinkTimes("WA-OLD", linkFixtureAt(1), linkFixtureAt(1))
	if reply := env.createReply("/chatwoot/webhook"); reply.ChatwootConfigID != 11 {
		t.Fatalf("reply link = %+v, want config 11", reply)
	}
	if n := env.countByConfig(11); n != 2 {
		t.Fatalf("Count(11) before delete = %d, want 2 (forwarded and reply links)", n)
	}

	if status, raw := env.request(http.MethodDelete, "/devices/"+replyTestSlot+"/chatwoot/config", ""); status != fiber.StatusOK {
		t.Fatalf("delete status = %d body = %s, want 200", status, raw)
	}
	if cfg, err := env.base.GetChatwootDeviceConfig(replyTestSlot); err != nil || cfg != nil {
		t.Fatalf("config after delete = %+v, err = %v; want none", cfg, err)
	}
	if cfg, err := env.base.GetChatwootDeviceConfig("slot-b"); err != nil || cfg == nil || cfg.ID != 22 {
		t.Fatalf("control config = %+v, err = %v; want config 22", cfg, err)
	}
	for _, waID := range []string{"WA-FWD", "WA-REPLY"} {
		if link := env.link(waID); link != nil {
			t.Fatalf("link %s survived the config delete: %+v", waID, link)
		}
	}
	if n := env.countByConfig(11); n != 0 {
		t.Fatalf("Count(11) after delete = %d, want 0", n)
	}
	if old := env.link("WA-OLD"); old == nil || old.ChatwootConfigID != 0 {
		t.Fatalf("old link = %+v, want kept with config 0", old)
	}
	control, err := env.base.GetChatwootMessageLinkByWhatsAppID(controlDevice, "WA-CTRL")
	if err != nil || control == nil || control.ChatwootConfigID != 22 {
		t.Fatalf("control link = %+v, err = %v; want kept with config 22", control, err)
	}
	if link, err := env.base.GetLatestChatwootMessageLinkByConversation(7, 3, false, 11); err != nil || link != nil {
		t.Fatalf("conversation lookup in config 11 = %+v, err = %v; want nil", link, err)
	}

	if err := env.base.DeleteChatwootMessageLinksByConfig(0); err == nil {
		t.Fatal("DeleteChatwootMessageLinksByConfig(0) = nil, want an error")
	}
	if old := env.link("WA-OLD"); old == nil || old.ChatwootConfigID != 0 {
		t.Fatalf("old link after config-0 delete = %+v, want kept", old)
	}
}
