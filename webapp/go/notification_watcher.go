package isureal

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// notificationWatcher はサーバーごとに1本だけDBを監視し、SSE接続ごとのポーリングを置き換える。
// 500msおきに有効な投稿期間とSSE接続中のユーザーをまとめて確認し、
// 投稿期間が変わったリージョンの接続と、無効になったユーザーの接続に知らせる。
type notificationWatcher struct {
	db *sqlx.DB

	mu       sync.Mutex
	windows  map[string]string // region -> 有効な投稿期間のID(連結)。nil は未取得
	changed  map[string]chan struct{}
	watchers map[*notificationSubscriber]struct{}
}

type notificationSubscriber struct {
	userID  string
	region  string
	gone    chan struct{}
	endOnce sync.Once
}

func (s *notificationSubscriber) end() {
	s.endOnce.Do(func() { close(s.gone) })
}

func newNotificationWatcher(db *sqlx.DB) *notificationWatcher {
	return &notificationWatcher{
		db:       db,
		changed:  map[string]chan struct{}{},
		watchers: map[*notificationSubscriber]struct{}{},
	}
}

func (w *notificationWatcher) run(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		w.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// changes は region の投稿期間が次に変わったときに閉じられるチャネルを返す。
func (w *notificationWatcher) changes(region string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.changedLocked(region)
}

func (w *notificationWatcher) changedLocked(region string) chan struct{} {
	ch, ok := w.changed[region]
	if !ok {
		ch = make(chan struct{})
		w.changed[region] = ch
	}
	return ch
}

func (w *notificationWatcher) subscribe(userID, region string) *notificationSubscriber {
	s := &notificationSubscriber{userID: userID, region: region, gone: make(chan struct{})}
	w.mu.Lock()
	w.watchers[s] = struct{}{}
	w.mu.Unlock()
	return s
}

func (w *notificationWatcher) unsubscribe(s *notificationSubscriber) {
	w.mu.Lock()
	delete(w.watchers, s)
	w.mu.Unlock()
}

func (w *notificationWatcher) poll(ctx context.Context) {
	var rows []struct {
		ID     string `db:"id"`
		Region string `db:"region"`
	}
	if err := w.db.SelectContext(ctx, &rows, `SELECT id, region FROM posting_windows WHERE ended_at IS NULL`); err != nil {
		if ctx.Err() == nil {
			slog.Error("failed to watch posting windows", "error", err)
		}
	} else {
		ids := map[string][]string{}
		for _, r := range rows {
			ids[r.Region] = append(ids[r.Region], r.ID)
		}
		next := map[string]string{}
		for region, list := range ids {
			slices.Sort(list)
			next[region] = strings.Join(list, ",")
		}

		w.mu.Lock()
		first := w.windows == nil
		for region, ch := range w.changed {
			// 初回取得時は、取得前に接続した購読者に念のため再確認させる
			if first || w.windows[region] != next[region] {
				close(ch)
				delete(w.changed, region)
			}
		}
		w.windows = next
		w.mu.Unlock()
	}

	w.mu.Lock()
	subs := make([]*notificationSubscriber, 0, len(w.watchers))
	userIDs := make([]string, 0, len(w.watchers))
	for s := range w.watchers {
		subs = append(subs, s)
		userIDs = append(userIDs, s.userID)
	}
	w.mu.Unlock()
	if len(subs) == 0 {
		return
	}

	var users []struct {
		ID     string `db:"id"`
		Region string `db:"region"`
	}
	if err := w.db.SelectContext(ctx, &users, `SELECT id, region FROM users WHERE id = ANY($1) AND status = 'active'`, userIDs); err != nil {
		if ctx.Err() == nil {
			slog.Error("failed to watch notification recipients", "error", err)
		}
		return
	}
	regions := make(map[string]string, len(users))
	for _, u := range users {
		regions[u.ID] = u.Region
	}
	for _, s := range subs {
		// ユーザーが削除された、またはリージョンが変わった接続は終える
		if region, ok := regions[s.userID]; !ok || region != s.region {
			s.end()
		}
	}
}
