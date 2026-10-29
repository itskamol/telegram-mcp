package tele

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// floodPaths runs f once on a file behind a DC pool and once on a file on
// the home DC: both must treat flood waits the same. Each gets its own copy
// of floods, since a fake DC answers each flood once.
func floodPaths(t *testing.T, floods map[int64]string, f func(t *testing.T, s *Service, file MediaFile, dc *fileDC)) {
	data := fileData(1000)
	loc := &tg.InputDocumentFileLocation{ID: 1}
	t.Run("pool", func(t *testing.T) {
		far := &fileDC{data: data, floods: maps.Clone(floods)}
		pools := &fakePools{dcs: map[int]*fileDC{4: far}, this: homeDC}
		s := &Service{API: tg.NewClient(&fileDC{}), Pools: pools}
		f(t, s, MediaFile{Info: MediaInfo{Size: 1000}, Location: loc, DC: 4}, far)
	})
	t.Run("home", func(t *testing.T) {
		home := &fileDC{data: data, floods: maps.Clone(floods)}
		s := &Service{API: tg.NewClient(home), Pools: &fakePools{this: homeDC}}
		f(t, s, MediaFile{Info: MediaInfo{Size: 1000}, Location: loc, DC: homeDC}, home)
	})
}

// A short flood wait is slept through and the part requested again. gotd
// sleeps it with the real clock (wait + 1 s), so this test takes ~2 s per
// path.
func TestDownloadShortFloodWaitRetried(t *testing.T) {
	floodPaths(t, map[int64]string{0: "FLOOD_WAIT_1"}, func(t *testing.T, s *Service, f MediaFile, dc *fileDC) {
		if err := s.DownloadFile(context.Background(), f, &memSink{}, noRefetch); err != nil {
			t.Fatalf("DownloadFile: %v", err)
		}
		if n := dc.offsets()[0]; n != 2 {
			t.Errorf("offset 0 requested %d times, want 2 (flood, then retry)", n)
		}
	})
}

// A flood wait over a minute fails the download at once with a FloodError,
// instead of gotd sleeping through it until the context runs out.
func TestDownloadLongFloodWaitFailsFast(t *testing.T) {
	for _, typ := range []string{"FLOOD_WAIT", "FLOOD_PREMIUM_WAIT"} {
		t.Run(typ, func(t *testing.T) {
			floodPaths(t, map[int64]string{0: typ + "_120"}, func(t *testing.T, s *Service, f MediaFile, _ *fileDC) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err := s.DownloadFile(ctx, f, &memSink{}, noRefetch)
				var flood *FloodError
				if !errors.As(err, &flood) || flood.Seconds != 120 {
					t.Fatalf("err = %v, want a FloodError of 120 s", err)
				}
				if errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("err = %v: the wait was slept until the deadline", err)
				}
			})
		})
	}
}

func TestExplainFloodError(t *testing.T) {
	err := Explain(fmt.Errorf("download: %w", &FloodError{Seconds: 120}))
	if !strings.Contains(err.Error(), "wait 120 seconds") {
		t.Errorf("Explain = %q, want the wait in seconds", err)
	}
}

// errInvoker fails every call with err.
type errInvoker struct{ err error }

func (e errInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error { return e.err }

// The limit itself, without a download (whose gotd retry sleeps on the
// real clock): 60 s is still slept through, 61 s is not.
func TestFloodLimitBoundary(t *testing.T) {
	for _, c := range []struct {
		typ  string
		fast bool
	}{
		{"FLOOD_WAIT_60", false},
		{"FLOOD_WAIT_61", true},
		{"FLOOD_PREMIUM_WAIT_60", false},
		{"FLOOD_PREMIUM_WAIT_61", true},
	} {
		orig := tgerr.New(420, c.typ)
		err := floodLimit{errInvoker{orig}}.Invoke(context.Background(), &tg.UploadGetFileRequest{}, &tg.UploadFileBox{})
		var flood *FloodError
		if got := errors.As(err, &flood); got != c.fast {
			t.Errorf("%s: FloodError = %v, want %v (err %v)", c.typ, got, c.fast, err)
		}
		if !c.fast && !errors.Is(err, orig) {
			t.Errorf("%s: err = %v, want the original error passed through", c.typ, err)
		}
	}
	other := errors.New("other")
	if err := (floodLimit{errInvoker{other}}).Invoke(context.Background(), &tg.UploadGetFileRequest{}, &tg.UploadFileBox{}); err != other {
		t.Errorf("non-flood error: %v, want it unchanged", err)
	}
}
