package tele

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/itskamol/telegram-mcp-go/internal/sanitize"
)

// MediaInfo describes a message's attachment.
type MediaInfo struct {
	ChatID    int64  `json:"chat_id"`
	MessageID int    `json:"message_id"`
	Kind      string `json:"kind"`
	FileName  string `json:"file_name,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Duration  int    `json:"duration_seconds,omitempty"`
	// Downloadable is false for media without a file (polls, locations, links).
	Downloadable bool `json:"downloadable"`
}

// MediaFile is a downloadable attachment.
type MediaFile struct {
	Info     MediaInfo
	Location tg.InputFileLocationClass
	// photo is set for photos, to pick a smaller size for previews.
	photo *tg.Photo
}

// ErrNoMedia means the message has no attachment.
var ErrNoMedia = errors.New("this message has no media")

// Media fetches message id of chat r and describes its attachment.
func (s *Service) Media(ctx context.Context, r Resolved, id int) (MediaFile, error) {
	msg, err := s.rawMessage(ctx, r, id)
	if err != nil {
		return MediaFile{}, err
	}
	f := MediaFile{Info: MediaInfo{ChatID: r.Marked, MessageID: id, Kind: mediaKind(msg.Media)}}
	switch m := msg.Media.(type) {
	case nil, *tg.MessageMediaEmpty:
		return MediaFile{}, ErrNoMedia
	case *tg.MessageMediaPhoto:
		p, ok := m.Photo.(*tg.Photo)
		if !ok {
			return f, nil // expired self-destructing photo
		}
		best := pickPhotoSize(p.Sizes, 0)
		if best.Type == "" {
			return f, nil
		}
		f.photo = p
		f.Info.Kind, f.Info.MimeType, f.Info.Downloadable = "photo", "image/jpeg", true
		f.Info.Width, f.Info.Height, f.Info.Size = best.W, best.H, int64(best.Size)
		f.Location = photoLocation(p, best.Type)
	case *tg.MessageMediaDocument:
		d, ok := m.Document.(*tg.Document)
		if !ok {
			return f, nil
		}
		f.Info.MimeType, f.Info.Size, f.Info.Downloadable = d.MimeType, d.Size, true
		for _, a := range d.Attributes {
			switch a := a.(type) {
			case *tg.DocumentAttributeFilename:
				f.Info.FileName = sanitize.Name(a.FileName)
			case *tg.DocumentAttributeVideo:
				f.Info.Width, f.Info.Height, f.Info.Duration = a.W, a.H, int(a.Duration)
			case *tg.DocumentAttributeImageSize:
				f.Info.Width, f.Info.Height = a.W, a.H
			case *tg.DocumentAttributeAudio:
				f.Info.Duration = a.Duration
			}
		}
		f.Location = &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}
	}
	return f, nil
}

// PreviewLocation returns a location for an image no larger than maxSide
// pixels on its long edge, for showing to the model. ok is false when the
// media is not an image.
func (f MediaFile) PreviewLocation(maxSide int) (tg.InputFileLocationClass, int, bool) {
	if f.photo != nil {
		sz := pickPhotoSize(f.photo.Sizes, maxSide)
		return photoLocation(f.photo, sz.Type), sz.Size, true
	}
	switch f.Info.MimeType {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return f.Location, int(f.Info.Size), f.Location != nil
	}
	return nil, 0, false
}

// DefaultName is a file name for the attachment when none was given.
func (f MediaFile) DefaultName() string {
	if f.Info.FileName != "" {
		return f.Info.FileName
	}
	ext := ".bin"
	if f.Info.MimeType == "image/jpeg" {
		ext = ".jpg"
	} else if exts, _ := mime.ExtensionsByType(f.Info.MimeType); len(exts) > 0 {
		ext = exts[0]
	}
	return fmt.Sprintf("telegram_%d_%d%s", f.Info.ChatID, f.Info.MessageID, ext)
}

// Download streams a file location into w.
func (s *Service) Download(ctx context.Context, loc tg.InputFileLocationClass, w io.Writer) error {
	if _, err := downloader.NewDownloader().Download(s.API, loc).Stream(ctx, w); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	return nil
}

func photoLocation(p *tg.Photo, thumb string) *tg.InputPhotoFileLocation {
	return &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: thumb}
}

type photoSize struct {
	Type string
	W, H int
	Size int
}

// pickPhotoSize returns the largest size whose long edge is <= maxSide
// (0 = no limit), falling back to the smallest real size.
func pickPhotoSize(sizes []tg.PhotoSizeClass, maxSide int) photoSize {
	var all []photoSize
	for _, s := range sizes {
		switch s := s.(type) {
		case *tg.PhotoSize:
			all = append(all, photoSize{s.Type, s.W, s.H, s.Size})
		case *tg.PhotoSizeProgressive:
			if len(s.Sizes) > 0 {
				all = append(all, photoSize{s.Type, s.W, s.H, s.Sizes[len(s.Sizes)-1]})
			}
		}
	}
	var best, smallest photoSize
	for _, s := range all {
		area := s.W * s.H
		if smallest.Type == "" || area < smallest.W*smallest.H {
			smallest = s
		}
		if (maxSide == 0 || max(s.W, s.H) <= maxSide) && area > best.W*best.H {
			best = s
		}
	}
	if best.Type == "" {
		return smallest
	}
	return best
}

// rawMessage fetches one message and checks it belongs to chat r.
func (s *Service) rawMessage(ctx context.Context, r Resolved, id int) (*tg.Message, error) {
	res, err := s.fetch(ctx, r, []int{id})
	if err != nil {
		return nil, err
	}
	m, ok := res.AsModified()
	if ok {
		for _, mc := range m.GetMessages() {
			if msg, ok := mc.(*tg.Message); ok && msg.ID == id && markedPeer(msg.PeerID) == r.Marked {
				return msg, nil
			}
		}
	}
	return nil, fmt.Errorf("message %d not found in chat %d", id, r.Marked)
}

// fetch loads messages by ID, using the channel method for channels.
func (s *Service) fetch(ctx context.Context, r Resolved, ids []int) (tg.MessagesMessagesClass, error) {
	in := make([]tg.InputMessageClass, len(ids))
	for i, id := range ids {
		in[i] = &tg.InputMessageID{ID: id}
	}
	var res tg.MessagesMessagesClass
	var err error
	if ch, ok := r.InputChannel(); ok {
		res, err = s.API.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: ch, ID: in})
	} else {
		// Outside channels message IDs are per account, not per chat: this
		// returns whatever chat each ID lives in, so callers filter by chat.
		res, err = s.API.MessagesGetMessages(ctx, in)
	}
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	return res, nil
}

// Upload is a local file to send.
type Upload struct {
	File     *os.File
	Size     int64
	Name     string
	MimeType string
	AsPhoto  bool // send as a compressed photo instead of a document
}

// SendFile uploads a file and posts it with an optional caption, reply and
// topic. It returns the new message ID.
func (s *Service) SendFile(ctx context.Context, r Resolved, up Upload, caption Formatted, replyTo, topicID int) (int, error) {
	in, err := uploader.NewUploader(s.API).Upload(ctx, uploader.NewUpload(up.Name, up.File, up.Size))
	if err != nil {
		return 0, fmt.Errorf("upload: %w", err)
	}
	var media tg.InputMediaClass
	if up.AsPhoto {
		media = &tg.InputMediaUploadedPhoto{File: in}
	} else {
		media = &tg.InputMediaUploadedDocument{
			File:       in,
			MimeType:   up.MimeType,
			ForceFile:  true,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: up.Name}},
		}
	}
	req := &tg.MessagesSendMediaRequest{
		Peer: r.Input, Media: media, Message: caption.Text, Entities: caption.Entities, RandomID: randomID(),
		ReplyTo: replyTarget(replyTo, topicID),
	}
	upd, err := s.API.MessagesSendMedia(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("send file: %w", err)
	}
	return sentID(upd, req.RandomID), nil
}
