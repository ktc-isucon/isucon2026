package isureal

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
)

// ユーザー検索用に、正規化(normalizeSearchText)した username と display_name を users に持つ。
// 部分一致は pg_trgm の GIN インデックスで絞り、最後に Go 側でも今までどおり判定する。
//
// 列は Go で正規化した値で埋める(PostgreSQL の lower などでは NFC(Fold(NFC(x))) を再現できないため)。
// 書くのは、登録(INSERT)、表示名の変更(PATCH /api/me)、起動時と初期化時の一括更新(backfillUserSearchColumns)。
// username は変更されない。初期化(reset.sql)は初期ユーザーの表示名を戻すので、初期化のたびに一括更新で合わせる。

// userSearchSchemaSQL は列・拡張・インデックスを用意する(何度実行してもよい)。
var userSearchSchemaSQL = []string{
	`CREATE EXTENSION IF NOT EXISTS pg_trgm`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS search_username text, ADD COLUMN IF NOT EXISTS search_display_name text`,
	`CREATE INDEX IF NOT EXISTS users_search_username_trgm ON users USING gin (search_username gin_trgm_ops)`,
	`CREATE INDEX IF NOT EXISTS users_search_display_name_trgm ON users USING gin (search_display_name gin_trgm_ops)`,
}

// userSearchMigrationLock は、複数のサーバーが同時に起動しても DDL が衝突しないようにする advisory lock のキー
const userSearchMigrationLock = 7302026001

// userSearchReady は、列があり一括更新も済んだとき true。false の間は今までの全件取得の検索を使う
var userSearchReady atomic.Bool

// prepareUserSearch は起動時に、列とインデックスを用意し、正規化した値を埋める。
// 失敗したときは今までの検索のまま動く(userSearchReady が false のまま)。
func prepareUserSearch(ctx context.Context, db *sqlx.DB) error {
	conn, err := db.Connx(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, userSearchMigrationLock); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, userSearchMigrationLock) //nolint:errcheck
	for _, stmt := range userSearchSchemaSQL {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("user search schema: %w", err)
		}
	}
	if err := backfillUserSearchColumns(ctx, db); err != nil {
		return err
	}
	userSearchReady.Store(true)
	return nil
}

// backfillUserSearchColumns は、正規化した値が今の username・display_name と食い違う行(NULL を含む)だけを更新する。
func backfillUserSearchColumns(ctx context.Context, db sqlx.ExtContext) error {
	var rows []struct {
		ID                string  `db:"id"`
		Username          string  `db:"username"`
		DisplayName       string  `db:"display_name"`
		SearchUsername    *string `db:"search_username"`
		SearchDisplayName *string `db:"search_display_name"`
	}
	if err := sqlx.SelectContext(ctx, db, &rows, `SELECT id, username, display_name, search_username, search_display_name FROM users`); err != nil {
		return fmt.Errorf("load users for search: %w", err)
	}
	var ids, usernames, displayNames []string
	for _, row := range rows {
		su, sd := normalizeSearchText(row.Username), normalizeSearchText(row.DisplayName)
		if row.SearchUsername != nil && *row.SearchUsername == su && row.SearchDisplayName != nil && *row.SearchDisplayName == sd {
			continue
		}
		ids = append(ids, row.ID)
		usernames = append(usernames, su)
		displayNames = append(displayNames, sd)
	}
	const batch = 1000
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		if _, err := db.ExecContext(ctx, `
   UPDATE users AS u SET search_username = v.su, search_display_name = v.sd
   FROM unnest($1::text[], $2::text[], $3::text[]) AS v(id, su, sd)
   WHERE u.id = v.id
  `, ids[start:end], usernames[start:end], displayNames[start:end]); err != nil {
			return fmt.Errorf("update user search columns: %w", err)
		}
	}
	return nil
}

// likeContainsPattern は正規化済みの q を LIKE '%q%' (ESCAPE '\') のパターンにする
func likeContainsPattern(normalized string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(normalized) + "%"
}
