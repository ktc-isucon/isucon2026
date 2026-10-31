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
}

type ipBanSnapshot struct {
	loadedAt time.Time
	bans     map[string]time.Time
}

type ipBan struct {
	IP          string    `db:"ip"`
	BannedUntil time.Time `db:"banned_until"`
}

// Runは、isureal-guardがip_bansに書いたBANを ipBanReloadInterval ごとに裏で読み直す。
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
