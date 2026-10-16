package whatsapp

import (
	"errors"
	"fmt"
	"testing"

	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
)

// replyLinkSpyRepo records the filters that reach the base repository and
// returns a fixed result.
type replyLinkSpyRepo struct {
	domainChatStorage.IChatStorageRepository
	filters []domainChatStorage.ChatwootReplyLinkFilter
	link    *domainChatStorage.ChatwootMessageLink
	err     error
}

func (r *replyLinkSpyRepo) GetChatwootMessageLinkForReply(filter domainChatStorage.ChatwootReplyLinkFilter) (*domainChatStorage.ChatwootMessageLink, error) {
	r.filters = append(r.filters, filter)
	return r.link, r.err
}

// The wrapper fills an omitted device, keeps an explicit one, forwards
// every other field (legacy flag included) unchanged, and returns the base
// repository's result and error as is.
func TestDeviceChatStorageDelegatesReplyLinkLookup(t *testing.T) {
	baseFilter := domainChatStorage.ChatwootReplyLinkFilter{
		ChatwootMessageID:      41,
		ChatwootConversationID: 7,
		ChatwootAccountID:      3,
		ChatwootInboxID:        5,
		ChatwootConfigID:       11,
		AllowLegacyAccountZero: true,
	}

	cases := []struct {
		name       string
		deviceID   string
		wantDevice string
	}{
		{name: "omitted device is filled", deviceID: "", wantDevice: "device-a@s.whatsapp.net"},
		{name: "explicit device is preserved", deviceID: "device-b@s.whatsapp.net", wantDevice: "device-b@s.whatsapp.net"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := &domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-ORIG"}
			wantErr := errors.New("lookup failed")
			spy := &replyLinkSpyRepo{link: want, err: wantErr}
			wrapper := newDeviceChatStorage("device-a@s.whatsapp.net", spy)

			filter := baseFilter
			filter.DeviceID = tc.deviceID
			got, err := wrapper.GetChatwootMessageLinkForReply(filter)

			if got != want {
				t.Fatalf("link = %+v, want the base repository's link", got)
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("err = %v, want %v", err, wantErr)
			}
			if len(spy.filters) != 1 {
				t.Fatalf("base lookups = %d, want 1", len(spy.filters))
			}
			expected := baseFilter
			expected.DeviceID = tc.wantDevice
			if spy.filters[0] != expected {
				t.Fatalf("base filter = %+v, want %+v", spy.filters[0], expected)
			}
		})
	}

	t.Run("nil result passes through", func(t *testing.T) {
		spy := &replyLinkSpyRepo{}
		wrapper := newDeviceChatStorage("device-a@s.whatsapp.net", spy)
		got, err := wrapper.GetChatwootMessageLinkForReply(baseFilter)
		if got != nil || err != nil {
			t.Fatalf("got (%+v, %v), want (nil, nil)", got, err)
		}
	})
}

// linkScopeSpyRepo records, as text, the arguments every existing Chatwoot
// link method receives from the wrapper and returns fixed results, so a test
// can prove the wrapper neither drops nor rewrites the configuration scope.
type linkScopeSpyRepo struct {
	domainChatStorage.IChatStorageRepository
	calls    []string
	link     *domainChatStorage.ChatwootMessageLink
	count    int
	affected int64
	err      error
}

func (r *linkScopeSpyRepo) record(format string, args ...any) {
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *linkScopeSpyRepo) UpsertChatwootMessageLink(link *domainChatStorage.ChatwootMessageLink) error {
	r.record("Upsert device=%s wa=%s config=%d account=%d", link.DeviceID, link.WhatsAppMessageID, link.ChatwootConfigID, link.ChatwootAccountID)
	return r.err
}

func (r *linkScopeSpyRepo) GetChatwootMessageLinkByWhatsAppID(deviceID, waMessageID string) (*domainChatStorage.ChatwootMessageLink, error) {
	r.record("ByWhatsAppID device=%s wa=%s", deviceID, waMessageID)
	return r.link, r.err
}

func (r *linkScopeSpyRepo) GetChatwootMessageLinkByChatwootID(deviceID string, chatwootMessageID int) (*domainChatStorage.ChatwootMessageLink, error) {
	r.record("ByChatwootID device=%s message=%d", deviceID, chatwootMessageID)
	return r.link, r.err
}

func (r *linkScopeSpyRepo) GetLatestChatwootMessageLinkByConversation(conversationID, accountID int, allowLegacyZero bool, configID int64) (*domainChatStorage.ChatwootMessageLink, error) {
	r.record("LatestByConversation conversation=%d account=%d legacy=%t config=%d", conversationID, accountID, allowLegacyZero, configID)
	return r.link, r.err
}

func (r *linkScopeSpyRepo) GetLatestUnreadChatwootMessageLinkByChat(deviceID, waChatJID string) (*domainChatStorage.ChatwootMessageLink, error) {
	r.record("LatestUnreadByChat device=%s chat=%s", deviceID, waChatJID)
	return r.link, r.err
}

func (r *linkScopeSpyRepo) BackfillChatwootMessageLinkAccount(accountID int) (int64, error) {
	r.record("BackfillAccount account=%d", accountID)
	return r.affected, r.err
}

