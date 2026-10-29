package mcpserver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/audit"
	"github.com/itskamol/telegram-mcp-go/internal/fsguard"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

// Preview limits for view_photo: what the model sees, not the original.
const (
	previewMaxSide  = 1280
	previewMaxBytes = 5 << 20
)

type mediaIn struct {
	Chat      ChatRef `json:"chat" jsonschema:"chat id from list_chats (number or string), @username, or me"`
	MessageID int     `json:"message_id" jsonschema:"id of the message with the attachment, from list_messages"`
}

func (s *Server) media(ctx context.Context, in mediaIn) (tele.MediaFile, error) {
	if in.MessageID <= 0 {
		return tele.MediaFile{}, errors.New("message_id is required")
	}
	r, err := s.chat(ctx, in.Chat)
	if err != nil {
		return tele.MediaFile{}, err
	}
	return s.svc.Media(ctx, r, in.MessageID)
}

func (s *Server) getMediaInfo(ctx context.Context, _ *mcp.CallToolRequest, in mediaIn) (*mcp.CallToolResult, tele.MediaInfo, error) {
	f, err := s.media(ctx, in)
	if err != nil {
		return nil, tele.MediaInfo{}, err
	}
	return nil, f.Info, nil
}

type downloadIn struct {
	Chat      ChatRef `json:"chat" jsonschema:"chat id from list_chats (number or string), @username, or me"`
	MessageID int     `json:"message_id" jsonschema:"id of the message with the attachment, from list_messages"`
	Dir       string  `json:"dir,omitempty" jsonschema:"existing directory inside the allowed directories; a relative dir is inside the first one, which is the default"`
	FileName  string  `json:"file_name,omitempty" jsonschema:"file name only, no directories; default is the original name"`
}

type downloadOut struct {
	Status     string `json:"status"` // "done", or "downloading" while it goes on in the server
	Path       string `json:"path"`   // while downloading, the planned path
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded,omitempty"` // bytes so far, while downloading
	MimeType   string `json:"mime_type,omitempty"`
	// While downloading: seconds left at the speed so far (0 = not known
	// yet), and what to do next, so an agent does not poll the file.
	EtaSeconds int64  `json:"eta_seconds,omitempty"`
	Next       string `json:"next,omitempty"`
}

func (s *Server) downloadMedia(ctx context.Context, _ *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, downloadOut, error) {
	// Resolve the directory first: a bad path should fail before any download.
	dir, err := fsguard.ResolveDir(s.policy.AllowedRoots, in.Dir)
	if err != nil {
		return nil, downloadOut{}, err
	}
	f, err := s.media(ctx, mediaIn{Chat: in.Chat, MessageID: in.MessageID})
	if err != nil {
		return nil, downloadOut{}, err
	}
	if !f.Info.Downloadable {
		return nil, downloadOut{}, fmt.Errorf("%s media has no file to download", f.Info.Kind)
	}
	if f.Info.Size > s.policy.MaxDownload {
		return nil, downloadOut{}, fmt.Errorf("file is %d MB; max_download_mb allows %d MB",
			f.Info.Size>>20, s.policy.MaxDownload>>20)
	}

	// The original name comes from the sender, so it is sanitized too.
	name := fsguard.SafeName(cmp.Or(in.FileName, f.DefaultName()), "telegram_file")
	key := jobKey{chat: f.Info.ChatID, msg: in.MessageID}
	info := jobInfo{planned: filepath.Join(dir, name), size: f.Info.Size, mime: f.Info.MimeType}
	j, err := s.downloads.start(key, info, func(ctx context.Context, j *job) (string, error) {
		return s.runDownload(ctx, j, f, dir, name, in)
	})
	if err != nil {
		return nil, downloadOut{}, err
	}
	if err := s.downloads.waitFor(ctx, j); err != nil {
		return nil, downloadOut{}, err
	}
	select {
	case <-j.done:
	default:
		got := j.downloaded()
		return nil, downloadOut{Status: "downloading", Path: j.info.planned, Size: j.info.size,
			Downloaded: got, MimeType: j.info.mime,
			EtaSeconds: etaSeconds(got, j.info.size, time.Since(j.started)),
			Next: fmt.Sprintf("Not done yet. Call download_media again with the same chat and message_id; "+
				"each call waits up to about 45 s. From a shell, `telegram-mcp download --chat %s --message %d` "+
				"waits until it is done and prints the path. Do not poll the file: it appears at path "+
				"(or a \"name (1)\" variant) only when complete.", in.Chat, in.MessageID),
		}, nil
	}
	path, err := j.result()
	if err != nil {
		s.downloads.forget(j) // reported now; the next call tries again
		return nil, downloadOut{}, err
	}
	return nil, downloadOut{Status: "done", Path: path, Size: j.info.size, MimeType: j.info.mime}, nil
}

