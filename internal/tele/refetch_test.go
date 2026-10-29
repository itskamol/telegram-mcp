package tele

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

var (
	oldRef = []byte("old reference")
	newRef = []byte("new reference")
)

// refetchFixture is a 2.5 MB document on another DC whose old reference
// expires from the second part on, so the first part may already be in
// the sink when the expiry comes (it depends on Parallel's ordering).
type refetchFixture struct {
	data    []byte
	dc      *fileDC
	s       *Service
	file    MediaFile
	fetches int
}

func newRefetchFixture() *refetchFixture {
	data := fileData(2<<20 + 512<<10)
	dc := &fileDC{data: data, expired: oldRef, expiredFrom: downloadPartSize}
	pools := &fakePools{dcs: map[int]*fileDC{4: dc}, this: homeDC}
	return &refetchFixture{
		data: data,
		dc:   dc,
		s:    &Service{API: tg.NewClient(&fileDC{}), Pools: pools},
		file: docFile(7, oldRef, int64(len(data))),
	}
}

func docFile(id int64, ref []byte, size int64) MediaFile {
	return MediaFile{
		Info:     MediaInfo{Size: size},
		Location: &tg.InputDocumentFileLocation{ID: id, AccessHash: 1, FileReference: ref},
		DC:       4,
	}
}

// refetch returns f as the message's file now, counting the calls. A
// second call fails, so a retry loop without a limit ends at once instead
// of running until the test times out.
func (x *refetchFixture) refetch(f MediaFile) func(context.Context) (MediaFile, error) {
	return func(context.Context) (MediaFile, error) {
		x.fetches++
		if x.fetches > 1 {
			return MediaFile{}, errors.New("refetched more than once")
		}
		return f, nil
	}
}

// An expired reference is refreshed once: the sink is reset, the whole file
// is fetched again with the new reference, and the result is exact.
func TestDownloadRefetchesExpiredReference(t *testing.T) {
	x := newRefetchFixture()
	sink := &memSink{}
	err := x.s.DownloadFile(context.Background(), x.file, sink, x.refetch(docFile(7, newRef, int64(len(x.data)))))
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if x.fetches != 1 || sink.resets != 1 {
		t.Errorf("refetches %d, resets %d; want 1 and 1", x.fetches, sink.resets)
	}
	if !bytes.Equal(sink.buf, x.data) {
		t.Errorf("content differs: %d bytes, want %d", len(sink.buf), len(x.data))
	}
}

// A reference that expires again after the refetch is not chased: the
// download fails with the expiry, which Explain turns into its hint.
func TestDownloadSecondExpiryFails(t *testing.T) {
	x := newRefetchFixture()
	err := x.s.DownloadFile(context.Background(), x.file, &memSink{}, x.refetch(docFile(7, oldRef, int64(len(x.data)))))
	if !tgerr.Is(err, "FILE_REFERENCE_EXPIRED") {
		t.Errorf("err = %v, want FILE_REFERENCE_EXPIRED", err)
	}
	if x.fetches != 1 {
		t.Errorf("refetched %d times, want 1", x.fetches)
	}
}

// If the message now holds another file, or the same id with another size,
// nothing more is downloaded or reset: the new file would be written under
// the old name and the old size limit.
func TestDownloadRefetchedFileChanged(t *testing.T) {
	for name, now := range map[string]func(size int64) MediaFile{
		"other document": func(size int64) MediaFile { return docFile(8, newRef, size) },
		"other size":     func(size int64) MediaFile { return docFile(7, newRef, size+1) },
		"photo instead": func(size int64) MediaFile {
			return MediaFile{Info: MediaInfo{Size: size}, DC: 4,
				Location: &tg.InputPhotoFileLocation{ID: 7, AccessHash: 1, FileReference: newRef, ThumbSize: "y"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newRefetchFixture()
			sink := &memSink{}
			err := x.s.DownloadFile(context.Background(), x.file, sink, x.refetch(now(int64(len(x.data)))))
			if !errors.Is(err, ErrFileChanged) {
				t.Errorf("err = %v, want ErrFileChanged", err)
			}
			if sink.resets != 0 {
				t.Errorf("sink reset %d times, want 0", sink.resets)
			}
		})
	}
}

// A sink that refuses Reset (fsguard's ErrWriteInFlight: a write of the
// first attempt is still running) ends the download at once: no second
// attempt, no second Reset.
func TestDownloadResetRefusedIsFatal(t *testing.T) {
	x := newRefetchFixture()
	refused := errors.New("reset while a part is still being written")
	var requestsAtReset int
	sink := &memSink{onReset: func() error {
		requestsAtReset = x.dc.calls()
		return refused
	}}
	err := x.s.DownloadFile(context.Background(), x.file, sink, x.refetch(docFile(7, newRef, int64(len(x.data)))))
	if !errors.Is(err, refused) {
		t.Errorf("err = %v, want the Reset error", err)
	}
	if sink.resets != 1 {
		t.Errorf("Reset called %d times, want 1", sink.resets)
	}
	if n := x.dc.calls(); n != requestsAtReset {
		t.Errorf("%d requests after the refused Reset, want 0", n-requestsAtReset)
	}
}
