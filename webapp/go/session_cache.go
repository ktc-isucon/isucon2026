package isureal

import (
	"context"
	"sync"

	"github.com/jmoiron/sqlx"
)

// sessionInvalidateChannel はセッションの無効化を全サーバーへ知らせる LISTEN/NOTIFY のチャネル名。
// ペイロードはユーザーID。sessionInvalidateAll は全消去を表す。
const (
	sessionInvalidateChannel = "session_invalidate"
	sessionInvalidateAll     = "*"
)

// sessionCache は有効なセッション(token_hash -> Session)をサーバーのメモリに持つ。
// セッションを無効にする書き込み(ログイン置き換え・ログアウト・退会・リージョン変更・初期化)は
// トランザクション内で pg_notify を送り、各サーバーは LISTEN で受け取って消す。
// LISTEN が繋がっていない間は通知を取りこぼすので、キャッシュを使わない。
type sessionCache struct {
	mu      sync.Mutex
	enabled bool
	// epoch は無効化のたびに進む。DBを読む前の値と保存時の値が違えば、読んだ値は古い可能性があるので保存しない
	epoch  uint64
	byHash map[string]Session
	byUser map[string]map[string]struct{}
}

var sessions = &sessionCache{
	byHash: map[string]Session{},
	byUser: map[string]map[string]struct{}{},
}

// begin はキャッシュを引き、なければDBを読む前の世代を返す。
func (c *sessionCache) begin(tokenHash string) (session Session, hit bool, epoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return Session{}, false, c.epoch
	}
	session, hit = c.byHash[tokenHash]
	return session, hit, c.epoch
}

// put はDBから読んだ有効なセッションを保存する。読む間に無効化があった場合は保存しない。
func (c *sessionCache) put(epoch uint64, session Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || epoch != c.epoch {
		return
	}
	session.Token = ""
	c.byHash[session.TokenHash] = session
	hashes, ok := c.byUser[session.UserID]
	if !ok {
		hashes = map[string]struct{}{}
		c.byUser[session.UserID] = hashes
	}
	hashes[session.TokenHash] = struct{}{}
}

// invalidateUser はユーザーのセッションをすべて消す。
func (c *sessionCache) invalidateUser(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	for hash := range c.byUser[userID] {
		delete(c.byHash, hash)
	}
	delete(c.byUser, userID)
}

// clear はキャッシュをすべて消す。
func (c *sessionCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
}

func (c *sessionCache) clearLocked() {
	c.epoch++
	c.byHash = map[string]Session{}
	c.byUser = map[string]map[string]struct{}{}
}

// setEnabled は LISTEN の接続状態に合わせてキャッシュを使うかを切り替える。どちらに切り替えても中身は捨てる。
func (c *sessionCache) setEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
	c.clearLocked()
}

// handleNotification は LISTEN で受け取った無効化の通知を反映する。
func (c *sessionCache) handleNotification(payload string) {
	if payload == sessionInvalidateAll {
		c.clear()
		return
	}
	c.invalidateUser(payload)
}

// notifyAllSessionsInvalidated は全サーバーのキャッシュを消すよう通知する(初期化用)。
func notifyAllSessionsInvalidated(ctx context.Context, db sqlx.ExecerContext) error {
	_, err := db.ExecContext(ctx, `SELECT pg_notify('`+sessionInvalidateChannel+`', '`+sessionInvalidateAll+`')`)
	return err
}

// notifySessionInvalidated は userID のセッションを全サーバーで消すよう通知する。
// トランザクション内で呼べば、通知はコミット時に届く。
func notifySessionInvalidated(ctx context.Context, db sqlx.ExecerContext, userID string) error {
	_, err := db.ExecContext(ctx, `SELECT pg_notify('`+sessionInvalidateChannel+`', $1::text)`, userID)
	return err
}
