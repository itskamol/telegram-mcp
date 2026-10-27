package feed

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

// Watch lists the chats to buffer: marked chat id -> forum topics to keep.
// No topics means the whole chat.
type Watch map[int64][]int

// Handler stores updates of watched chats.
//
// It never returns an error to gotd. gotd advances its in-memory pts after
// the handler regardless of the result and persists it with the next
// SetPts, so a returned error would silently drop the message. Instead a
// failed write is retried, then recorded as a dead letter, and if even
// that fails the handler reports a fatal error and blocks, so the pts of
// that update is never persisted and Telegram redelivers it after restart.
type Handler struct {
	Store   *Store
	Notify  *Notifier
	Watch   Watch
	Service *tele.Service // set before updates.Manager runs
	Log     *slog.Logger

	// Fatal receives at most one error; the server must then shut down.
	Fatal chan error

	Retries int
	Backoff time.Duration
}

// NewHandler fills in the defaults.
func NewHandler(store *Store, notify *Notifier, watch Watch, log *slog.Logger) *Handler {
	return &Handler{Store: store, Notify: notify, Watch: watch, Log: log,
		Fatal: make(chan error, 1), Retries: 5, Backoff: 200 * time.Millisecond}
}

// Register wires the handler into a dispatcher.
func (h *Handler) Register(d tg.UpdateDispatcher) {
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		h.message(ctx, e, u.Message)
		return nil
	})
	d.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		h.message(ctx, e, u.Message)
		return nil
	})
	d.OnEditMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditMessage) error {
		h.message(ctx, e, u.Message)
		return nil
	})
	d.OnEditChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditChannelMessage) error {
		h.message(ctx, e, u.Message)
		return nil
	})
	d.OnDeleteMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdateDeleteMessages) error {
		h.write(ctx, 0, 0, func(ctx context.Context) (bool, error) { return h.Store.DeleteNonChannel(ctx, u.Messages) })
		return nil
	})
	d.OnDeleteChannelMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdateDeleteChannelMessages) error {
		chat := tele.MarkedID(storage.KindChannel, u.ChannelID)
		if _, ok := h.Watch[chat]; !ok {
			return nil
		}
		// No topic comes with a delete; deleting only rows that passed the
		// topic filter on the way in keeps this fail-closed.
		h.write(ctx, chat, 0, func(ctx context.Context) (bool, error) { return h.Store.DeleteChannel(ctx, chat, u.Messages) })
		return nil
	})
}

// keep decides whether a message of a watched chat is buffered. Topic
// filters fail closed: a message whose topic cannot be told is dropped.
func (h *Handler) keep(chat int64, m *tg.Message) bool {
	topics, watched := h.Watch[chat]
	if !watched {
		return false
	}
	if len(topics) == 0 {
		return true
	}
	topic, known := tele.TopicOf(m)
	return known && slices.Contains(topics, topic)
}

func (h *Handler) message(ctx context.Context, e tg.Entities, mc tg.MessageClass) {
	m, ok := mc.(*tg.Message) // service messages (joins, pins) are not buffered
	if !ok {
		return
	}
	chat, kind := tele.ChatOfPeer(m.PeerID)
	if !h.keep(chat, m) {
		return
	}
	h.write(ctx, chat, m.ID, func(ctx context.Context) (bool, error) {
		x, ok, err := h.Service.Convert(ctx, m, e)
		if err != nil || !ok {
			return false, err
		}
		return h.Store.Upsert(ctx, kind, x.TopicID, x, m.Date)
	})
}

// write runs op with retries, then falls back to a dead letter, then to a
// fatal stop. It returns only when the update is safe to mark as handled.
func (h *Handler) write(ctx context.Context, chat int64, msgID int, op func(context.Context) (bool, error)) {
	var err error
	for attempt := range h.Retries {
		var changed bool
		if changed, err = op(ctx); err == nil {
			if changed {
				h.Notify.Publish()
			}
			return
		}
		if ctx.Err() != nil {
			// Shutting down: return so gotd can stop. StateStorage refuses
			// writes on a cancelled context, so this pts is not persisted.
			return
		}
		select {
		case <-time.After(h.Backoff << attempt):
		case <-ctx.Done():
			return
		}
	}
	if derr := h.Store.DeadLetter(ctx, chat, msgID, err); derr == nil {
		h.Log.Error("telegram update could not be buffered; recorded as a dead letter",
			"chat_id", chat, "message_id", msgID, "err", err)
		return
	}
	h.stop(fmt.Errorf("buffering chat %d message %d failed and could not be recorded: %w", chat, msgID, err))
}

// stop reports a fatal error and never returns, so gotd never reaches the
// SetPts for this update. The server exits on Fatal.
func (h *Handler) stop(err error) {
	select {
	case h.Fatal <- err:
	default: // another update already reported one
	}
	select {}
}
