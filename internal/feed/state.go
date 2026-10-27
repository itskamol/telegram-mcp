package feed

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gotd/td/telegram/updates"

	"github.com/itskamol/telegram-mcp-go/internal/storage"
)

const stateSchema = `
CREATE TABLE IF NOT EXISTS update_state (
	account TEXT PRIMARY KEY,
	pts INTEGER NOT NULL, qts INTEGER NOT NULL, date INTEGER NOT NULL, seq INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS update_channel_pts (
	account    TEXT    NOT NULL,
	channel_id INTEGER NOT NULL,
	pts        INTEGER NOT NULL,
	PRIMARY KEY (account, channel_id)
);
`

// State persists gotd's update state (pts, qts, date, seq and per-channel
// pts) next to the buffer, and serves the access hashes gotd needs from
// the peer cache. It is scoped to one account; gotd's userID is ignored.
//
// Writes on a cancelled context are refused: during shutdown or after a
// fatal buffering error, the pts of an update that may not have been
// buffered must not be saved, so Telegram redelivers it after restart.
type State struct {
	db      *storage.DB
	peers   *storage.Peers
	account string
}

var (
	_ updates.StateStorage        = (*State)(nil)
	_ updates.ChannelAccessHasher = (*State)(nil)
	_ updates.UserAccessHasher    = (*State)(nil)
)

// NewState creates the tables if needed.
func NewState(ctx context.Context, db *storage.DB, peers *storage.Peers, account string) (*State, error) {
	if _, err := db.SQL.ExecContext(ctx, stateSchema); err != nil {
		return nil, err
	}
	return &State{db: db, peers: peers, account: account}, nil
}

func (s *State) GetState(ctx context.Context, _ int64) (updates.State, bool, error) {
	var st updates.State
	err := s.db.SQL.QueryRowContext(ctx, `SELECT pts, qts, date, seq FROM update_state WHERE account = ?`, s.account).
		Scan(&st.Pts, &st.Qts, &st.Date, &st.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return updates.State{}, false, nil
	}
	return st, err == nil, err
}

func (s *State) SetState(ctx context.Context, _ int64, st updates.State) error {
	return s.exec(ctx, `
		INSERT INTO update_state (account, pts, qts, date, seq) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (account) DO UPDATE SET pts = excluded.pts, qts = excluded.qts, date = excluded.date, seq = excluded.seq`,
		s.account, st.Pts, st.Qts, st.Date, st.Seq)
}

func (s *State) SetPts(ctx context.Context, _ int64, v int) error { return s.setColumn(ctx, "pts", v) }
func (s *State) SetQts(ctx context.Context, _ int64, v int) error { return s.setColumn(ctx, "qts", v) }
func (s *State) SetDate(ctx context.Context, _ int64, v int) error {
	return s.setColumn(ctx, "date", v)
}
func (s *State) SetSeq(ctx context.Context, _ int64, v int) error { return s.setColumn(ctx, "seq", v) }

func (s *State) SetDateSeq(ctx context.Context, _ int64, date, seq int) error {
	return s.mustUpdate(ctx, `UPDATE update_state SET date = ?, seq = ? WHERE account = ?`, date, seq, s.account)
}

// setColumn's name is one of four constants above, never user input.
func (s *State) setColumn(ctx context.Context, column string, v int) error {
	return s.mustUpdate(ctx, `UPDATE update_state SET `+column+` = ? WHERE account = ?`, v, s.account)
}

func (s *State) mustUpdate(ctx context.Context, q string, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := s.db.SQL.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("update state not initialized")
	}
	return nil
}

func (s *State) GetChannelPts(ctx context.Context, _, channelID int64) (int, bool, error) {
	var pts int
	err := s.db.SQL.QueryRowContext(ctx, `SELECT pts FROM update_channel_pts WHERE account = ? AND channel_id = ?`,
		s.account, channelID).Scan(&pts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return pts, err == nil, err
}

func (s *State) SetChannelPts(ctx context.Context, _, channelID int64, pts int) error {
	return s.exec(ctx, `
		INSERT INTO update_channel_pts (account, channel_id, pts) VALUES (?, ?, ?)
		ON CONFLICT (account, channel_id) DO UPDATE SET pts = excluded.pts`, s.account, channelID, pts)
}

// ForEachChannels reads every row first and calls f afterwards: f may call
// SetChannelPts, which on a single-connection pool would otherwise wait
// for the connection the open result set holds.
func (s *State) ForEachChannels(ctx context.Context, _ int64, f func(ctx context.Context, channelID int64, pts int) error) error {
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT channel_id, pts FROM update_channel_pts WHERE account = ?`, s.account)
	if err != nil {
		return err
	}
	type row struct {
		id  int64
		pts int
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.pts); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range all {
		if err := f(ctx, r.id, r.pts); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) exec(ctx context.Context, q string, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.db.SQL.ExecContext(ctx, q, args...)
	return err
}

func (s *State) SetChannelAccessHash(ctx context.Context, _, channelID, hash int64) error {
	return s.peers.SetAccessHash(ctx, storage.KindChannel, channelID, hash)
}

func (s *State) GetChannelAccessHash(ctx context.Context, _, channelID int64) (int64, bool, error) {
	return s.hash(ctx, storage.KindChannel, channelID)
}

func (s *State) SetUserAccessHash(ctx context.Context, _, userID, hash int64) error {
	return s.peers.SetAccessHash(ctx, storage.KindUser, userID, hash)
}

func (s *State) GetUserAccessHash(ctx context.Context, _, userID int64) (int64, bool, error) {
	return s.hash(ctx, storage.KindUser, userID)
}

func (s *State) hash(ctx context.Context, kind string, id int64) (int64, bool, error) {
	p, err := s.peers.Get(ctx, kind, id)
	if errors.Is(err, storage.ErrPeerNotCached) || (err == nil && p.AccessHash == 0) {
		return 0, false, nil
	}
	return p.AccessHash, err == nil, err
}
