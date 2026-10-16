package rest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/config"
	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
	domainMessage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/message"
	domainSend "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/send"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/chatstorage"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/chatwoot"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/whatsapp"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/sqlite"
	"github.com/gofiber/fiber/v3"
	"github.com/sirupsen/logrus"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// Synthetic fixtures only: device-a / slot-a, peer-a, group-a, account 3,
// inbox 5, conversation 7, config 11, quoted Chatwoot message 41.
const (
	replyTestDeviceJID = "device-a@s.whatsapp.net"
	replyTestSlot      = "slot-a"
	replyTestChatJID   = "peer-a@s.whatsapp.net"
	replyTestLID       = "peer-a@lid"
	replyTestGroupJID  = "group-a@g.us"
	replyTestDataURL   = "https://files.example.test/attachment.bin"
)

func replyValue(id *string) string {
	if id == nil {
		return "<nil>"
	}
	return *id
}

// --- send usecase stub ---------------------------------------------------

type replySendCall struct {
	Method string
	Device *whatsapp.DeviceInstance
	Phone  string
	Text   string // message for text, caption for media
	URL    string
	PTT    bool
	Reply  *string
}

type replySendStub struct {
	domainSend.ISendUsecase
	mu       sync.Mutex
	calls    []replySendCall
	failCall map[int]error    // 1-based call index
	failKind map[string]error // method name
	ids      []string         // WhatsApp ids for the first successful sends
	sent     int
}

func (s *replySendStub) record(ctx context.Context, call replySendCall) (domainSend.GenericResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst, ok := whatsapp.DeviceFromContext(ctx); ok {
		call.Device = inst
	}
	if call.Reply != nil {
		copied := *call.Reply
		call.Reply = &copied
	}
	s.calls = append(s.calls, call)
	n := len(s.calls)
	if err := s.failCall[n]; err != nil {
		return domainSend.GenericResponse{MessageID: fmt.Sprintf("WA-FAILED-%d", n)}, err
	}
	if err := s.failKind[call.Method]; err != nil {
		return domainSend.GenericResponse{MessageID: fmt.Sprintf("WA-FAILED-%d", n)}, err
	}
	s.sent++
	if s.sent <= len(s.ids) {
		return domainSend.GenericResponse{MessageID: s.ids[s.sent-1]}, nil
	}
	return domainSend.GenericResponse{MessageID: fmt.Sprintf("WA-SENT-%d", s.sent)}, nil
}

// --- message usecase stub ------------------------------------------------

type replyMessageCall struct {
	Method    string
	Device    *whatsapp.DeviceInstance
	MessageID string
	Phone     string
}

// replyMessageStub records the read-receipt and revoke calls the webhook makes.
type replyMessageStub struct {
	domainMessage.IMessageUsecase
	mu    sync.Mutex
	calls []replyMessageCall
}

func (s *replyMessageStub) record(ctx context.Context, call replyMessageCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst, ok := whatsapp.DeviceFromContext(ctx); ok {
		call.Device = inst
	}
	s.calls = append(s.calls, call)
}

func (s *replyMessageStub) MarkAsRead(ctx context.Context, r domainMessage.MarkAsReadRequest) (domainMessage.GenericResponse, error) {
	s.record(ctx, replyMessageCall{Method: "MarkAsRead", MessageID: r.MessageID, Phone: r.Phone})
	return domainMessage.GenericResponse{MessageID: r.MessageID}, nil
}

func (s *replyMessageStub) RevokeMessage(ctx context.Context, r domainMessage.RevokeRequest) (domainMessage.GenericResponse, error) {
	s.record(ctx, replyMessageCall{Method: "RevokeMessage", MessageID: r.MessageID, Phone: r.Phone})
	return domainMessage.GenericResponse{MessageID: r.MessageID}, nil
}

func (s *replyMessageStub) snapshot() []replyMessageCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]replyMessageCall(nil), s.calls...)
}

func derefURL(u *string) string {
	if u == nil {
		return ""
	}
	return *u
}

func (s *replySendStub) SendText(ctx context.Context, r domainSend.MessageRequest) (domainSend.GenericResponse, error) {
	return s.record(ctx, replySendCall{Method: "SendText", Phone: r.Phone, Text: r.Message, Reply: r.ReplyMessageID})
}

func (s *replySendStub) SendImage(ctx context.Context, r domainSend.ImageRequest) (domainSend.GenericResponse, error) {
	return s.record(ctx, replySendCall{Method: "SendImage", Phone: r.Phone, Text: r.Caption, URL: derefURL(r.ImageURL), Reply: r.ReplyMessageID})
}

func (s *replySendStub) SendAudio(ctx context.Context, r domainSend.AudioRequest) (domainSend.GenericResponse, error) {
	return s.record(ctx, replySendCall{Method: "SendAudio", Phone: r.Phone, URL: derefURL(r.AudioURL), PTT: r.PTT, Reply: r.ReplyMessageID})
}

func (s *replySendStub) SendVideo(ctx context.Context, r domainSend.VideoRequest) (domainSend.GenericResponse, error) {
	return s.record(ctx, replySendCall{Method: "SendVideo", Phone: r.Phone, Text: r.Caption, URL: derefURL(r.VideoURL), Reply: r.ReplyMessageID})
}

func (s *replySendStub) SendFile(ctx context.Context, r domainSend.FileRequest) (domainSend.GenericResponse, error) {
	return s.record(ctx, replySendCall{Method: "SendFile", Phone: r.Phone, Text: r.Caption, URL: derefURL(r.FileURL), Reply: r.ReplyMessageID})
}

func (s *replySendStub) snapshot() []replySendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]replySendCall(nil), s.calls...)
}

// --- repositories --------------------------------------------------------

// replyCountingRepo wraps the real SQLite repository, counting quoted-link
// lookups and link writes, with optional injected failures.
type replyCountingRepo struct {
	domainChatStorage.IChatStorageRepository
	mu             sync.Mutex
	replyFilters   []domainChatStorage.ChatwootReplyLinkFilter
	upserts        []domainChatStorage.ChatwootMessageLink
	messageLookups []string
	configLookups  int
	replyErr       error
	upsertErr      error
	countErr       error
	messageErr     error
	configErr      error
}

func (r *replyCountingRepo) GetMessageByIDAndDevice(deviceID, id string) (*domainChatStorage.Message, error) {
	r.mu.Lock()
	r.messageLookups = append(r.messageLookups, deviceID+" "+id)
	err := r.messageErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.IChatStorageRepository.GetMessageByIDAndDevice(deviceID, id)
}

func (r *replyCountingRepo) GetChatwootDeviceConfigByIdentifier(identifier string) (*domainChatStorage.ChatwootDeviceConfig, error) {
	r.mu.Lock()
	r.configLookups++
	err := r.configErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.IChatStorageRepository.GetChatwootDeviceConfigByIdentifier(identifier)
}

func (r *replyCountingRepo) GetChatwootMessageLinkForReply(filter domainChatStorage.ChatwootReplyLinkFilter) (*domainChatStorage.ChatwootMessageLink, error) {
	r.mu.Lock()
	r.replyFilters = append(r.replyFilters, filter)
	err := r.replyErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.IChatStorageRepository.GetChatwootMessageLinkForReply(filter)
}

func (r *replyCountingRepo) UpsertChatwootMessageLink(link *domainChatStorage.ChatwootMessageLink) error {
	r.mu.Lock()
	if link != nil {
		r.upserts = append(r.upserts, *link)
	}
	err := r.upsertErr
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return r.IChatStorageRepository.UpsertChatwootMessageLink(link)
}

func (r *replyCountingRepo) CountChatwootDeviceConfigs() (int, error) {
	if r.countErr != nil {
		return 0, r.countErr
	}
	return r.IChatStorageRepository.CountChatwootDeviceConfigs()
}

func (r *replyCountingRepo) lookups() []domainChatStorage.ChatwootReplyLinkFilter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domainChatStorage.ChatwootReplyLinkFilter(nil), r.replyFilters...)
}

func (r *replyCountingRepo) writes() []domainChatStorage.ChatwootMessageLink {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domainChatStorage.ChatwootMessageLink(nil), r.upserts...)
}

func (r *replyCountingRepo) messageReads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.messageLookups...)
}

