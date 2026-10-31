package isureal

import (
	"context"
	"log"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// 読み取り専用リクエストを手元の非同期レプリカで処理するための仕組み(read-index方式)。
//
//  1. リクエストの最初に primary へ pg_current_wal_flush_lsn() を聞く(1往復)。
//  2. 手元のレプリカの pg_last_wal_replay_lsn() がその位置に追いつくまで待つ。
//  3. 追いついたら、そのリクエストの読み取りはすべてレプリカで行う。
//
// synchronous_commit=on なので、成功を返した書き込みのコミット記録はflush位置より前にある。
// よって手順1より前に成功したすべての書き込み(どのリージョンのものでも)は、手順3の読み取りで必ず見える。
// 待ちが上限を超えた場合や、レプリカでエラーになった場合は primary で読む(正しさは変わらない)。
//
// LSNの取得を複数リクエストで共有する最適化はしていない。共有してよいのは
// 「そのリクエストが届いた後に primary で取得したLSN」だけで、そうすると待ちが最大2往復に延びるため。

const (
	replicaWaitTimeout  = 200 * time.Millisecond
	replicaPollInterval = time.Millisecond
	readDBContextKey    = "readDB"
)

type replicaReader struct {
	primary *sqlx.DB
	replica *sqlx.DB
}

func openReplica(dsn string) *sqlx.DB {
	if dsn == "" {
		return nil
	}
	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		log.Printf("failed to open replica, reads go to primary: %v", err)
		return nil
	}
	db.SetMaxOpenConns(30)
	db.SetMaxIdleConns(30)
	return db
}

// waitForReplica は、呼び出し時点の primary のflush位置までレプリカが反映するのを待つ。
// 追いつけばtrue。
func (r *replicaReader) waitForReplica(ctx context.Context) bool {
	var lsn string
	if err := r.primary.GetContext(ctx, &lsn, `SELECT pg_current_wal_flush_lsn()::text`); err != nil {
		return false
	}
	deadline := time.Now().Add(replicaWaitTimeout)
	for {
		var caughtUp bool
		// レプリカでない(リカバリ中でない)場合 pg_last_wal_replay_lsn() はNULLになり、falseとして扱われる
		err := r.replica.GetContext(ctx, &caughtUp, `SELECT COALESCE(pg_last_wal_replay_lsn() >= $1::pg_lsn, false)`, lsn)
		if err != nil {
			return false
		}
		if caughtUp {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(replicaPollInterval):
		}
	}
}

// ReplicaReadMiddleware は、読み取り専用のルートで、レプリカが追いついていればそのリクエストの読み取り先をレプリカにする。
// SessionMiddleware より前に置くこと(セッション確認もレプリカで行うため)。
func ReplicaReadMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if h.Replica != nil && h.Replica.waitForReplica(c.Request().Context()) {
				c.Set(readDBContextKey, h.Replica.replica)
			}
			return next(c)
		}
	}
}

// ReadDB は、そのリクエストの読み取りに使うDBを返す。
// ReplicaReadMiddleware がレプリカを選んだ場合はレプリカ、それ以外は primary。
// 書き込みや行ロックには使わないこと(必ず h.DB を使う)。
func (h *HandlerContext) ReadDB(c *echo.Context) *sqlx.DB {
	if db, ok := c.Get(readDBContextKey).(*sqlx.DB); ok {
		return db
	}
	return h.DB
}
