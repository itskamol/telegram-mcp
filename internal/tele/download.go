package tele

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

const (
	// downloadPartSize is the largest part upload.getFile serves.
	downloadPartSize = 1 << 20
	// downloadConcurrency is the number of parts requested at once. A DC
	// pool connection carries one request at a time, so pools have exactly
	// this many connections.
	downloadConcurrency = 8
)

// Pools opens connections to other DCs and reports the home DC;
// *telegram.Client satisfies it.
type Pools interface {
	DC(ctx context.Context, dc int, max int64) (telegram.CloseInvoker, error)
	Config() tg.Config
}

// PartSink is where DownloadFile writes; Reset empties it for a restart.
type PartSink interface {
	io.WriterAt
	Reset() error
}

// DownloadFile writes f to w in parallel on f's DC. refetch reloads the
// message when the file reference expired.
func (s *Service) DownloadFile(ctx context.Context, f MediaFile, w PartSink,
	refetch func(context.Context) (MediaFile, error)) error {
	api, err := s.fileAPI(ctx, f.DC)
	if err != nil {
		return err
	}
	_, err = downloader.NewDownloader().WithPartSize(downloadPartSize).
		Download(api, f.Location).WithThreads(downloadConcurrency).Parallel(ctx, w)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	return nil
}

// fileAPI returns the client to fetch a file stored on dc. Files on other
// DCs go straight to a pool there: through the home connection every part
// would first be refused with FILE_MIGRATE. While the home DC is unknown
// (0), or the file's DC is, the home connection is used, so a pool is never
// opened on the home DC with the primary key.
func (s *Service) fileAPI(ctx context.Context, dc int) (downloader.Client, error) {
	if s.Pools == nil || dc == 0 {
		return s.API, nil
	}
	if home := s.Pools.Config().ThisDC; home == 0 || dc == home {
		return s.API, nil
	}

	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if p, ok := s.pools[dc]; ok {
		return tg.NewClient(p), nil
	}
	p, err := s.Pools.DC(ctx, dc, downloadConcurrency)
	if err != nil {
		return nil, fmt.Errorf("connect to DC %d: %w", dc, err)
	}
	if s.pools == nil {
		s.pools = map[int]telegram.CloseInvoker{}
	}
	s.pools[dc] = p
	return tg.NewClient(p), nil
}

// Close closes the DC pools opened for downloads.
func (s *Service) Close() error {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	var errs []error
	for dc, p := range s.pools {
		errs = append(errs, p.Close())
		delete(s.pools, dc)
	}
	return errors.Join(errs...)
}