func (r *linkScopeSpyRepo) CountChatwootMessageLinksByConfig(configID int64) (int, error) {
	r.record("CountByConfig config=%d", configID)
	return r.count, r.err
}

func (r *linkScopeSpyRepo) DeleteChatwootMessageLinksByConfig(configID int64) error {
	r.record("DeleteByConfig config=%d", configID)
	return r.err
}

// Every existing link method of the wrapper hands the configuration (and
// the account/conversation scope) to the base repository exactly as asked —
// 11 stays 11 and an explicit 0 stays 0 — fills only an omitted device, and
// returns the base repository's result and error unchanged.
// TestDeviceChatStorageDelegatesReplyLinkLookup covers the reply-link lookup.
func TestDeviceChatStorageDelegatesChatwootLinkScope(t *testing.T) {
	const device = "device-a@s.whatsapp.net"
	const otherDevice = "device-b@s.whatsapp.net"

	type outcome struct {
		link *domainChatStorage.ChatwootMessageLink
		n    int64
		err  error
	}
	cases := []struct {
		name     string
		call     func(domainChatStorage.IChatStorageRepository) outcome
		wantCall string
		wantN    int64
		wantLink bool
	}{
		{
			name: "upsert fills omitted device and keeps config 11",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				return outcome{err: w.UpsertChatwootMessageLink(&domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-REPLY", ChatwootConfigID: 11, ChatwootAccountID: 3})}
			},
			wantCall: "Upsert device=device-a@s.whatsapp.net wa=WA-REPLY config=11 account=3",
		},
		{
			name: "upsert keeps explicit device and legacy config 0",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				return outcome{err: w.UpsertChatwootMessageLink(&domainChatStorage.ChatwootMessageLink{DeviceID: otherDevice, WhatsAppMessageID: "WA-OLD", ChatwootConfigID: 0, ChatwootAccountID: 3})}
			},
			wantCall: "Upsert device=device-b@s.whatsapp.net wa=WA-OLD config=0 account=3",
		},
		{
			name: "lookup by WhatsApp id fills omitted device",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				link, err := w.GetChatwootMessageLinkByWhatsAppID("", "WA-FWD")
				return outcome{link: link, err: err}
			},
			wantCall: "ByWhatsAppID device=device-a@s.whatsapp.net wa=WA-FWD",
			wantLink: true,
		},
		{
			name: "lookup by Chatwoot id keeps explicit device",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				link, err := w.GetChatwootMessageLinkByChatwootID(otherDevice, 42)
				return outcome{link: link, err: err}
			},
			wantCall: "ByChatwootID device=device-b@s.whatsapp.net message=42",
			wantLink: true,
		},
		{
			name: "conversation lookup keeps config 11",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				link, err := w.GetLatestChatwootMessageLinkByConversation(7, 3, false, 11)
				return outcome{link: link, err: err}
			},
			wantCall: "LatestByConversation conversation=7 account=3 legacy=false config=11",
			wantLink: true,
		},
		{
			name: "conversation lookup keeps requested config 0",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				link, err := w.GetLatestChatwootMessageLinkByConversation(7, 3, true, 0)
				return outcome{link: link, err: err}
			},
			wantCall: "LatestByConversation conversation=7 account=3 legacy=true config=0",
			wantLink: true,
		},
		{
			name: "latest unread lookup fills omitted device",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				link, err := w.GetLatestUnreadChatwootMessageLinkByChat("", "peer-a@s.whatsapp.net")
				return outcome{link: link, err: err}
			},
			wantCall: "LatestUnreadByChat device=device-a@s.whatsapp.net chat=peer-a@s.whatsapp.net",
			wantLink: true,
		},
		{
			name: "account backfill passes the account",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				n, err := w.BackfillChatwootMessageLinkAccount(3)
				return outcome{n: n, err: err}
			},
			wantCall: "BackfillAccount account=3",
			wantN:    2,
		},
		{
			name: "count by config keeps 11",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				n, err := w.CountChatwootMessageLinksByConfig(11)
				return outcome{n: int64(n), err: err}
			},
			wantCall: "CountByConfig config=11",
			wantN:    5,
		},
		{
			name: "delete by config keeps 11",
			call: func(w domainChatStorage.IChatStorageRepository) outcome {
				return outcome{err: w.DeleteChatwootMessageLinksByConfig(11)}
			},
			wantCall: "DeleteByConfig config=11",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseLink := &domainChatStorage.ChatwootMessageLink{WhatsAppMessageID: "WA-REPLY", ChatwootConfigID: 11}
			baseErr := errors.New("storage failed")
			spy := &linkScopeSpyRepo{link: baseLink, count: 5, affected: 2, err: baseErr}
			wrapper := newDeviceChatStorage(device, spy)

			got := tc.call(wrapper)

			if len(spy.calls) != 1 || spy.calls[0] != tc.wantCall {
				t.Fatalf("base calls = %q, want [%q]", spy.calls, tc.wantCall)
			}
			if !errors.Is(got.err, baseErr) {
				t.Fatalf("err = %v, want the base repository's error", got.err)
			}
			if tc.wantLink && got.link != baseLink {
				t.Fatalf("link = %+v, want the base repository's link", got.link)
			}
			if got.n != tc.wantN {
				t.Fatalf("result = %d, want %d", got.n, tc.wantN)
			}
		})
	}
}
