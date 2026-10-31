package isureal

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jmoiron/sqlx"
)

// postingWindowChannel は投稿期間の開始を知らせる LISTEN/NOTIFY のチャネル名(ペイロードはリージョン)
const postingWindowChannel = "posting_window"

// notificationWatcher はサーバーごとに1本だけDBを監視し、SSE接続ごとのポーリングを置き換える。
// 投稿期間の開始は LISTEN/NOTIFY で即座に受け取り、取りこぼし対策として2秒おきにも確認する。
// SSE接続中のユーザーは500msおきにまとめて確認し、無効になったユーザーの接続に知らせる。
type notificationWatcher struct {
	db  *sqlx.DB
	dsn string
	// refresh は投稿期間の取り直しを依頼する(バッファ1、溢れた分は捨ててよい)
	refresh chan struct{}

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

func newNotificationWatcher(db *sqlx.DB, dsn string) *notificationWatcher {
	return &notificationWatcher{
		db:       db,
		dsn:      dsn,
		refresh:  make(chan struct{}, 1),
		changed:  map[string]chan struct{}{},
		watchers: map[*notificationSubscriber]struct{}{},
	}
}

func (w *notificationWatcher) run(ctx context.Context) {
	go w.listen(ctx)
	go func() {
		windowTicker := time.NewTicker(2 * time.Second)
		defer windowTicker.Stop()
		for {
			w.pollWindows(ctx)
			select {
			case <-ctx.Done():
				return
			case <-windowTicker.C:
			case <-w.refresh:
			}
		}
	}()
	userTicker := time.NewTicker(500 * time.Millisecond)
	defer userTicker.Stop()
	for {
		w.pollUsers(ctx)
		select {
		case <-ctx.Done():
			return
		case <-userTicker.C:
		}
	}
}

func (w *notificationWatcher) requestRefresh() {
	select {
	case w.refresh <- struct{}{}:
	default:
	}
}

// listen は専用のDB接続で投稿期間の開始通知を待ち、届いたリージョンの購読者にすぐ知らせる。
// 接続が切れたら間隔を広げながら繋ぎ直し、繋ぎ直した直後に投稿期間を取り直す(切断中の取りこぼし対策)。
func (w *notificationWatcher) listen(ctx context.Context) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		if err := w.listenOnce(ctx, func() { backoff = 100 * time.Millisecond }); err != nil && ctx.Err() == nil {
			slog.Error("failed to listen posting window notifications", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (w *notificationWatcher) listenOnce(ctx context.Context, connected func()) error {
	conn, err := pgx.Connect(ctx, w.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+postingWindowChannel); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+sessionInvalidateChannel); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+imageCacheResetChannel); err != nil {
		return err
	}
	// 切れている間に初期化の通知を取りこぼしたかもしれないので、手元のruntime画像キャッシュは捨てる
	resetLocalRuntimeImages()
	// セッションの無効化を受け取れる間だけキャッシュを使う。切れている間の通知は取りこぼすので、繋ぎ直しても中身は捨てる
	sessions.setEnabled(true)
	defer sessions.setEnabled(false)
	userViews.setEnabled(true)
	defer userViews.setEnabled(false)
	connected()
	w.requestRefresh()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if n.Channel == sessionInvalidateChannel {
			sessions.handleNotification(n.Payload)
			userViews.handleSessionNotification(n.Payload)
			continue
		}
		if n.Channel == imageCacheResetChannel {
			resetLocalRuntimeImages()
			continue
		}
		// 投稿期間が変わったので、SSEの購読者に知らせる前に、今の投稿期間と今日の投稿のキャッシュを消す。
		// こうすると、通知を受けて current を読み直したユーザーは必ず新しい投稿期間を読む
		userViews.clear()
		w.mu.Lock()
		if ch, ok := w.changed[n.Payload]; ok {
			close(ch)
			delete(w.changed, n.Payload)
		}
		w.mu.Unlock()
		// 通知で知らせた分の状態を揃えるため、投稿期間の一覧も取り直す
		w.requestRefresh()
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

func (w *notificationWatcher) pollWindows(ctx context.Context) {
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
		if !first && !maps.Equal(w.windows, next) {
			// NOTIFY より先にポーリングで変化に気づいた場合も、念のためキャッシュを消す
			userViews.clear()
		}
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
}

func (w *notificationWatcher) pollUsers(ctx context.Context) {
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
