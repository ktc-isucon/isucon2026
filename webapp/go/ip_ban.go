package isureal

import (
	"context"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

const ipBanReloadInterval = 200 * time.Millisecond

type IPBanList struct {
	reloading sync.Mutex
	snapshot  atomic.Pointer[ipBanSnapshot]

	// このサーバーで BAN して、まだ DB から読み直せていないかもしれない分(reloading で保護)
	local map[string]time.Time
}

// addLocal は、このサーバーで BAN した IP をすぐにスナップショットへ反映する。
// DB への書き込みより前の Reload で消えないよう、期限まで local にも残して読み直しのたびに足す。
func (l *IPBanList) addLocal(ip string, until time.Time) {
	l.reloading.Lock()
	defer l.reloading.Unlock()
	if l.local == nil {
		l.local = make(map[string]time.Time)
	}
	if until.After(l.local[ip]) {
		l.local[ip] = until
	}
	current := l.snapshot.Load()
	bans := make(map[string]time.Time)
	if current != nil {
		for k, v := range current.bans {
			bans[k] = v
		}
	}
	if until.After(bans[ip]) {
		bans[ip] = until
	}
	l.snapshot.Store(&ipBanSnapshot{loadedAt: time.Now(), bans: bans})
}

// dropLocal は、DB に書けた BAN の控えを捨てる。スナップショットにはそのまま残し、次の読み直しで DB の値に置き換わる。
func (l *IPBanList) dropLocal(ip string) {
	l.reloading.Lock()
	delete(l.local, ip)
	l.reloading.Unlock()
}

// resetLocal は、初期化(ip_bans の TRUNCATE)のときに、このサーバーの BAN の控えを捨てる。
func (l *IPBanList) resetLocal() {
	l.reloading.Lock()
	l.local = nil
	l.reloading.Unlock()
}

type ipBanSnapshot struct {
	loadedAt time.Time
	bans     map[string]time.Time
}

type ipBan struct {
	IP          string    `db:"ip"`
	BannedUntil time.Time `db:"banned_until"`
}

// Runは、ip_bansに書かれたBAN(他サーバーのアプリが書いたものを含む)を ipBanReloadInterval ごとに裏で読み直す。
// リクエストはメモリ上のスナップショットを見るだけなので、読み直しのDB往復を待たない。
func (l *IPBanList) Run(ctx context.Context, db *sqlx.DB) {
	ticker := time.NewTicker(ipBanReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := l.Reload(ctx, db); err != nil && ctx.Err() == nil {
			log.Printf("failed to reload ip bans: %v", err)
		}
	}
}

// Reloadは、ip_bansをすぐに読み直す。起動時と初期化(ip_bansのTRUNCATE)の直後にも呼ぶ。
func (l *IPBanList) Reload(ctx context.Context, db *sqlx.DB) error {
	l.reloading.Lock()
	defer l.reloading.Unlock()
	loaded, err := loadIPBans(ctx, db)
	if err != nil {
		return err
	}
	now := time.Now()
	for ip, until := range l.local {
		if !now.Before(until) {
			delete(l.local, ip)
			continue
		}
		if until.After(loaded.bans[ip]) {
			loaded.bans[ip] = until
		}
	}
	l.snapshot.Store(loaded)
	return nil
}

func (l *IPBanList) Banned(ip string) bool {
	snapshot := l.snapshot.Load()
	if snapshot == nil {
		return false
	}
	until, ok := snapshot.bans[ip]
	return ok && time.Now().Before(until)
}

func loadIPBans(ctx context.Context, db *sqlx.DB) (*ipBanSnapshot, error) {
	var rows []ipBan
	if err := db.SelectContext(ctx, &rows, `SELECT ip, banned_until FROM ip_bans WHERE banned_until > clock_timestamp()`); err != nil {
		return nil, err
	}
	bans := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		bans[row.IP] = row.BannedUntil
	}
	return &ipBanSnapshot{loadedAt: time.Now(), bans: bans}, nil
}

func extractClientIP(req *http.Request) string {
	if ip := req.Header.Get("X-Client-IP"); ip != "" {
		return ip
	}
	return echo.ExtractIPDirect()(req)
}

func IPBanMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if h.IPBans.Banned(c.RealIP()) {
				return NewAPIError(http.StatusForbidden, "ip_banned")
			}
			return next(c)
		}
	}
}