// runDownload is the body of a download job, on the server's context. It
// writes the job's one audit line when it ends, whoever is still waiting.
func (s *Server) runDownload(ctx context.Context, j *job, f tele.MediaFile, dir, name string, in downloadIn) (string, error) {
	var path, tmp string
	var err error
	// journal records the temp file before anything is written to it, so a
	// killed server's next start can remove it. The journal outlives a
	// cancelled job: shutdown must still be able to clear the entry.
	journal := func(w interface{ TempPath() string }) error {
		if s.downloads.Journal == nil {
			return nil
		}
		if err := s.downloads.Journal.Add(context.WithoutCancel(ctx), w.TempPath()); err != nil {
			return fmt.Errorf("record the partial file: %w", err)
		}
		tmp = w.TempPath()
		return nil
	}
	if f.Info.Size > 0 {
		// Parts arrive out of order; the file is kept only if all of them came.
		refetch := func(ctx context.Context) (tele.MediaFile, error) {
			return s.media(ctx, mediaIn{Chat: in.Chat, MessageID: in.MessageID})
		}
		path, err = fsguard.WriteNewAt(dir, name, f.Info.Size, s.policy.MaxDownload, tele.DownloadPartSize,
			func(w *fsguard.PartWriter) error {
				if err := journal(w); err != nil {
					return err
				}
				j.setProgress(w)
				if err := s.svc.DownloadFile(ctx, f, w, refetch); err != nil {
					return err
				}
				// A job cancelled by shutdown must not link its file: its
				// audit line could no longer be written.
				return ctx.Err()
			})
	} else {
		// Unknown size: stream in order, bounded by the limit as it grows.
		path, err = fsguard.WriteNew(dir, name, s.policy.MaxDownload, func(w io.Writer) error {
			if t, ok := w.(interface{ TempPath() string }); ok {
				if err := journal(t); err != nil {
					return err
				}
			}
			if err := s.svc.Download(ctx, f.DC, f.Location, w); err != nil {
				return err
			}
			return ctx.Err()
		})
	}
	// The temp file is gone now (linked or removed), so is its entry. A
	// stale entry is harmless: the startup sweep finds no file to remove.
	if tmp != "" {
		_ = s.downloads.Journal.Remove(context.WithoutCancel(ctx), tmp)
	}
	if errors.Is(err, fsguard.ErrTooLarge) {
		err = fmt.Errorf("file is larger than max_download_mb (%d MB)", s.policy.MaxDownload>>20)
	}
	if err != nil && ctx.Err() != nil {
		err = context.Cause(ctx) // server stopped, stalled or too long
	}
	err = s.record(audit.Entry{Tool: "download_media", ChatID: f.Info.ChatID, IDs: []int{in.MessageID}, Path: path}, err)
	return path, err
}

func (s *Server) viewPhoto(ctx context.Context, _ *mcp.CallToolRequest, in mediaIn) (*mcp.CallToolResult, any, error) {
	f, err := s.media(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	loc, size, ok := f.PreviewLocation(previewMaxSide)
	if !ok {
		return nil, nil, fmt.Errorf("message %d has %s media, not a photo or image; use download_media", in.MessageID, f.Info.Kind)
	}
	if size > previewMaxBytes {
		return nil, nil, fmt.Errorf("image is %d MB, too large to show; use download_media", size>>20)
	}

	buf := &fsguard.LimitedBuffer{Limit: previewMaxBytes}
	if err := s.svc.Download(ctx, f.DC, loc, buf); err != nil {
		if errors.Is(err, fsguard.ErrTooLarge) {
			return nil, nil, errors.New("image is too large to show; use download_media")
		}
		return nil, nil, err
	}
	meta, _ := json.Marshal(map[string]any{"chat_id": f.Info.ChatID, "message_id": in.MessageID, "mime_type": f.Info.MimeType})
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: buf.Bytes, MIMEType: f.Info.MimeType},
		&mcp.TextContent{Text: string(meta)},
	}}, nil, nil
}

// maxCaptionLen is Telegram's caption limit for non-premium accounts.
const maxCaptionLen = 1024

// maxPhotoSize is the largest file Telegram accepts as a compressed photo.
const maxPhotoSize = 10 << 20

type sendFileIn struct {
	Chat           ChatRef `json:"chat" jsonschema:"chat id from list_chats (number or string), @username, or me"`
	Path           string  `json:"path" jsonschema:"file inside the allowed directories; a relative path is inside the first one"`
	Caption        string  `json:"caption,omitempty" jsonschema:"optional caption, up to 1024 characters after formatting"`
	ParseMode      string  `json:"parse_mode,omitempty" jsonschema:"plain (default), markdown or html, for the caption"`
	AsDocument     bool    `json:"as_document,omitempty" jsonschema:"send images as files instead of compressed photos"`
	ReplyTo        int     `json:"reply_to,omitempty" jsonschema:"id of a message in this chat to reply to"`
	TopicID        int     `json:"topic_id,omitempty" jsonschema:"forum topic id from list_topics"`
	IdempotencyKey string  `json:"idempotency_key,omitempty" jsonschema:"any unique string for this file; a retry with the same key returns the original message instead of sending it again"`
}

