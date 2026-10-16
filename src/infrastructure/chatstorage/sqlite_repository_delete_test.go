package chatstorage

import (
	"context"
	"testing"
	"time"

	domainChatStorage "github.com/aldinokemal/go-whatsapp-web-multidevice/domains/chatstorage"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/whatsapp"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestCreateMessageRevokeDeletesTargetForActiveDevice(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	deviceID := "device-a@s.whatsapp.net"
	otherDeviceID := "device-b@s.whatsapp.net"
	chatJID := "628123456789@s.whatsapp.net"
	timestamp := time.Date(2026, time.August, 22, 9, 0, 0, 0, time.UTC)

	seedChatMessage(t, repo, deviceID, chatJID, "message-1", "remove me", timestamp)
	seedChatMessage(t, repo, otherDeviceID, chatJID, "message-1", "keep me", timestamp)

	ctx := whatsapp.ContextWithDevice(context.Background(), whatsapp.NewDeviceInstance(deviceID, nil, repo))
	revoke := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   types.NewJID("628123456789", types.DefaultUserServer),
				Sender: types.NewJID("628123456789", types.DefaultUserServer),
			},
			ID:        "revoke-event-1",
			Timestamp: timestamp.Add(time.Minute),
		},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(chatJID),
				ID:        proto.String("message-1"),
			},
		}},
	}

	require.NoError(t, repo.CreateMessage(ctx, revoke))

	deleted, err := repo.GetMessageByIDAndDevice(deviceID, "message-1")
	require.NoError(t, err)
	require.Nil(t, deleted)

	preserved, err := repo.GetMessageByIDAndDevice(otherDeviceID, "message-1")
	require.NoError(t, err)
	require.NotNil(t, preserved)
	require.Equal(t, "keep me", preserved.Content)
}

func TestDeleteMessageByDevicePurgesContentButPreservesChatwootLink(t *testing.T) {
	repo := newTestSQLiteRepository(t)
	deviceID := "device-a@s.whatsapp.net"
	chatJID := "628123456789@s.whatsapp.net"
	timestamp := time.Date(2026, time.August, 22, 10, 0, 0, 0, time.UTC)

	seedChatMessage(t, repo, deviceID, chatJID, "message-1", "latest content", timestamp)
	seedReaction(t, repo, deviceID, chatJID, "message-1", "628111111111@s.whatsapp.net")
	require.NoError(t, repo.StoreMessageEdit(&domainChatStorage.MessageEdit{
		OriginalMessageID: "message-1",
		EditEventID:       "edit-1",
		ChatJID:           chatJID,
		DeviceID:          deviceID,
		Editor:            deviceID,
		PreviousContent:   "sensitive original content",
		NewContent:        "latest content",
		EditedAt:          timestamp,
	}))
	require.NoError(t, repo.UpsertChatwootMessageLink(&domainChatStorage.ChatwootMessageLink{
		DeviceID:               deviceID,
		WhatsAppMessageID:      "message-1",
		WhatsAppChatJID:        chatJID,
		ChatwootMessageID:      101,
		ChatwootConversationID: 202,
		ChatwootInboxID:        303,
		ChatwootConfigID:       404,
		ChatwootAccountID:      505,
	}))

	require.NoError(t, repo.DeleteMessageByDevice(deviceID, "message-1", chatJID))

	message, err := repo.GetMessageByIDAndDevice(deviceID, "message-1")
	require.NoError(t, err)
	require.Nil(t, message)

	edits, err := repo.GetMessageEdits("message-1", deviceID)
	require.NoError(t, err)
	require.Empty(t, edits)
	require.Zero(t, countMessageReactions(t, repo))

	link, err := repo.GetChatwootMessageLinkByWhatsAppID(deviceID, "message-1")
	require.NoError(t, err)
	require.NotNil(t, link)
	require.Equal(t, 101, link.ChatwootMessageID)
}

