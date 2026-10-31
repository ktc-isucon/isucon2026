package isureal

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// banGuard は、isureal-guard(/opt/isureal-guard/src/guard.go)と同じ条件・回数・期間で、
// 攻撃を繰り返す IP アドレスをアプリ内で BAN する。guard はジャーナルに出た行動ログを読み直していたが、
// アプリは応答の時点で判定できるので、行動ログの出力とジャーナルの読み直しが要らなくなる。
// guard と同じく、このサーバーで処理したリクエストだけを数える(サーバー間で共有しない)。
type banGuard struct {
	mu          sync.Mutex
	detectors   map[banFailureKind]*banDetector
	banDuration time.Duration

	// attach されるまでは数えるだけで BAN しない(起動直後の DB 接続前)
	db    *sqlx.DB
	bans  *IPBanList
	logf  func(format string, args ...any)
	queue chan string
}

type banFailureKind string

const (
	banLoginFailure     banFailureKind = "login"
	banInvalidImage     banFailureKind = "invalid_image"
	banInvalidSignature banFailureKind = "invalid_signature"
)

func newBanGuardFromEnv() *banGuard {
	threshold := 10
	if v := os.Getenv("GUARD_FAIL_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			threshold = n
		} else {
			log.Printf("invalid GUARD_FAIL_THRESHOLD %q, using %d", v, threshold)
		}
	}
	window := envPositiveDuration("GUARD_FAIL_WINDOW", 10*time.Second)
	banDuration := envPositiveDuration("GUARD_BAN_DURATION", 2*time.Minute)
	return newBanGuard(threshold, window, banDuration)
}

func envPositiveDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("invalid %s %q, using %s", key, v, def)
		return def
	}
	return d
}

func newBanGuard(threshold int, window, banDuration time.Duration) *banGuard {
	return &banGuard{
		detectors: map[banFailureKind]*banDetector{
			banLoginFailure:     newBanDetector(threshold, window),
			banInvalidImage:     newBanDetector(2, 5*time.Second),
			banInvalidSignature: newBanDetector(3, 5*time.Second),
		},
		banDuration: banDuration,
		logf:        log.Printf,
	}
}

// attach は DB と BAN 一覧がそろってから呼ぶ。BAN の書き込みは裏の goroutine で順に行う。
func (g *banGuard) attach(ctx context.Context, db *sqlx.DB, bans *IPBanList) {
	g.mu.Lock()
	g.db = db
	g.bans = bans
	g.queue = make(chan string, 1024)
	queue := g.queue
	g.mu.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ip := <-queue:
				if err := g.writeBan(ctx, ip); err != nil {
					if ctx.Err() == nil {
						g.logf("ban failed: ip=%s: %v", ip, err)
					}
					continue
				}
				// DB に書けたら、以後は ip_bans の読み直しに任せる(初期化の TRUNCATE で消えるように控えを捨てる)
				g.bans.dropLocal(ip)
			}
		}
	}()
}

// classifyBanFailure は guard.go の parseFailure と同じ条件で、BAN の対象になる失敗を返す。
func classifyBanFailure(action string, status int, ip, abuse string) banFailureKind {
	if ip == "" {
		return ""
	}
	switch {
	case action == "login" && status == http.StatusUnauthorized:
		return banLoginFailure
	case action == "put_profile_image" && status == http.StatusBadRequest && abuse == string(banInvalidImage):
		return banInvalidImage
	case action == "get_image" && status == http.StatusForbidden && abuse == string(banInvalidSignature):
		return banInvalidSignature
	}
	return ""
}

// observe は1件の応答を記録し、閾値に達したら BAN する。BAN したら true を返す。
func (g *banGuard) observe(action string, status int, ip, abuse string, at time.Time) bool {
	kind := classifyBanFailure(action, status, ip, abuse)
	if kind == "" {
		return false
	}
	g.mu.Lock()
	banned := g.detectors[kind].observe(ip, at)
	bans, queue := g.bans, g.queue
	g.mu.Unlock()
	if !banned {
		return false
	}
	g.logf("banned: ip=%s reason=%s", ip, kind)
	if bans != nil {
		// DB に書く前から、このサーバーではすぐに BAN を効かせる(期限は DB に書く値の近似)
		bans.addLocal(ip, at.Add(g.banDuration))
	}
	if queue != nil {
		select {
		case queue <- ip:
		default:
			g.logf("ban queue full: ip=%s", ip)
		}
	}
	return true
}

// writeBan は guard.go の dbBanner.ban と同じ SQL で、期限を DB の時刻で決める。
func (g *banGuard) writeBan(ctx context.Context, ip string) error {
	_, err := g.db.ExecContext(ctx, `
		INSERT INTO ip_bans (ip, banned_until)
		VALUES ($1, clock_timestamp() + make_interval(secs => $2))
		ON CONFLICT (ip) DO UPDATE SET banned_until = GREATEST(ip_bans.banned_until, EXCLUDED.banned_until)
	`, ip, g.banDuration.Seconds())
	return err
}

// banDetector は guard.go の detector と同じ。IP ごとに、集計期間内の失敗の時刻を持つ。
type banDetector struct {
	threshold int
	window    time.Duration
	failures  map[string][]time.Time
	lastSweep time.Time
}

func newBanDetector(threshold int, window time.Duration) *banDetector {
	return &banDetector{threshold: threshold, window: window, failures: make(map[string][]time.Time)}
}

// observe は ip の失敗を記録し、BAN すべきなら true を返す。BAN したら数え直す。
func (d *banDetector) observe(ip string, at time.Time) bool {
	d.sweep(at)
	kept := d.failures[ip][:0]
	for _, failedAt := range d.failures[ip] {
		if at.Sub(failedAt) < d.window {
			kept = append(kept, failedAt)
		}
	}
	kept = append(kept, at)
	if len(kept) >= d.threshold {
		delete(d.failures, ip)
		return true
	}
	d.failures[ip] = kept
	return false
}

// sweep は集計期間ごとに、期間内の失敗がなくなった IP を捨てる。
func (d *banDetector) sweep(now time.Time) {
	if now.Sub(d.lastSweep) < d.window {
		return
	}
	d.lastSweep = now
	for ip, times := range d.failures {
		if now.Sub(times[len(times)-1]) >= d.window {
			delete(d.failures, ip)
		}
	}
}
