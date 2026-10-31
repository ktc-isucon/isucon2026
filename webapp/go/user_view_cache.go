package isureal

import "sync"

// userViewCache は、本人しか読まないデータ(今の投稿期間とその日の投稿状況、自分の情報、自分の今日の投稿)を
// ユーザーごとにサーバーのメモリに持つ。
//
// 本人のリクエストは RegionMiddleware で必ず本人のリージョンのサーバーに来るので、本人による書き込みは
// そのサーバーでコミット後に invalidateUser する。別のサーバーで起きる変更は NOTIFY で受け取って消す。
//   - 投稿期間の開始(admin): posting_window の NOTIFY で全消去
//   - リージョン変更・退会・認証アカウント化・初期化: session_invalidate の NOTIFY でユーザーごと(または全)消去
//
// LISTEN が繋がっていない間は通知を取りこぼすので、キャッシュを使わない(sessionCache と同じ)。
type userViewCache struct {
	mu      sync.Mutex
	enabled bool
	// epoch は無効化のたびに進む。DBを読む前の値と保存時の値が違えば、読んだ値は古い可能性があるので保存しない
	epoch  uint64
	byUser map[string]*userViewEntry
}

type userViewEntry struct {
	window *cachedPostingWindowState
	me     *User
	myPost *cachedMyPost
}

// cachedPostingWindowState は GET /api/posting-window/current のDBの結果。found=false は投稿期間なし。
type cachedPostingWindowState struct {
	found   bool
	window  PostingWindow
	active  int
	deleted int
}

// cachedMyPost は GET /api/posts/me/current のDBの結果。found=false は今日の投稿なし。
type cachedMyPost struct {
	found bool
	post  PostWithAuthor
}

var userViews = &userViewCache{byUser: map[string]*userViewEntry{}}

// begin はDBを読む前の世代と、キャッシュ済みの値(あれば)を返す。
func (c *userViewCache) begin(userID string) (entry userViewEntry, epoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return userViewEntry{}, c.epoch
	}
	if e, ok := c.byUser[userID]; ok {
		entry = *e
	}
	return entry, c.epoch
}

// put はDBから読んだ値を保存する。読む間に無効化があった場合は保存しない。
func (c *userViewCache) put(epoch uint64, userID string, update func(e *userViewEntry)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || epoch != c.epoch {
		return
	}
	e, ok := c.byUser[userID]
	if !ok {
		e = &userViewEntry{}
		c.byUser[userID] = e
	}
	update(e)
}

func (c *userViewCache) putWindow(epoch uint64, userID string, state cachedPostingWindowState) {
	c.put(epoch, userID, func(e *userViewEntry) { e.window = &state })
}

func (c *userViewCache) putMe(epoch uint64, userID string, user User) {
	c.put(epoch, userID, func(e *userViewEntry) { e.me = &user })
}

func (c *userViewCache) putMyPost(epoch uint64, userID string, post cachedMyPost) {
	c.put(epoch, userID, func(e *userViewEntry) { e.myPost = &post })
}

// invalidateUser はユーザーの値をすべて消す。
func (c *userViewCache) invalidateUser(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	delete(c.byUser, userID)
}

// clear はキャッシュをすべて消す。
func (c *userViewCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
}

func (c *userViewCache) clearLocked() {
	c.epoch++
	c.byUser = map[string]*userViewEntry{}
}

// setEnabled は LISTEN の接続状態に合わせてキャッシュを使うかを切り替える。どちらに切り替えても中身は捨てる。
func (c *userViewCache) setEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
	c.clearLocked()
}

// handleSessionNotification は session_invalidate の通知(ユーザーID、または全消去)を反映する。
func (c *userViewCache) handleSessionNotification(payload string) {
	if payload == sessionInvalidateAll {
		c.clear()
		return
	}
	c.invalidateUser(payload)
}