func TestDeleteMessageRollsBackDependentDeletesWhenMessageDeleteFails(t *testing.T) {
	tests := []struct {
		name   string
		delete func(repo *SQLiteRepository, deviceID, messageID, chatJID string) error
	}{
		{
			name: "unscoped",
			delete: func(repo *SQLiteRepository, _, messageID, chatJID string) error {
				return repo.DeleteMessage(messageID, chatJID)
			},
		},
		{
			name: "device scoped",
			delete: func(repo *SQLiteRepository, deviceID, messageID, chatJID string) error {
				return repo.DeleteMessageByDevice(deviceID, messageID, chatJID)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestSQLiteRepository(t)
			deviceID := "device-a@s.whatsapp.net"
			chatJID := "628123456789@s.whatsapp.net"
			timestamp := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

			seedChatMessage(t, repo, deviceID, chatJID, "message-1", "latest content", timestamp)
			seedReaction(t, repo, deviceID, chatJID, "message-1", "628111111111@s.whatsapp.net")
			require.NoError(t, repo.StoreMessageEdit(&domainChatStorage.MessageEdit{
				OriginalMessageID: "message-1",
				EditEventID:       "edit-1",
				ChatJID:           chatJID,
				DeviceID:          deviceID,
				Editor:            deviceID,
				PreviousContent:   "original content",
				NewContent:        "latest content",
				EditedAt:          timestamp,
			}))
			_, err := repo.db.Exec(`
				CREATE TRIGGER fail_target_message_delete
				BEFORE DELETE ON messages
				WHEN OLD.id = 'message-1'
				BEGIN
					SELECT RAISE(ABORT, 'forced message delete failure');
				END
			`)
			require.NoError(t, err)

			err = tc.delete(repo, deviceID, "message-1", chatJID)
			require.ErrorContains(t, err, "forced message delete failure")

			message, lookupErr := repo.GetMessageByIDAndDevice(deviceID, "message-1")
			require.NoError(t, lookupErr)
			require.NotNil(t, message)
			require.Equal(t, 1, countMessageReactions(t, repo))

			edits, editsErr := repo.GetMessageEdits("message-1", deviceID)
			require.NoError(t, editsErr)
			require.Len(t, edits, 1)
		})
	}
}

// The general cleanups (chat, chat by device, device data, truncate) do not
// read chatwoot_config_id: the target's config 0 and config 11 links go together,
// while controls outside the target remain.
func TestGeneralDeletesRemoveChatwootLinksOfEveryConfig(t *testing.T) {
	const (
		deviceA = "device-a@s.whatsapp.net"
		deviceB = "device-b@s.whatsapp.net"
		chatA   = "peer-a@s.whatsapp.net"
		chatB   = "peer-b@s.whatsapp.net"
	)
	tests := []struct {
		name string
		run  func(repo *SQLiteRepository) error
		want []string
	}{
		{
			name: "DeleteChat",
			run:  func(repo *SQLiteRepository) error { return repo.DeleteChat(chatA) },
			want: []string{deviceA + " WA-OTHER-CHAT"},
		},
		{
			name: "DeleteChatByDevice",
			run:  func(repo *SQLiteRepository) error { return repo.DeleteChatByDevice(deviceA, chatA) },
			want: []string{deviceA + " WA-OTHER-CHAT", deviceB + " WA-OTHER-DEVICE"},
		},
		{
			name: "DeleteDeviceData",
			run:  func(repo *SQLiteRepository) error { return repo.DeleteDeviceData(deviceA) },
			want: []string{deviceB + " WA-OTHER-DEVICE"},
		},
		{
			name: "TruncateAllChats",
			run:  func(repo *SQLiteRepository) error { return repo.TruncateAllChats() },
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestSQLiteRepository(t)
			for _, link := range []*domainChatStorage.ChatwootMessageLink{
				{DeviceID: deviceA, WhatsAppMessageID: "WA-CFG0", WhatsAppChatJID: chatA, ChatwootMessageID: 40, ChatwootConversationID: 7, ChatwootInboxID: 5, ChatwootAccountID: 3, ChatwootConfigID: 0},
				{DeviceID: deviceA, WhatsAppMessageID: "WA-CFG11", WhatsAppChatJID: chatA, ChatwootMessageID: 42, ChatwootConversationID: 7, ChatwootInboxID: 5, ChatwootAccountID: 3, ChatwootConfigID: 11},
				{DeviceID: deviceB, WhatsAppMessageID: "WA-OTHER-DEVICE", WhatsAppChatJID: chatA, ChatwootMessageID: 43, ChatwootConversationID: 8, ChatwootInboxID: 5, ChatwootAccountID: 3, ChatwootConfigID: 22},
				{DeviceID: deviceA, WhatsAppMessageID: "WA-OTHER-CHAT", WhatsAppChatJID: chatB, ChatwootMessageID: 44, ChatwootConversationID: 9, ChatwootInboxID: 5, ChatwootAccountID: 3, ChatwootConfigID: 0},
			} {
				require.NoError(t, repo.UpsertChatwootMessageLink(link))
			}

			require.NoError(t, tc.run(repo))

			rows, err := repo.db.Query(`SELECT device_id, wa_message_id FROM chatwoot_message_links ORDER BY device_id, wa_message_id`)
			require.NoError(t, err)
			defer rows.Close()
			remaining := []string{}
			for rows.Next() {
				var deviceID, waMessageID string
				require.NoError(t, rows.Scan(&deviceID, &waMessageID))
				remaining = append(remaining, deviceID+" "+waMessageID)
			}
			require.NoError(t, rows.Err())
			require.Equal(t, tc.want, remaining)
		})
	}
}