// replyLookupStub serves quoted-link lookups and stored WhatsApp messages
// from maps, for resolver tests.
type replyLookupStub struct {
	domainChatStorage.IChatStorageRepository
	links        map[int]*domainChatStorage.ChatwootMessageLink
	err          error
	filters      []domainChatStorage.ChatwootReplyLinkFilter
	messages     map[string]*domainChatStorage.Message // by device + " " + id
	messageErr   error
	messageReads []string
}

func (r *replyLookupStub) GetMessageByIDAndDevice(deviceID, id string) (*domainChatStorage.Message, error) {
	key := deviceID + " " + id
	r.messageReads = append(r.messageReads, key)
	if r.messageErr != nil {
		return nil, r.messageErr
	}
	message := r.messages[key]
	if message == nil {
		return nil, nil
	}
	cloned := *message
	return &cloned, nil
}

// withMessage stores WhatsApp message id of chatJID under device-a.
func (r *replyLookupStub) withMessage(id, chatJID string) *replyLookupStub {
	if r.messages == nil {
		r.messages = map[string]*domainChatStorage.Message{}
	}
	r.messages[replyTestDeviceJID+" "+id] = &domainChatStorage.Message{
		ID: id, ChatJID: chatJID, DeviceID: replyTestDeviceJID, Sender: chatJID, Content: "original",
	}
	return r
}

func (r *replyLookupStub) GetChatwootMessageLinkForReply(filter domainChatStorage.ChatwootReplyLinkFilter) (*domainChatStorage.ChatwootMessageLink, error) {
	r.filters = append(r.filters, filter)
	if r.err != nil {
		return nil, r.err
	}
	link := r.links[filter.ChatwootMessageID]
	if link == nil {
		return nil, nil
	}
	cloned := *link
	return &cloned, nil
}

// replyLIDStore maps one LID to one phone JID (or nothing when empty); err
// makes every LID lookup fail.
type replyLIDStore struct {
	lid types.JID
	pn  types.JID
	err error
}

func (s *replyLIDStore) PutManyLIDMappings(context.Context, []store.LIDMapping) error { return nil }
func (s *replyLIDStore) PutLIDMapping(context.Context, types.JID, types.JID) error    { return nil }

func (s *replyLIDStore) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	if s.err != nil {
		return types.EmptyJID, s.err
	}
	if !s.lid.IsEmpty() && lid.ToNonAD() == s.lid.ToNonAD() {
		return s.pn, nil
	}
	return types.EmptyJID, nil
}

func (s *replyLIDStore) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	if !s.pn.IsEmpty() && pn.ToNonAD() == s.pn.ToNonAD() {
		return s.lid, nil
	}
	return types.EmptyJID, nil
}

func (s *replyLIDStore) GetManyLIDsForPNs(context.Context, []types.JID) (map[types.JID]types.JID, error) {
	return map[types.JID]types.JID{}, nil
}

func mustReplyJID(t *testing.T, raw string) types.JID {
	t.Helper()
	jid, err := types.ParseJID(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return jid
}

// captureReplyLogs sends logrus output to a buffer at debug level for the rest
// of the test, restoring the previous output and level afterwards.
func captureReplyLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logger := logrus.StandardLogger()
	prevOut, prevLevel := logger.Out, logger.GetLevel()
	t.Cleanup(func() {
		logrus.SetOutput(prevOut)
		logrus.SetLevel(prevLevel)
	})
	var buf bytes.Buffer
	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.DebugLevel)
	return &buf
}

func replyTestClient(t *testing.T, user string, lids *replyLIDStore) *whatsmeow.Client {
	t.Helper()
	id := types.NewJID(user, types.DefaultUserServer)
	device := &store.Device{ID: &id}
	if lids != nil {
		device.LIDs = lids
	}
	return &whatsmeow.Client{Store: device}
}

// --- webhook environment ---------------------------------------------------

type replyEnv struct {
	t        *testing.T
	db       *sql.DB
	base     domainChatStorage.IChatStorageRepository
	repo     *replyCountingRepo
	sender   *replySendStub
	dm       *whatsapp.DeviceManager
	instance *whatsapp.DeviceInstance
	lids     *replyLIDStore
	handler  *ChatwootHandler
	app      *fiber.App
	server   *httptest.Server

	notesMu sync.Mutex
	notes   int
}

