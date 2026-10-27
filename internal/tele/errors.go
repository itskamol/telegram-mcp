package tele

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/gotd/td/tgerr"
)

// hints turns Telegram RPC error types into advice an agent can act on.
// The raw type stays in the message, in brackets, for debugging.
var hints = map[string]string{
	"MESSAGE_NOT_MODIFIED":      "the new text is the same as the current one, so nothing was changed",
	"MESSAGE_ID_INVALID":        "no message with this id in this chat; get ids from list_messages for the same chat",
	"MESSAGE_EDIT_TIME_EXPIRED": "this message is too old to edit",
	"MESSAGE_AUTHOR_REQUIRED":   "only messages sent by this account can be edited",
	"MESSAGE_DELETE_FORBIDDEN":  "this account may not delete these messages (only its own, or as an admin)",
	"MESSAGE_EMPTY":             "the text is empty after formatting",
	"MESSAGE_TOO_LONG":          "the text is longer than Telegram allows",
	"MEDIA_CAPTION_TOO_LONG":    "the caption is longer than Telegram allows",
	"CHAT_WRITE_FORBIDDEN":      "this account cannot write in this chat",
	"CHAT_ADMIN_REQUIRED":       "this needs admin rights in the chat",
	"CHAT_SEND_MEDIA_FORBIDDEN": "files are not allowed in this chat",
	"CHAT_SEND_PLAIN_FORBIDDEN": "text messages are not allowed in this chat",
	"CHANNEL_PRIVATE":           "this channel is private or the account is not a member",
	"CHANNEL_INVALID":           "this channel is unknown; get its id from list_chats",
	"PEER_ID_INVALID":           "this chat is unknown to the account; get its id from list_chats, search_contacts or resolve_username",
	"USER_IS_BLOCKED":           "this user has blocked the account, or the account blocked them",
	"USER_PRIVACY_RESTRICTED":   "this user's privacy settings forbid it",
	"INPUT_USER_DEACTIVATED":    "this user's account was deleted",
	"USERNAME_NOT_OCCUPIED":     "no one has this username",
	"USERNAME_INVALID":          "this is not a valid username",
	"TOPIC_CLOSED":              "this forum topic is closed",
	"TOPIC_ID_INVALID":          "no such topic; get topic ids from list_topics",
	"FILE_REFERENCE_EXPIRED":    "the file reference expired; fetch the message again and retry",
	"RANDOM_ID_DUPLICATE":       "a message with this idempotency_key was already sent, so it was not sent again",
	"SCHEDULE_TOO_MUCH":         "too many scheduled messages in this chat",
}

// Explain rewrites a Telegram RPC error into a sentence an agent can act
// on. Other errors pass through unchanged.
func Explain(err error) error {
	rpc, ok := tgerr.As(err)
	if !ok {
		return err
	}
	switch rpc.Type {
	case "FLOOD_WAIT", "FLOOD_PREMIUM_WAIT":
		return fmt.Errorf("Telegram rate limit: wait %d seconds before trying again [%s]", rpc.Argument, rpc.Message)
	case "SLOWMODE_WAIT":
		return fmt.Errorf("slow mode is on in this chat: wait %d seconds before sending again [%s]", rpc.Argument, rpc.Message)
	}
	if hint, ok := hints[rpc.Type]; ok {
		return fmt.Errorf("%s [%s]", hint, rpc.Type)
	}
	return err
}

// IsDuplicate reports whether Telegram rejected a send because its
// random_id was already used. Telegram usually returns the original
// message instead; this covers the methods that answer with an error.
func IsDuplicate(err error) bool {
	return tgerr.Is(err, "RANDOM_ID_DUPLICATE")
}

// RandomID returns the random_id for a send. With an idempotency key it
// is derived from the key, so a retry with the same key reuses it, and
// Telegram answers with the message it already sent instead of sending a
// second one (verified live for sendMessage and forwardMessages). Without
// a key it is random. index separates the messages of one forward.
func RandomID(key string, index int) int64 {
	if key == "" {
		return randomID()
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("telegram-mcp\x00%s\x00%d", key, index)))
	return int64(binary.LittleEndian.Uint64(sum[:8]))
}

// TimeoutAdvice is appended to timeouts of write tools, where the action
// may have reached Telegram even though no answer came back.
func TimeoutAdvice(key string) string {
	if key != "" {
		return "it may still have gone through; retrying with the same idempotency_key is safe and returns the original message"
	}
	return "it may still have gone through; check list_messages before retrying, " +
		"or pass an idempotency_key next time so a retry cannot send twice"
}