func (in sendFileIn) idempotencyKey() string { return in.IdempotencyKey }

func (s *Server) sendFile(ctx context.Context, _ *mcp.CallToolRequest, in sendFileIn) (*mcp.CallToolResult, sentOut, error) {
	// The file check comes first: it is the guard against exfiltration. It
	// runs on every call, so a path that is no longer allowed is refused
	// before any job is looked up. The job sends the file opened here.
	f, fi, err := fsguard.OpenFile(s.policy.AllowedRoots, in.Path)
	if err != nil {
		return nil, sentOut{}, err
	}
	handedOver := false
	defer func() {
		if !handedOver {
			f.Close()
		}
	}()
	if fi.Size() == 0 {
		return nil, sentOut{}, errors.New("file is empty")
	}
	if fi.Size() > s.policy.MaxUpload {
		return nil, sentOut{}, fmt.Errorf("file is %d MB; max_upload_mb allows %d MB", fi.Size()>>20, s.policy.MaxUpload>>20)
	}

	var caption tele.Formatted
	if in.Caption != "" {
		if caption, err = s.format(ctx, in.Caption, in.ParseMode, maxCaptionLen); err != nil {
			return nil, sentOut{}, fmt.Errorf("caption: %w", err)
		}
	}

	r, err := s.chat(ctx, in.Chat)
	if err != nil {
		return nil, sentOut{}, err
	}
	mimeType := sniffMIME(f, fi.Name())
	up := tele.Upload{
		File: f, Size: fi.Size(), Name: fi.Name(), MimeType: mimeType,
		AsPhoto: !in.AsDocument && fi.Size() <= maxPhotoSize &&
			(mimeType == "image/jpeg" || mimeType == "image/png" || mimeType == "image/webp"),
	}
	up.Uploaded = &atomic.Int64{}

	key := jobKey{chat: r.Marked, ref: uploadRef(in, f.Name(), fi)}
	info := jobInfo{kind: kindUpload, planned: f.Name(), size: fi.Size(), mime: mimeType}
	j, created, err := s.downloads.startOrFind(key, info, func(ctx context.Context, j *job) (string, error) {
		defer f.Close()
		j.setProgress(byteCount{up.Uploaded})
		id, err := s.svc.SendFile(ctx, r, up, caption, in.ReplyTo, in.TopicID,
			tele.RandomID(scopedKey("send_file", r.Marked, in.IdempotencyKey), 0))
		if err != nil && ctx.Err() != nil {
			err = context.Cause(ctx) // server stopped, stalled or too long
		}
		// The job writes the audit line, once, whoever is still waiting.
		_, out, err := s.sent("send_file", r.Marked, id, in.ReplyTo, f.Name(), err)
		j.value = out
		return "", err
	})
	if err != nil {
		return nil, sentOut{}, err
	}
	handedOver = created
	if err := s.downloads.waitFor(ctx, j); err != nil {
		return nil, sentOut{}, err
	}
	select {
	case <-j.done:
	default:
		got := j.downloaded()
		return nil, sentOut{Status: "uploading", ChatID: r.Marked, Uploaded: got, Size: j.info.size,
			EtaSeconds: etaSeconds(got, j.info.size, time.Since(j.started)),
			Next: "Not sent yet. Call send_file again with the same arguments; each call waits up to about 45 s. " +
				"It is the same upload, so the message is never sent twice.",
		}, nil
	}
	if _, err := j.result(); err != nil {
		s.downloads.forget(j) // reported now; the next call tries again
		return nil, sentOut{}, err
	}
	out := j.value.(sentOut)
	out.Status = "sent"
	return nil, out, nil
}

// uploadRef identifies a send_file job: its idempotency_key or, without
// one, everything that makes the message, so a repeated call while it
// uploads, or soon after, is the same job and never sends twice.
func uploadRef(in sendFileIn, real string, fi os.FileInfo) string {
	if in.IdempotencyKey != "" {
		return "key\x00" + in.IdempotencyKey
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s\x00%s\x00%v\x00%d\x00%d",
		real, fi.Size(), fi.ModTime().UnixNano(), in.Caption, in.ParseMode, in.AsDocument, in.ReplyTo, in.TopicID))
	return "args\x00" + hex.EncodeToString(sum[:])
}

// byteCount reports a counter as a job's progress.
type byteCount struct{ n *atomic.Int64 }

func (b byteCount) Written() int64 { return b.n.Load() }

// sniffMIME guesses the type from the extension, then from the content.
func sniffMIME(f *os.File, name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	head := make([]byte, 512)
	n, _ := f.ReadAt(head, 0)
	t, _, _ := strings.Cut(http.DetectContentType(head[:n]), ";")
	return t
}