// newReplyEnv builds a webhook handler backed by a real temporary SQLite store,
// a recording send usecase, device slot-a (storage JID device-a), and a fake
// Chatwoot server that counts private failure notes. perDevice inserts config
// 11 (account 3, inbox 5) bound to slot-a; otherwise the env (legacy) config is
// used with CHATWOOT_DEVICE_ID=slot-a.
func newReplyEnv(t *testing.T, perDevice bool) *replyEnv {
	t.Helper()
	env := &replyEnv{t: t}

	db, err := sql.Open(sqlite.DriverName, filepath.Join(t.TempDir(), "chatstorage.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	base := chatstorage.NewStorageRepository(db)
	if err := base.InitializeSchema(); err != nil {
		t.Fatalf("initialize schema: %v", err)
	}
	env.db = db
	env.base = base
	env.repo = &replyCountingRepo{IChatStorageRepository: base}

	env.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/accounts/3/conversations/7/messages" {
			var body struct {
				Private bool `json:"private"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Private {
				env.notesMu.Lock()
				env.notes++
				env.notesMu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":990001}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(env.server.Close)

	prevURL, prevToken := config.ChatwootURL, config.ChatwootAPIToken
	prevAccount, prevInbox := config.ChatwootAccountID, config.ChatwootInboxID
	prevDevice, prevHosts := config.ChatwootDeviceID, config.ChatwootAllowedHosts
	prevRead, prevSecret := config.ChatwootMessageRead, config.ChatwootWebhookSecret
	t.Cleanup(func() {
		config.ChatwootURL, config.ChatwootAPIToken = prevURL, prevToken
		config.ChatwootAccountID, config.ChatwootInboxID = prevAccount, prevInbox
		config.ChatwootDeviceID, config.ChatwootAllowedHosts = prevDevice, prevHosts
		config.ChatwootMessageRead, config.ChatwootWebhookSecret = prevRead, prevSecret
	})
	config.ChatwootURL = env.server.URL
	config.ChatwootAPIToken = "token-a"
	config.ChatwootAccountID = 3
	config.ChatwootInboxID = 5
	config.ChatwootDeviceID = replyTestSlot
	// The fake Chatwoot listens on loopback; an explicit allowlist is the
	// operator opt-out of the per-device client's SSRF dial guard.
	config.ChatwootAllowedHosts = []string{"127.0.0.1"}
	config.ChatwootMessageRead = false
	config.ChatwootWebhookSecret = ""
	withSignConfig(t, false, "\n\n")

	if perDevice {
		env.insertConfig(11, 3, 5, true)
	}

	chatwoot.InitClientRegistry(env.repo)
	t.Cleanup(func() { chatwoot.InitClientRegistry(nil) })

	env.lids = &replyLIDStore{}
	env.dm = whatsapp.NewDeviceManager(nil, nil, nil)
	env.instance = whatsapp.NewDeviceInstance(replyTestSlot, replyTestClient(t, "device-a", env.lids), nil)
	env.dm.AddDevice(env.instance)

	env.sender = &replySendStub{}
	env.handler = &ChatwootHandler{SendUsecase: env.sender, DeviceManager: env.dm, ChatStorageRepo: env.repo}
	env.app = fiber.New()
	env.app.Post("/chatwoot/webhook", env.handler.HandleWebhook)
	env.app.Post("/chatwoot/webhook/:device_id", env.handler.HandleDeviceWebhook)
	env.app.Put("/devices/:device_id/chatwoot/config", env.handler.UpsertChatwootConfig)
	env.app.Delete("/devices/:device_id/chatwoot/config", env.handler.DeleteChatwootConfig)
	return env
}

// request sends any method to the env app and returns the status and body.
func (e *replyEnv) request(method, path, body string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		e.t.Fatalf("app.Test: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// setLinkTimes pins a link's timestamps, since the upsert always stamps
// updated_at with the wall clock; ordering tests use fixture times, no sleeps.
func (e *replyEnv) setLinkTimes(waMessageID string, created, updated time.Time) {
	e.t.Helper()
	res, err := e.db.Exec(`UPDATE chatwoot_message_links SET created_at = ?, updated_at = ? WHERE wa_message_id = ?`,
		created, updated, waMessageID)
	if err != nil {
		e.t.Fatalf("set times of %s: %v", waMessageID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		e.t.Fatalf("set times of %s: %d rows, want 1", waMessageID, n)
	}
}

func (e *replyEnv) countByConfig(configID int64) int {
	e.t.Helper()
	n, err := e.base.CountChatwootMessageLinksByConfig(configID)
	if err != nil {
		e.t.Fatalf("count links of config %d: %v", configID, err)
	}
	return n
}

func (e *replyEnv) insertConfig(id int64, accountID, inboxID int, enabled bool) {
	e.t.Helper()
	e.insertDeviceConfig(id, replyTestSlot, replyTestDeviceJID, accountID, inboxID, enabled)
}

func (e *replyEnv) insertDeviceConfig(id int64, deviceID, deviceJID string, accountID, inboxID int, enabled bool) {
	e.t.Helper()
	now := time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC)
	if _, err := e.db.Exec(`
		INSERT INTO chatwoot_device_configs (id, device_id, device_jid, chatwoot_url, account_id, inbox_id, api_token, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, deviceID, deviceJID, e.server.URL, accountID, inboxID, "token-a", enabled, now, now); err != nil {
		e.t.Fatalf("insert config %d: %v", id, err)
	}
}

// createReply delivers agent reply 42 through the webhook as the env's first
// send, so its reply link is the one the handler itself wrote: WA-REPLY.
func (e *replyEnv) createReply(path string, opts ...replyBodyOpt) *domainChatStorage.ChatwootMessageLink {
	e.t.Helper()
	if len(e.sender.snapshot()) != 0 {
		e.t.Fatalf("createReply must be the first send of the env")
	}
	e.sender.ids = []string{"WA-REPLY"}
	if got := e.post(path, replyWebhookBody(e.t, 42, opts...)); got != fiber.StatusOK {
		e.t.Fatalf("reply POST status = %d, want 200", got)
	}
	reply := e.link("WA-REPLY")
	if reply == nil {
		e.t.Fatalf("reply link WA-REPLY was not written")
	}
	return reply
}

// seedLink writes a link directly to SQLite (not counted), defaulting to the
// synthetic scope: device-a, peer-a, conversation 7, account 3, inbox 5.
func (e *replyEnv) seedLink(link domainChatStorage.ChatwootMessageLink) {
	e.t.Helper()
	if link.DeviceID == "" {
		link.DeviceID = replyTestDeviceJID
	}
	if link.WhatsAppChatJID == "" {
		link.WhatsAppChatJID = replyTestChatJID
	}
	if link.ChatwootConversationID == 0 {
		link.ChatwootConversationID = 7
	}
	if link.ChatwootInboxID == 0 {
		link.ChatwootInboxID = 5
	}
	if link.ChatwootAccountID == 0 {
		link.ChatwootAccountID = 3
	}
	if link.Direction == "" {
		link.Direction = "outgoing"
	}
	if link.CreatedAt.IsZero() {
		link.CreatedAt = time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)
	}
	if err := e.base.UpsertChatwootMessageLink(&link); err != nil {
		e.t.Fatalf("seed link %s: %v", link.WhatsAppMessageID, err)
	}
}

// seedMessage stores WhatsApp message id of chatJID under deviceID (device-a
// when empty), as the quote's source for a "WAID:<id>" reference.
func (e *replyEnv) seedMessage(deviceID, id, chatJID string) {
	e.t.Helper()
	if deviceID == "" {
		deviceID = replyTestDeviceJID
	}
	if err := e.base.StoreMessage(&domainChatStorage.Message{
		ID: id, ChatJID: chatJID, DeviceID: deviceID, Sender: chatJID, Content: "original",
		Timestamp: time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		e.t.Fatalf("seed message %s: %v", id, err)
	}
}

func (e *replyEnv) link(waMessageID string) *domainChatStorage.ChatwootMessageLink {
	e.t.Helper()
	link, err := e.base.GetChatwootMessageLinkByWhatsAppID(replyTestDeviceJID, waMessageID)
	if err != nil {
		e.t.Fatalf("load link %s: %v", waMessageID, err)
	}
	return link
}

func (e *replyEnv) linkCount() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM chatwoot_message_links`).Scan(&n); err != nil {
		e.t.Fatalf("count links: %v", err)
	}
	return n
}

func (e *replyEnv) noteCount() int {
	e.notesMu.Lock()
	defer e.notesMu.Unlock()
	return e.notes
}

func (e *replyEnv) post(path, body string) int {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		e.t.Fatalf("app.Test: %v", err)
	}
	return resp.StatusCode
}

type replyBodyOpt func(map[string]any)

// replyWebhookBody builds a real Chatwoot message_created JSON body so the
// handler decodes it exactly as in production (numbers become float64).
func replyWebhookBody(t *testing.T, id int, opts ...replyBodyOpt) string {
	t.Helper()
	payload := map[string]any{
		"event":        "message_created",
		"message_type": "outgoing",
		"id":           id,
		"content":      "Olá",
		"private":      false,
		"source_id":    nil,
		"account":      map[string]any{"id": 3},
		"conversation": map[string]any{
			"id":       7,
			"inbox_id": 5,
			"meta": map[string]any{"sender": map[string]any{
				"id":                1,
				"custom_attributes": map[string]any{"gowa_whatsapp_jid": replyTestChatJID},
			}},
		},
		"sender": map[string]any{"name": "Agente"},
	}
	for _, opt := range opts {
		opt(payload)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(raw)
}

func withReplyField(key string, value any) replyBodyOpt {
	return func(p map[string]any) { p[key] = value }
}

func withReplyAttrs(attrs map[string]any) replyBodyOpt {
	return withReplyField("content_attributes", attrs)
}

func withQuotedID(id int) replyBodyOpt {
	return withReplyAttrs(map[string]any{"in_reply_to": id, "in_reply_to_external_id": nil})
}

func withContactAttr(key, value string) replyBodyOpt {
	return func(p map[string]any) {
		conv := p["conversation"].(map[string]any)
		sender := conv["meta"].(map[string]any)["sender"].(map[string]any)
		sender["custom_attributes"].(map[string]any)[key] = value
	}
}

func withAccountInbox(accountID, inboxID int) replyBodyOpt {
	return func(p map[string]any) {
		p["account"] = map[string]any{"id": accountID}
		p["conversation"].(map[string]any)["inbox_id"] = inboxID
	}
}

func withAttachments(fileTypes ...string) replyBodyOpt {
	return func(p map[string]any) {
		atts := make([]any, 0, len(fileTypes))
		for i, fileType := range fileTypes {
			atts = append(atts, map[string]any{"id": 800 + i, "file_type": fileType, "data_url": replyTestDataURL})
		}
		p["attachments"] = atts
	}
}

// --- resolver unit tests ---------------------------------------------------

var replyKnownScope = chatwootCorrelationScope{ConfigID: 11, AccountID: 3, InboxID: 5, Known: true}

func replyResolverPayload(attrs map[string]any) chatwoot.WebhookPayload {
	return chatwoot.WebhookPayload{
		ID:                99,
		Account:           chatwoot.Account{ID: 3},
		Conversation:      chatwoot.ConversationWebhook{ID: 7, InboxID: 5},
		ContentAttributes: attrs,
	}
}

func replyStubWithLink(id int, waID, chatJID string) *replyLookupStub {
	return &replyLookupStub{links: map[int]*domainChatStorage.ChatwootMessageLink{
		id: {DeviceID: replyTestDeviceJID, WhatsAppMessageID: waID, WhatsAppChatJID: chatJID, ChatwootMessageID: id},
	}}
}

// A WAID-prefixed external reference wins, with or without surrounding spaces:
// the stored message is read by device and id, and no link lookup runs even
// when in_reply_to points elsewhere.
func TestResolveChatwootReplyExternalReferenceTakesPrecedence(t *testing.T) {
	for _, external := range []string{"WAID:WA-ORIG", "  WAID:WA-ORIG  ", "WAID: WA-ORIG "} {
		t.Run(strconv.Quote(external), func(t *testing.T) {
			repo := replyStubWithLink(41, "WA-AGENT", replyTestChatJID).withMessage("WA-ORIG", replyTestChatJID)
			h := &ChatwootHandler{ChatStorageRepo: repo}
			payload := replyResolverPayload(map[string]any{"in_reply_to_external_id": external, "in_reply_to": float64(41)})

			id := h.resolveChatwootReplyMessageID(context.Background(), payload, replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil)
			if id == nil || *id != "WA-ORIG" {
				t.Fatalf("reply id = %s, want WA-ORIG", replyValue(id))
			}
			if len(repo.filters) != 0 {
				t.Fatalf("quoted-link lookups = %d, want 0", len(repo.filters))
			}
			if want := replyTestDeviceJID + " WA-ORIG"; len(repo.messageReads) != 1 || repo.messageReads[0] != want {
				t.Fatalf("message reads = %q, want [%q]", repo.messageReads, want)
			}
		})
	}
}

// A WAID reference is quoted only when the message is stored for the sending
// device and belongs to the destination chat (an @lid side maps only through
// the sending client). Anything else is a plain send, and the numeric
// in_reply_to, which here resolves to a valid link, is not tried instead.
func TestResolveChatwootReplyExternalReferenceChecksStoredMessage(t *testing.T) {
	lid := mustReplyJID(t, replyTestLID)
	pn := mustReplyJID(t, replyTestChatJID)
	mapped := replyTestClient(t, "device-a", &replyLIDStore{lid: lid, pn: pn})
	unmapped := replyTestClient(t, "device-a", &replyLIDStore{})
	stored := func(chatJID string) func() *replyLookupStub {
		return func() *replyLookupStub { return (&replyLookupStub{}).withMessage("WA-ORIG", chatJID) }
	}

	cases := []struct {
		name        string
		repo        func() *replyLookupStub
		device      string
		destination string
		client      *whatsmeow.Client
		want        string
	}{
		{"same chat", stored(replyTestChatJID), replyTestDeviceJID, replyTestChatJID, nil, "WA-ORIG"},
		{"group chat", stored(replyTestGroupJID), replyTestDeviceJID, replyTestGroupJID, nil, "WA-ORIG"},
		{"stored under lid, mapped by sending client", stored(replyTestLID), replyTestDeviceJID, replyTestChatJID, mapped, "WA-ORIG"},
		{"stored under lid without mapping", stored(replyTestLID), replyTestDeviceJID, replyTestChatJID, unmapped, "<nil>"},
		{"other chat", stored("peer-b@s.whatsapp.net"), replyTestDeviceJID, replyTestChatJID, mapped, "<nil>"},
		{"not stored", func() *replyLookupStub { return &replyLookupStub{} }, replyTestDeviceJID, replyTestChatJID, nil, "<nil>"},
		{"read error", func() *replyLookupStub {
			repo := stored(replyTestChatJID)()
			repo.messageErr = errors.New("database is locked")
			return repo
		}, replyTestDeviceJID, replyTestChatJID, nil, "<nil>"},
		{"no storage device", stored(replyTestChatJID), " ", replyTestChatJID, nil, "<nil>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tc.repo()
			repo.links = map[int]*domainChatStorage.ChatwootMessageLink{
				41: {DeviceID: replyTestDeviceJID, WhatsAppMessageID: "WA-AGENT", WhatsAppChatJID: tc.destination, ChatwootMessageID: 41},
			}
			h := &ChatwootHandler{ChatStorageRepo: repo}
			payload := replyResolverPayload(map[string]any{"in_reply_to_external_id": "WAID:WA-ORIG", "in_reply_to": float64(41)})

			id := h.resolveChatwootReplyMessageID(context.Background(), payload, replyKnownScope, tc.device, tc.destination, tc.client)
			if got := replyValue(id); got != tc.want {
				t.Fatalf("reply id = %s, want %s", got, tc.want)
			}
			if len(repo.filters) != 0 {
				t.Fatalf("quoted-link lookups = %d, want 0 (no numeric fallback)", len(repo.filters))
			}
			if strings.TrimSpace(tc.device) == "" && len(repo.messageReads) != 0 {
				t.Fatalf("message reads = %q, want none without a storage device", repo.messageReads)
			}
		})
	}
}

// Absent or malformed optional attributes yield no reference and no
// lookup; a valid in_reply_to next to an unusable external id still resolves.
func TestResolveChatwootReplyIgnoresInvalidOptionalAttributes(t *testing.T) {
	cases := map[string]map[string]any{
		"nil map":           nil,
		"empty map":         {},
		"null fields":       {"in_reply_to": nil, "in_reply_to_external_id": nil},
		"external number":   {"in_reply_to_external_id": float64(41)},
		"external object":   {"in_reply_to_external_id": map[string]any{"id": "WA-ORIG"}},
		"no prefix":         {"in_reply_to_external_id": "WA-ORIG"},
		"wrong prefix":      {"in_reply_to_external_id": "WAMID:WA-ORIG"},
		"lowercase prefix":  {"in_reply_to_external_id": "waid:WA-ORIG"},
		"empty suffix":      {"in_reply_to_external_id": "WAID:"},
		"blank suffix":      {"in_reply_to_external_id": "WAID:   "},
		"prefix not at top": {"in_reply_to_external_id": "x WAID:WA-ORIG"},
	}
	for name, attrs := range cases {
		t.Run(name, func(t *testing.T) {
			repo := replyStubWithLink(41, "WA-AGENT", replyTestChatJID)
			h := &ChatwootHandler{ChatStorageRepo: repo}

			id := h.resolveChatwootReplyMessageID(context.Background(), replyResolverPayload(attrs), replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil)
			if id != nil {
				t.Fatalf("reply id = %q, want nil", *id)
			}
			if len(repo.filters) != 0 {
				t.Fatalf("quoted-link lookups = %d, want 0", len(repo.filters))
			}

			if attrs == nil {
				return
			}
			withNumber := map[string]any{"in_reply_to": float64(41)}
			for k, v := range attrs {
				if k != "in_reply_to" {
					withNumber[k] = v
				}
			}
			id = h.resolveChatwootReplyMessageID(context.Background(), replyResolverPayload(withNumber), replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil)
			if id == nil || *id != "WA-AGENT" {
				t.Fatalf("with in_reply_to 41: reply id = %s, want WA-AGENT", replyValue(id))
			}
			if len(repo.filters) != 1 || repo.filters[0].ChatwootMessageID != 41 {
				t.Fatalf("with in_reply_to 41: lookups = %+v, want exactly one for 41", repo.filters)
			}
		})
	}
}

// in_reply_to is accepted only as the float64 JSON decoding produces,
// positive, integral, finite and within int/float64-safe range.
func TestResolveChatwootReplyNumericBounds(t *testing.T) {
	lookup := func(t *testing.T, raw any) (*string, []domainChatStorage.ChatwootReplyLinkFilter) {
		t.Helper()
		repo := &replyLookupStub{links: map[int]*domainChatStorage.ChatwootMessageLink{}}
		if v, ok := raw.(float64); ok && v > 0 && v <= float64(chatwootMaxReplyMessageID) && v == math.Trunc(v) {
			repo.links[int(v)] = &domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-AGENT", WhatsAppChatJID: replyTestChatJID}
		}
		h := &ChatwootHandler{ChatStorageRepo: repo}
		id := h.resolveChatwootReplyMessageID(context.Background(), replyResolverPayload(map[string]any{"in_reply_to": raw}), replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil)
		return id, repo.filters
	}

	t.Run("float64 41 queries exactly 41", func(t *testing.T) {
		id, filters := lookup(t, float64(41))
		if id == nil || *id != "WA-AGENT" {
			t.Fatalf("reply id = %s, want WA-AGENT", replyValue(id))
		}
		want := domainChatStorage.ChatwootReplyLinkFilter{
			DeviceID: replyTestDeviceJID, ChatwootMessageID: 41, ChatwootConversationID: 7,
			ChatwootAccountID: 3, ChatwootInboxID: 5, ChatwootConfigID: 11,
		}
		if len(filters) != 1 || filters[0] != want {
			t.Fatalf("lookups = %+v, want [%+v]", filters, want)
		}
	})

	invalid := map[string]any{
		"numeric string": "41",
		"bool":           true,
		"object":         map[string]any{"id": float64(41)},
		"list":           []any{float64(41)},
		"fraction":       41.5,
		"zero":           float64(0),
		"negative":       float64(-41),
		"+inf":           math.Inf(1),
		"-inf":           math.Inf(-1),
		"nan":            math.NaN(),
		"overflow":       1e300,
		"first above":    float64(chatwootMaxReplyMessageID) + 1,
	}
	for name, raw := range invalid {
		t.Run("rejects "+name, func(t *testing.T) {
			id, filters := lookup(t, raw)
			if id != nil || len(filters) != 0 {
				t.Fatalf("reply id = %s, lookups = %d; want nil and 0", replyValue(id), len(filters))
			}
		})
	}

	t.Run("largest allowed id is accepted", func(t *testing.T) {
		id, filters := lookup(t, float64(chatwootMaxReplyMessageID))
		if id == nil || len(filters) != 1 || filters[0].ChatwootMessageID != chatwootMaxReplyMessageID {
			t.Fatalf("reply id = %s, lookups = %+v; want one lookup for %d", replyValue(id), filters, chatwootMaxReplyMessageID)
		}
	})

	if strconv.IntSize == 64 {
		t.Run("MaxInt32+1 accepted on 64-bit", func(t *testing.T) {
			id, filters := lookup(t, float64(math.MaxInt32)+1)
			if id == nil || len(filters) != 1 || int64(filters[0].ChatwootMessageID) != int64(math.MaxInt32)+1 {
				t.Fatalf("reply id = %s, lookups = %+v; want one lookup for MaxInt32+1", replyValue(id), filters)
			}
		})
	}
}

// At the resolver level, the link's chat must be the destination chat, compared
// as full NonAD JIDs; an @lid side maps only through the sending client.
func TestResolveChatwootReplyChecksLinkChat(t *testing.T) {
	lid := mustReplyJID(t, replyTestLID)
	pn := mustReplyJID(t, replyTestChatJID)
	mapped := replyTestClient(t, "device-a", &replyLIDStore{lid: lid, pn: pn})
	unmapped := replyTestClient(t, "device-a", &replyLIDStore{})

	cases := []struct {
		name        string
		linkChat    string
		destination string
		client      *whatsmeow.Client
		want        string
	}{
		{"same chat", replyTestChatJID, replyTestChatJID, nil, "WA-AGENT"},
		{"same chat with device suffix", "peer-a:12@s.whatsapp.net", replyTestChatJID, nil, "WA-AGENT"},
		{"other chat", "peer-b@s.whatsapp.net", replyTestChatJID, mapped, "<nil>"},
		{"same user other server", "peer-a@g.us", replyTestChatJID, mapped, "<nil>"},
		{"group", replyTestGroupJID, replyTestGroupJID, nil, "WA-AGENT"},
		{"lid link, phone destination, mapped by sending client", replyTestLID, replyTestChatJID, mapped, "WA-AGENT"},
		{"phone link, lid destination, mapped by sending client", replyTestChatJID, replyTestLID, mapped, "WA-AGENT"},
		{"same lid without mapping", replyTestLID, replyTestLID, unmapped, "WA-AGENT"},
		{"lid and phone without mapping", replyTestLID, replyTestChatJID, unmapped, "<nil>"},
		{"lid and phone without client", replyTestLID, replyTestChatJID, nil, "<nil>"},
		{"empty link chat", "", replyTestChatJID, mapped, "<nil>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := replyStubWithLink(41, "WA-AGENT", tc.linkChat)
			h := &ChatwootHandler{ChatStorageRepo: repo}
			payload := replyResolverPayload(map[string]any{"in_reply_to": float64(41)})
			id := h.resolveChatwootReplyMessageID(context.Background(), payload, replyKnownScope, replyTestDeviceJID, tc.destination, tc.client)
			if got := replyValue(id); got != tc.want {
				t.Fatalf("reply id = %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("blank WhatsApp id on the link", func(t *testing.T) {
		repo := replyStubWithLink(41, "  ", replyTestChatJID)
		h := &ChatwootHandler{ChatStorageRepo: repo}
		id := h.resolveChatwootReplyMessageID(context.Background(), replyResolverPayload(map[string]any{"in_reply_to": float64(41)}), replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil)
		if id != nil {
			t.Fatalf("reply id = %q, want nil", *id)
		}
	})
}

// At the resolver level, a missing link, a lookup error, no repository, or an
// incomplete scope give nil; the incomplete scope is never queried.
func TestResolveChatwootReplyUnavailable(t *testing.T) {
	payload := replyResolverPayload(map[string]any{"in_reply_to": float64(41)})

	t.Run("no link", func(t *testing.T) {
		repo := &replyLookupStub{}
		h := &ChatwootHandler{ChatStorageRepo: repo}
		if id := h.resolveChatwootReplyMessageID(context.Background(), payload, replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil); id != nil {
			t.Fatalf("reply id = %q, want nil", *id)
		}
		if len(repo.filters) != 1 {
			t.Fatalf("lookups = %d, want 1", len(repo.filters))
		}
	})

	t.Run("lookup error", func(t *testing.T) {
		repo := &replyLookupStub{err: errors.New("database is locked")}
		h := &ChatwootHandler{ChatStorageRepo: repo}
		if id := h.resolveChatwootReplyMessageID(context.Background(), payload, replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil); id != nil {
			t.Fatalf("reply id = %q, want nil", *id)
		}
	})

	t.Run("no repository", func(t *testing.T) {
		h := &ChatwootHandler{}
		if id := h.resolveChatwootReplyMessageID(context.Background(), payload, replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil); id != nil {
			t.Fatalf("reply id = %q, want nil", *id)
		}
		external := replyResolverPayload(map[string]any{"in_reply_to_external_id": "WAID:WA-ORIG"})
		if id := h.resolveChatwootReplyMessageID(context.Background(), external, replyKnownScope, replyTestDeviceJID, replyTestChatJID, nil); id != nil {
			t.Fatalf("external reply id without repository = %q, want nil", *id)
		}
	})

	incomplete := map[string]struct {
		scope   chatwootCorrelationScope
		device  string
		payload chatwoot.WebhookPayload
	}{
		"unconfirmed scope": {chatwootCorrelationScope{ConfigID: 11, AccountID: 3, InboxID: 5}, replyTestDeviceJID, payload},
		"no account":        {chatwootCorrelationScope{ConfigID: 11, InboxID: 5, Known: true}, replyTestDeviceJID, payload},
		"no inbox":          {chatwootCorrelationScope{ConfigID: 11, AccountID: 3, Known: true}, replyTestDeviceJID, payload},
		"no device":         {replyKnownScope, " ", payload},
		"no conversation": {replyKnownScope, replyTestDeviceJID, chatwoot.WebhookPayload{
			Account: chatwoot.Account{ID: 3}, Conversation: chatwoot.ConversationWebhook{InboxID: 5},
			ContentAttributes: map[string]any{"in_reply_to": float64(41)},
		}},
	}
	for name, tc := range incomplete {
		t.Run(name, func(t *testing.T) {
			repo := replyStubWithLink(41, "WA-AGENT", replyTestChatJID)
			h := &ChatwootHandler{ChatStorageRepo: repo}
			if id := h.resolveChatwootReplyMessageID(context.Background(), tc.payload, tc.scope, tc.device, replyTestChatJID, nil); id != nil {
				t.Fatalf("reply id = %q, want nil", *id)
			}
			if len(repo.filters) != 0 {
				t.Fatalf("lookups = %d, want 0", len(repo.filters))
			}
		})
	}
}

// --- webhook tests -----------------------------------------------------------

// A reply to an earlier agent message (no source_id) quotes the WhatsApp
// message recorded by the first delivery, in legacy and per-device modes.
func TestChatwootWebhookQuotesPreviousAgentMessage(t *testing.T) {
	for _, mode := range []struct {
		name      string
		perDevice bool
		configID  int64
	}{{"legacy", false, 0}, {"per-device", true, 11}} {
		t.Run(mode.name, func(t *testing.T) {
			env := newReplyEnv(t, mode.perDevice)

			if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 50, withReplyField("content", "Primeira"))); got != fiber.StatusOK {
				t.Fatalf("first POST status = %d, want 200", got)
			}
			first := env.link("WA-SENT-1")
			if first == nil || first.ChatwootMessageID != 50 || first.SourceID != "" || first.ChatwootConfigID != mode.configID {
				t.Fatalf("first link = %+v, want Chatwoot 50, empty source_id, config %d", first, mode.configID)
			}

			body := replyWebhookBody(t, 51, withReplyField("content", "Segunda"), withQuotedID(50))
			if got := env.post("/chatwoot/webhook", body); got != fiber.StatusOK {
				t.Fatalf("second POST status = %d, want 200", got)
			}

			calls := env.sender.snapshot()
			if len(calls) != 2 {
				t.Fatalf("send calls = %+v, want 2", calls)
			}
			want := []struct{ method, text, reply string }{
				{"SendText", "Primeira", "<nil>"},
				{"SendText", "Segunda", "WA-SENT-1"},
			}
			for i, w := range want {
				if calls[i].Method != w.method || calls[i].Text != w.text || replyValue(calls[i].Reply) != w.reply {
					t.Fatalf("call %d = %s %q reply=%s, want %s %q reply=%s", i, calls[i].Method, calls[i].Text, replyValue(calls[i].Reply), w.method, w.text, w.reply)
				}
			}
			lookups := env.repo.lookups()
			if len(lookups) != 1 || lookups[0].ChatwootMessageID != 50 || lookups[0].ChatwootConfigID != mode.configID ||
				lookups[0].AllowLegacyAccountZero != !mode.perDevice {
				t.Fatalf("lookups = %+v, want one for 50 in config %d", lookups, mode.configID)
			}
			if env.noteCount() != 0 {
				t.Fatalf("failure notes = %d, want 0", env.noteCount())
			}
		})
	}
}

// The slot alias differs from the storage JID; lookup and link use the
// storage JID, every send carries the routed device, and the composed text
// (signature + markdown translation) is unchanged by the quote.
func TestChatwootWebhookReplyUsesStorageIdentityAndComposedText(t *testing.T) {
	env := newReplyEnv(t, true)
	withSignConfig(t, true, "\n\n")
	env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ORIG", ChatwootMessageID: 41, ChatwootConfigID: 11})

	body := replyWebhookBody(t, 52, withReplyField("content", "**Olá**"), withQuotedID(41))
	if got := env.post("/chatwoot/webhook", body); got != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	calls := env.sender.snapshot()
	if len(calls) != 1 {
		t.Fatalf("send calls = %+v, want 1", calls)
	}
	call := calls[0]
	if call.Method != "SendText" || call.Text != "*Agente*\n\n*Olá*" || replyValue(call.Reply) != "WA-ORIG" || call.Phone != "peer-a" {
		t.Fatalf("call = %s %q reply=%s phone=%q, want SendText \"*Agente*\\n\\n*Olá*\" reply=WA-ORIG phone=peer-a",
			call.Method, call.Text, replyValue(call.Reply), call.Phone)
	}
	if call.Device != env.instance {
		t.Fatalf("send context device = %v, want slot-a instance", call.Device)
	}
	lookups := env.repo.lookups()
	if len(lookups) != 1 || lookups[0].DeviceID != replyTestDeviceJID {
		t.Fatalf("lookups = %+v, want one with device %s", lookups, replyTestDeviceJID)
	}
	stored := env.link("WA-SENT-1")
	if stored == nil || stored.DeviceID != replyTestDeviceJID || stored.ChatwootMessageID != 52 {
		t.Fatalf("stored link = %+v, want device-a / Chatwoot 52", stored)
	}
}

// Each attachment type reaches exactly its send method with the quote,
// destination and URL; audio is a voice note; captioned types get the
// composed caption.
func TestChatwootWebhookQuotesEveryAttachmentType(t *testing.T) {
	cases := []struct {
		fileType string
		method   string
		caption  string
		ptt      bool
	}{
		{"image", "SendImage", "*Agente*\n\nLegenda", false},
		{"audio", "SendAudio", "", true},
		{"video", "SendVideo", "*Agente*\n\nLegenda", false},
		{"file", "SendFile", "*Agente*\n\nLegenda", false},
		{"location", "SendFile", "*Agente*\n\nLegenda", false},
	}
	for i, tc := range cases {
		t.Run(tc.fileType, func(t *testing.T) {
			env := newReplyEnv(t, true)
			withSignConfig(t, true, "\n\n")
			env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ORIG", ChatwootMessageID: 41, ChatwootConfigID: 11})

			body := replyWebhookBody(t, 60+i, withReplyField("content", "Legenda"), withQuotedID(41), withAttachments(tc.fileType))
			if got := env.post("/chatwoot/webhook", body); got != fiber.StatusOK {
				t.Fatalf("status = %d, want 200", got)
			}
			calls := env.sender.snapshot()
			if len(calls) != 1 {
				t.Fatalf("send calls = %+v, want exactly 1", calls)
			}
			call := calls[0]
			if call.Method != tc.method || replyValue(call.Reply) != "WA-ORIG" || call.Phone != "peer-a" ||
				call.URL != replyTestDataURL || call.Text != tc.caption || call.PTT != tc.ptt {
				t.Fatalf("call = %+v reply=%s, want %s reply=WA-ORIG phone=peer-a url=%s caption=%q ptt=%v",
					call, replyValue(call.Reply), tc.method, replyTestDataURL, tc.caption, tc.ptt)
			}
		})
	}
}

// Several attachments all carry the quote; only the first carries the
// caption; no extra text message; one link per delivered file.
func TestChatwootWebhookQuotesAllAttachments(t *testing.T) {
	env := newReplyEnv(t, true)
	withSignConfig(t, true, "\n\n")
	env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ORIG", ChatwootMessageID: 41, ChatwootConfigID: 11})

	body := replyWebhookBody(t, 70, withReplyField("content", "Legenda"), withQuotedID(41), withAttachments("image", "image", "image"))
	if got := env.post("/chatwoot/webhook", body); got != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	calls := env.sender.snapshot()
	var replies, captions []string
	for _, call := range calls {
		if call.Method != "SendImage" {
			t.Fatalf("unexpected send %s (calls %+v)", call.Method, calls)
		}
		replies = append(replies, replyValue(call.Reply))
		captions = append(captions, call.Text)
	}
	if strings.Join(replies, ",") != "WA-ORIG,WA-ORIG,WA-ORIG" {
		t.Fatalf("replies = %v, want [WA-ORIG WA-ORIG WA-ORIG]", replies)
	}
	if len(captions) != 3 || captions[0] != "*Agente*\n\nLegenda" || captions[1] != "" || captions[2] != "" {
		t.Fatalf("captions = %q, want [composed, \"\", \"\"]", captions)
	}
	for _, waID := range []string{"WA-SENT-1", "WA-SENT-2", "WA-SENT-3"} {
		link := env.link(waID)
		if link == nil || link.ChatwootMessageID != 70 || link.ChatwootConfigID != 11 {
			t.Fatalf("link %s = %+v, want Chatwoot 70 / config 11", waID, link)
		}
	}
	if n := env.linkCount(); n != 4 {
		t.Fatalf("links = %d, want 4 (seed + 3 files)", n)
	}
}

// Audio fails, the file fallback keeps the quote, URL and caption, and
// only the fallback's id is linked, without a failure note.
func TestChatwootWebhookAudioFallbackKeepsQuote(t *testing.T) {
	env := newReplyEnv(t, true)
	withSignConfig(t, true, "\n\n")
	env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ORIG", ChatwootMessageID: 41, ChatwootConfigID: 11})
	env.sender.failKind = map[string]error{"SendAudio": errors.New("unsupported audio")}

	body := replyWebhookBody(t, 71, withReplyField("content", "Legenda"), withQuotedID(41), withAttachments("audio"))
	if got := env.post("/chatwoot/webhook", body); got != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	calls := env.sender.snapshot()
	if len(calls) != 2 || calls[0].Method != "SendAudio" || calls[1].Method != "SendFile" {
		t.Fatalf("calls = %+v, want [SendAudio SendFile]", calls)
	}
	for _, call := range calls {
		if replyValue(call.Reply) != "WA-ORIG" || call.URL != replyTestDataURL {
			t.Fatalf("call %s reply=%s url=%s, want WA-ORIG and %s", call.Method, replyValue(call.Reply), call.URL, replyTestDataURL)
		}
	}
	if calls[1].Text != "*Agente*\n\nLegenda" {
		t.Fatalf("fallback caption = %q, want composed caption", calls[1].Text)
	}
	if link := env.link("WA-SENT-1"); link == nil || link.ChatwootMessageID != 71 {
		t.Fatalf("fallback link = %+v, want Chatwoot 71", link)
	}
	if link := env.link("WA-FAILED-1"); link != nil {
		t.Fatalf("failed audio attempt was linked: %+v", link)
	}
	if n := env.linkCount(); n != 2 {
		t.Fatalf("links = %d, want 2 (seed + fallback)", n)
	}
	if env.noteCount() != 0 {
		t.Fatalf("failure notes = %d, want 0", env.noteCount())
	}
}

// The first attachment fails; the second still carries the quote with an
// empty caption, only its id is linked, and one failure note is posted.
func TestChatwootWebhookPartialAttachmentFailureKeepsQuote(t *testing.T) {
	env := newReplyEnv(t, true)
	withSignConfig(t, true, "\n\n")
	env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ORIG", ChatwootMessageID: 41, ChatwootConfigID: 11})
	env.sender.failCall = map[int]error{1: errors.New("upload failed")}

	body := replyWebhookBody(t, 72, withReplyField("content", "Legenda"), withQuotedID(41), withAttachments("image", "video"))
	if got := env.post("/chatwoot/webhook", body); got != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	calls := env.sender.snapshot()
	if len(calls) != 2 || calls[0].Method != "SendImage" || calls[1].Method != "SendVideo" {
		t.Fatalf("calls = %+v, want [SendImage SendVideo] and no retry", calls)
	}
	if replyValue(calls[0].Reply) != "WA-ORIG" || replyValue(calls[1].Reply) != "WA-ORIG" {
		t.Fatalf("replies = [%s %s], want both WA-ORIG", replyValue(calls[0].Reply), replyValue(calls[1].Reply))
	}
	if calls[1].Text != "" {
		t.Fatalf("second caption = %q, want empty", calls[1].Text)
	}
	if link := env.link("WA-SENT-1"); link == nil || link.ChatwootMessageID != 72 {
		t.Fatalf("second attachment link = %+v, want Chatwoot 72", link)
	}
	if n := env.linkCount(); n != 2 {
		t.Fatalf("links = %d, want 2 (seed + second attachment)", n)
	}
	if env.noteCount() != 1 {
		t.Fatalf("failure notes = %d, want 1", env.noteCount())
	}
}

// At the webhook level, the destination always comes from the route; a quoted
// link of another chat is dropped, a group link is quoted into the group, and
// an @lid link needs the sending device's own mapping.
func TestChatwootWebhookReplyChatIdentity(t *testing.T) {
	newer := time.Date(2026, time.October, 1, 10, 0, 0, 0, time.UTC)

	t.Run("other chat keeps destination without quote", func(t *testing.T) {
		env := newReplyEnv(t, true)
		env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-OTHER", WhatsAppChatJID: "peer-b@s.whatsapp.net", ChatwootMessageID: 41, ChatwootConfigID: 11})
		env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-DEST", ChatwootMessageID: 30, ChatwootConfigID: 11, CreatedAt: newer})

		if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 80, withQuotedID(41))); got != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
		calls := env.sender.snapshot()
		if len(calls) != 1 || calls[0].Phone != "peer-a" || calls[0].Reply != nil {
			t.Fatalf("calls = %+v, want one send to peer-a without quote", calls)
		}
	})

	t.Run("group", func(t *testing.T) {
		env := newReplyEnv(t, true)
		env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-GROUP", WhatsAppChatJID: replyTestGroupJID, ChatwootMessageID: 41, ChatwootConfigID: 11})

		body := replyWebhookBody(t, 81, withQuotedID(41), withContactAttr("gowa_whatsapp_jid", replyTestGroupJID))
		if got := env.post("/chatwoot/webhook", body); got != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
		calls := env.sender.snapshot()
		if len(calls) != 1 || calls[0].Phone != replyTestGroupJID || replyValue(calls[0].Reply) != "WA-GROUP" {
			t.Fatalf("calls = %+v, want one send to %s quoting WA-GROUP", calls, replyTestGroupJID)
		}
	})

	for _, tc := range []struct {
		name      string
		mapOnSlot bool
		want      string
	}{
		{"lid mapped by the sending device", true, "WA-LID"},
		{"lid mapped only by another device", false, "<nil>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReplyEnv(t, true)
			lid, pn := mustReplyJID(t, replyTestLID), mustReplyJID(t, replyTestChatJID)
			if tc.mapOnSlot {
				env.lids.lid, env.lids.pn = lid, pn
			}
			env.dm.AddDevice(whatsapp.NewDeviceInstance("slot-b", replyTestClient(t, "device-b", &replyLIDStore{lid: lid, pn: pn}), nil))
			env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-LID", WhatsAppChatJID: replyTestLID, ChatwootMessageID: 41, ChatwootConfigID: 11})
			env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-DEST", ChatwootMessageID: 30, ChatwootConfigID: 11, CreatedAt: newer})

			if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 82, withQuotedID(41))); got != fiber.StatusOK {
				t.Fatalf("status = %d, want 200", got)
			}
			calls := env.sender.snapshot()
			if len(calls) != 1 || calls[0].Phone != "peer-a" || replyValue(calls[0].Reply) != tc.want || calls[0].Device != env.instance {
				t.Fatalf("calls = %+v, want one send from slot-a to peer-a quoting %s", calls, tc.want)
			}
		})
	}
}

// At the webhook level, with a valid route, an unresolvable quote still sends
// exactly one plain message, returns 200 and posts no failure note.
func TestChatwootWebhookUnresolvedQuoteSendsPlainMessage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replyErr error
	}{
		{"no link", nil},
		{"lookup error", errors.New("database is locked")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReplyEnv(t, true)
			env.repo.replyErr = tc.replyErr

			if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 83, withQuotedID(41))); got != fiber.StatusOK {
				t.Fatalf("status = %d, want 200", got)
			}
			calls := env.sender.snapshot()
			if len(calls) != 1 || calls[0].Method != "SendText" || calls[0].Reply != nil {
				t.Fatalf("calls = %+v, want exactly one plain SendText", calls)
			}
			if len(env.repo.lookups()) != 1 {
				t.Fatalf("lookups = %d, want 1", len(env.repo.lookups()))
			}
			if env.noteCount() != 0 {
				t.Fatalf("failure notes = %d, want 0", env.noteCount())
			}
		})
	}
}

// Private notes, incoming messages and echoes stop before delivery: no send,
// no quoted-link lookup and no stored-message read. An agent reply with an
// empty source_id and a WAID reference to a stored message of its chat is
// delivered with the quote.
func TestChatwootWebhookGuardsRunBeforeQuoteResolution(t *testing.T) {
	quoted := withReplyAttrs(map[string]any{"in_reply_to": 41, "in_reply_to_external_id": "WAID:WA-ORIG"})
	chatwoot.MarkMessageAsSent(3, 7700001)

	// The numeric-only reference would hit the link store if resolution ran
	// before the guards; the WAID one would not, so both are exercised.
	numericOnly := withReplyAttrs(map[string]any{"in_reply_to": 41, "in_reply_to_external_id": nil})
	for _, ref := range []struct {
		name string
		opt  replyBodyOpt
	}{
		{"waid", quoted},
		{"numeric", numericOnly},
	} {
		for _, tc := range []struct {
			name string
			body func(t *testing.T, ref replyBodyOpt) string
		}{
			{"private", func(t *testing.T, ref replyBodyOpt) string {
				return replyWebhookBody(t, 90, ref, withReplyField("private", true))
			}},
			{"incoming", func(t *testing.T, ref replyBodyOpt) string {
				return replyWebhookBody(t, 91, ref, withReplyField("message_type", "incoming"))
			}},
			{"forwarded echo", func(t *testing.T, ref replyBodyOpt) string {
				return replyWebhookBody(t, 92, ref, withReplyField("source_id", "WAID:WA-ECHO"))
			}},
			{"api echo", func(t *testing.T, ref replyBodyOpt) string { return replyWebhookBody(t, 7700001, ref) }},
		} {
			t.Run(ref.name+"/"+tc.name, func(t *testing.T) {
				env := newReplyEnv(t, true)
				env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-AGENT", ChatwootMessageID: 41, ChatwootConfigID: 11})
				env.seedMessage("", "WA-ORIG", replyTestChatJID)
				if got := env.post("/chatwoot/webhook", tc.body(t, ref.opt)); got != fiber.StatusOK {
					t.Fatalf("status = %d, want 200", got)
				}
				if calls := env.sender.snapshot(); len(calls) != 0 {
					t.Fatalf("calls = %+v, want none", calls)
				}
				if n := len(env.repo.lookups()); n != 0 {
					t.Fatalf("lookups = %d, want 0", n)
				}
				if reads := env.repo.messageReads(); len(reads) != 0 {
					t.Fatalf("message reads = %q, want none", reads)
				}
			})
		}
	}

	t.Run("agent reply without source_id", func(t *testing.T) {
		env := newReplyEnv(t, true)
		env.seedMessage("", "WA-ORIG", replyTestChatJID)
		if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 93, quoted, withReplyField("source_id", ""))); got != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
		calls := env.sender.snapshot()
		if len(calls) != 1 || replyValue(calls[0].Reply) != "WA-ORIG" || calls[0].Phone != "peer-a" {
			t.Fatalf("calls = %+v, want one send to peer-a quoting WA-ORIG", calls)
		}
		if n := len(env.repo.lookups()); n != 0 {
			t.Fatalf("lookups = %d, want 0 (external reference)", n)
		}
	})
}

// A link write failure after a successful send is not a delivery
// failure: one send, 200, no failure note, no resend.
func TestChatwootWebhookLinkWriteFailureDoesNotResend(t *testing.T) {
	env := newReplyEnv(t, true)
	env.repo.upsertErr = errors.New("disk full")

	if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 94)); got != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if calls := env.sender.snapshot(); len(calls) != 1 {
		t.Fatalf("calls = %+v, want exactly 1", calls)
	}
	if n := len(env.repo.writes()); n != 1 {
		t.Fatalf("link writes = %d, want 1", n)
	}
	if env.noteCount() != 0 {
		t.Fatalf("failure notes = %d, want 0", env.noteCount())
	}
}

// On the webhook, a WAID reference is quoted only when the message is stored
// for the sending device in the destination chat. Otherwise exactly one plain
// message is sent, with 200 and no failure note, and the numeric in_reply_to
// (a valid link here) is not tried instead.
func TestChatwootWebhookExternalReplyRequiresStoredMessageInChat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *replyEnv)
		want  string
	}{
		{"same chat", func(env *replyEnv) { env.seedMessage("", "WA-ORIG", replyTestChatJID) }, "WA-ORIG"},
		{"stored under lid mapped by the sending device", func(env *replyEnv) {
			env.lids.lid, env.lids.pn = mustReplyJID(env.t, replyTestLID), mustReplyJID(env.t, replyTestChatJID)
			env.seedMessage("", "WA-ORIG", replyTestLID)
		}, "WA-ORIG"},
		{"other chat", func(env *replyEnv) { env.seedMessage("", "WA-ORIG", "peer-b@s.whatsapp.net") }, "<nil>"},
		{"stored only for another device", func(env *replyEnv) {
			env.seedMessage("device-b@s.whatsapp.net", "WA-ORIG", replyTestChatJID)
		}, "<nil>"},
		{"not stored", func(env *replyEnv) {}, "<nil>"},
		{"read error", func(env *replyEnv) {
			env.seedMessage("", "WA-ORIG", replyTestChatJID)
			env.repo.messageErr = errors.New("database is locked")
		}, "<nil>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReplyEnv(t, true)
			env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-AGENT", ChatwootMessageID: 41, ChatwootConfigID: 11})
			tc.setup(env)

			body := replyWebhookBody(t, 95, withReplyAttrs(map[string]any{"in_reply_to": 41, "in_reply_to_external_id": "WAID:WA-ORIG"}))
			if got := env.post("/chatwoot/webhook", body); got != fiber.StatusOK {
				t.Fatalf("status = %d, want 200", got)
			}
			calls := env.sender.snapshot()
			if len(calls) != 1 || calls[0].Method != "SendText" || calls[0].Phone != "peer-a" || replyValue(calls[0].Reply) != tc.want {
				t.Fatalf("calls = %+v, want one SendText to peer-a quoting %s", calls, tc.want)
			}
			wantRead := replyTestDeviceJID + " WA-ORIG"
			if reads := env.repo.messageReads(); len(reads) != 1 || reads[0] != wantRead {
				t.Fatalf("message reads = %q, want [%q]", reads, wantRead)
			}
			if n := len(env.repo.lookups()); n != 0 {
				t.Fatalf("quoted-link lookups = %d, want 0 (no numeric fallback)", n)
			}
			if env.noteCount() != 0 {
				t.Fatalf("failure notes = %d, want 0", env.noteCount())
			}
		})
	}
}

// In confirmed legacy mode the account-0 wildcard of the quoted-link lookup
// covers account-0 rows only: a config-0 link of another account (4) with the
// same device, message, conversation and inbox is never quoted.
func TestChatwootWebhookLegacyQuoteIgnoresOtherAccountLink(t *testing.T) {
	env := newReplyEnv(t, false)
	env.seedLink(domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ACCOUNT-4", ChatwootMessageID: 41, ChatwootConfigID: 0, ChatwootAccountID: 4})

	if got := env.post("/chatwoot/webhook", replyWebhookBody(t, 96, withQuotedID(41))); got != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	calls := env.sender.snapshot()
	if len(calls) != 1 || calls[0].Phone != "peer-a" || calls[0].Reply != nil || calls[0].Device != env.instance {
		t.Fatalf("calls = %+v, want one plain send from slot-a to peer-a", calls)
	}
	want := domainChatStorage.ChatwootReplyLinkFilter{
		DeviceID: replyTestDeviceJID, ChatwootMessageID: 41, ChatwootConversationID: 7,
		ChatwootAccountID: 3, ChatwootInboxID: 5, ChatwootConfigID: 0, AllowLegacyAccountZero: true,
	}
	if lookups := env.repo.lookups(); len(lookups) != 1 || lookups[0] != want {
		t.Fatalf("lookups = %+v, want [%+v]", lookups, want)
	}
	if env.noteCount() != 0 {
		t.Fatalf("failure notes = %d, want 0", env.noteCount())
	}
}

// Neither the scope confirmation nor the chat comparison writes the device
// identifier, the LID or the phone JID to the log, at any level: a failing
// config lookup during confirmation, and lid/phone comparisons with a mapping,
// without one, and with a failing LID store.
func TestChatwootReplyChecksDoNotLogIdentifiers(t *testing.T) {
	logs := captureReplyLogs(t)
	storageDeviceID := replyTestDeviceJID
	lid := mustReplyJID(t, replyTestLID)
	pn := mustReplyJID(t, replyTestChatJID)

	env := newReplyEnv(t, true)
	// The real ambiguity error quotes the identifier, so logging the error text
	// would leak it too.
	env.repo.configErr = fmt.Errorf("ambiguous identifier %q", storageDeviceID)
	payload := replyResolverPayload(map[string]any{"in_reply_to": float64(41)})
	route := chatwootWebhookRoute{DeviceID: storageDeviceID, AccountID: 3, InboxID: 5}
	if scope := env.handler.confirmChatwootCorrelationScope(payload, route, replyTestSlot, storageDeviceID); scope != (chatwootCorrelationScope{}) {
		t.Fatalf("scope = %+v, want unconfirmed on a config lookup error", scope)
	}
	if env.repo.configLookups != 2 {
		t.Fatalf("config lookups = %d, want 2 (slot and storage JID, both failing)", env.repo.configLookups)
	}

	ctx := context.Background()
	mapped := replyTestClient(t, "device-a", &replyLIDStore{lid: lid, pn: pn})
	unmapped := replyTestClient(t, "device-a", &replyLIDStore{})
	failing := replyTestClient(t, "device-a", &replyLIDStore{err: errors.New("lid store unavailable")})
	if !chatwootReplyChatMatches(ctx, mapped, lid.String(), pn.String()) || !chatwootReplyChatMatches(ctx, mapped, pn.String(), lid.String()) {
		t.Fatal("mapped lid and phone JID do not match")
	}
	if chatwootReplyChatMatches(ctx, unmapped, lid.String(), pn.String()) || chatwootReplyChatMatches(ctx, failing, lid.String(), pn.String()) {
		t.Fatal("lid and phone JID match without a mapping")
	}
	h := &ChatwootHandler{ChatStorageRepo: replyStubWithLink(41, "WA-AGENT", lid.String())}
	if id := h.resolveChatwootReplyMessageID(ctx, payload, replyKnownScope, storageDeviceID, pn.String(), mapped); replyValue(id) != "WA-AGENT" {
		t.Fatalf("reply id = %s, want WA-AGENT", replyValue(id))
	}

	got := logs.String()
	if !strings.Contains(got, "correlation scope unconfirmed") {
		t.Fatalf("the unconfirmed scope was not logged; log capture is not working:\n%s", got)
	}
	if strings.Contains(got, storageDeviceID) {
		t.Fatalf("logs contain the storage device id:\n%s", got)
	}
	if strings.Contains(got, lid.String()) {
		t.Fatalf("logs contain the LID:\n%s", got)
	}
	if strings.Contains(got, pn.String()) {
		t.Fatalf("logs contain the phone JID:\n%s", got)
	}
}
