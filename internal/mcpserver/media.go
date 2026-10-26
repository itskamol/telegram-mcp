package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/itskamol/telegram-mcp-go/internal/audit"
	"github.com/itskamol/telegram-mcp-go/internal/fsguard"
	"github.com/itskamol/telegram-mcp-go/internal/tele"
)

// Preview limits for open_photo: what the model sees, not the original.
const (
	previewMaxSide  = 1280
	previewMaxBytes = 5 << 20
)

type mediaIn struct {
	Chat      string `json:"chat" jsonschema:"chat id (as a string), @username, or me"`
	MessageID int    `json:"message_id" jsonschema:"id of the message with the attachment"`
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
	Chat      string `json:"chat" jsonschema:"chat id (as a string), @username, or me"`
	MessageID int    `json:"message_id" jsonschema:"id of the message with the attachment"`
	Dir       string `json:"dir,omitempty" jsonschema:"existing directory inside allowed_roots; relative paths are under the first root; default is the first root"`
	FileName  string `json:"file_name,omitempty" jsonschema:"file name only, no directories; default is the original name"`
}

type downloadOut struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type,omitempty"`
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
	path, err := fsguard.WriteNew(dir, name, s.policy.MaxDownload, func(w io.Writer) error {
		return s.svc.Download(ctx, f.Location, w)
	})
	if errors.Is(err, fsguard.ErrTooLarge) {
		err = fmt.Errorf("file is larger than max_download_mb (%d MB)", s.policy.MaxDownload>>20)
	}
	if err := s.record(audit.Entry{Tool: "download_media", ChatID: f.Info.ChatID, IDs: []int{in.MessageID}, Path: path}, err); err != nil {
		return nil, downloadOut{}, err
	}
	return nil, downloadOut{Path: path, Size: f.Info.Size, MimeType: f.Info.MimeType}, nil
}

func (s *Server) openPhoto(ctx context.Context, _ *mcp.CallToolRequest, in mediaIn) (*mcp.CallToolResult, any, error) {
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
	if err := s.svc.Download(ctx, loc, buf); err != nil {
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
	Chat       string `json:"chat" jsonschema:"chat id (as a string), @username, or me"`
	Path       string `json:"path" jsonschema:"file inside allowed_roots; relative paths are under the first root"`
	Caption    string `json:"caption,omitempty" jsonschema:"optional caption, up to 1024 characters after formatting"`
	ParseMode  string `json:"parse_mode,omitempty" jsonschema:"plain (default) or html, for the caption"`
	AsDocument bool   `json:"as_document,omitempty" jsonschema:"send images as files instead of compressed photos"`
	ReplyTo    int    `json:"reply_to,omitempty" jsonschema:"message id to reply to"`
	TopicID    int    `json:"topic_id,omitempty" jsonschema:"forum topic id from list_topics"`
}

func (s *Server) sendFile(ctx context.Context, _ *mcp.CallToolRequest, in sendFileIn) (*mcp.CallToolResult, sentOut, error) {
	// The file check comes first: it is the guard against exfiltration.
	f, fi, err := fsguard.OpenFile(s.policy.AllowedRoots, in.Path)
	if err != nil {
		return nil, sentOut{}, err
	}
	defer f.Close()
	if fi.Size() == 0 {
		return nil, sentOut{}, errors.New("file is empty")
	}
	if fi.Size() > s.policy.MaxUpload {
		return nil, sentOut{}, fmt.Errorf("file is %d MB; max_upload_mb allows %d MB", fi.Size()>>20, s.policy.MaxUpload>>20)
	}

	var caption tele.Formatted
	if in.Caption != "" {
		if caption, err = s.format(ctx, in.Caption, in.ParseMode); err != nil {
			return nil, sentOut{}, fmt.Errorf("caption: %w", err)
		}
		if n := utf8.RuneCountInString(caption.Text); n > maxCaptionLen {
			return nil, sentOut{}, fmt.Errorf("caption is %d characters; Telegram allows %d", n, maxCaptionLen)
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
	id, err := s.svc.SendFile(ctx, r, up, caption, in.ReplyTo, in.TopicID)
	if err := s.record(audit.Entry{Tool: "send_file", ChatID: r.Marked, IDs: nonZero(id), Path: f.Name()}, err); err != nil {
		return nil, sentOut{}, err
	}
	return nil, sentOut{ChatID: r.Marked, MessageID: id}, nil
}

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
