package isureal

import (
	"sync"
	"time"
)

// deleteImageObjectSQL は画像の行を消し、消せたときだけ、全サーバーの画像の署名鍵のキャッシュから
// 持ち主($2)の画像を消すよう通知する(通知はコミット時に届く)。消した行の created_at を返す。
const deleteImageObjectSQL = `
  WITH d AS (DELETE FROM image_objects WHERE id = $1 RETURNING created_at)
  SELECT d.created_at FROM d
  CROSS JOIN LATERAL (SELECT pg_notify('` + sessionInvalidateChannel + `', $2::text)) AS notify
 `

// imageAuthCacheLimit を超えたら中身を捨てる(メモリの上限。1件あたり200バイト前後)
const imageAuthCacheLimit = 300_000

// imageAuthEntry は画像の署名を検証するのに必要で、作成後に変わらない情報。
// 持ち主・持ち主の secret_key_hash・作成日時・置き場所は、画像IDごとに変わらない。
type imageAuthEntry struct {
	OwnerID       string
	SecretKeyHash string
	CreatedAt     time.Time
	OriginRegion  *string
}

// imageAuthCache は「画像の行があり、持ち主が有効」と確かめた画像の署名鍵を持つ。
// これに当たれば、不正な署名を DB を引かずに弾ける(応答と abuse=invalid_signature は DB を引いた場合と同じ)。
// 閲覧可否は毎回 DB で確かめるので、ここには入れない。
//
// 次の書き込みで「画像の行があり、持ち主が有効」が崩れるので、そのときに消す。
//   - 退会(users.status): 退会のトランザクション内の session_invalidate の NOTIFY と、同じサーバーでの即時の削除
//   - プロフィール画像の行の削除(変更・削除): 同じ文の中の session_invalidate の NOTIFY と、同じサーバーでの即時の削除
//   - 初期化(reset.sql): 全消去と session_invalidate の全消去の NOTIFY
//
// LISTEN が繋がっていない間は通知を取りこぼすので、キャッシュを使わない。
type imageAuthCache struct {
	mu      sync.Mutex
	enabled bool
	// epoch は無効化のたびに進む。DBを読む前の値と保存時の値が違えば、読んだ値は古い可能性があるので保存しない
	epoch   uint64
	byID    map[string]imageAuthEntry
	byOwner map[string]map[string]struct{}
}

var imageAuths = newImageAuthCache()

func newImageAuthCache() *imageAuthCache {
	return &imageAuthCache{
		byID:    map[string]imageAuthEntry{},
		byOwner: map[string]map[string]struct{}{},
	}
}

// begin はキャッシュを引き、DBを読む前の世代を返す。
func (c *imageAuthCache) begin(imageID string) (entry imageAuthEntry, hit bool, epoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return imageAuthEntry{}, false, c.epoch
	}
	entry, hit = c.byID[imageID]
	return entry, hit, c.epoch
}

// put は DB で「画像の行があり、持ち主が有効」と確かめた情報を保存する。読む間に無効化があった場合は保存しない。
func (c *imageAuthCache) put(epoch uint64, imageID string, entry imageAuthEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || epoch != c.epoch {
		return
	}
	if len(c.byID) >= imageAuthCacheLimit {
		c.clearLocked()
		return
	}
	c.byID[imageID] = entry
	ids, ok := c.byOwner[entry.OwnerID]
	if !ok {
		ids = map[string]struct{}{}
		c.byOwner[entry.OwnerID] = ids
	}
	ids[imageID] = struct{}{}
}

// invalidateOwner は持ち主の画像をすべて消す。
func (c *imageAuthCache) invalidateOwner(ownerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	for id := range c.byOwner[ownerID] {
		delete(c.byID, id)
	}
	delete(c.byOwner, ownerID)
}

// clear はキャッシュをすべて消す。
func (c *imageAuthCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
}

func (c *imageAuthCache) clearLocked() {
	c.epoch++
	c.byID = map[string]imageAuthEntry{}
	c.byOwner = map[string]map[string]struct{}{}
}

// setEnabled は LISTEN の接続状態に合わせてキャッシュを使うかを切り替える。どちらに切り替えても中身は捨てる。
func (c *imageAuthCache) setEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
	c.clearLocked()
}

// handleSessionNotification は session_invalidate の通知(ペイロードはユーザーIDか全消去)を反映する。
func (c *imageAuthCache) handleSessionNotification(payload string) {
	if payload == sessionInvalidateAll {
		c.clear()
		return
	}
	c.invalidateOwner(payload)
}
