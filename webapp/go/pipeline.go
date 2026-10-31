package isureal

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// pipelinedStmt は runPipelinedTx で送る1文。scan が nil なら結果を読まない。
type pipelinedStmt struct {
	sql  string
	args []any
	scan func(rows pgx.Rows) error
}

// runPipelinedTx は BEGIN・各文・COMMIT を pgx のバッチで1回の往復にまとめて送る。
// europe/america では DB まで往復約100msかかるため、文ごとの往復をなくす。
//
// 各文は前の文の結果をアプリで見てから送ることができない。そのため、後続の文は
// 必要な条件(ロックした行が有効か、重複がないか等)を自分の中で確認し、条件を満たさないときは何も変更しないこと。
// 各文は別々の文なので、前の文で取った行ロックの後の新しいスナップショットで評価される。
// scan はアプリ側で返す値を読むだけにし、エラーの判定は runPipelinedTx の後で行う。
func runPipelinedTx(ctx context.Context, db *sqlx.DB, stmts ...pipelinedStmt) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		pc := driverConn.(*stdlib.Conn).Conn()
		batch := &pgx.Batch{}
		batch.Queue("BEGIN")
		for _, s := range stmts {
			batch.Queue(s.sql, s.args...)
		}
		batch.Queue("COMMIT")

		br := pc.SendBatch(ctx, batch)
		err := readPipelinedResults(br, stmts)
		if closeErr := br.Close(); err == nil {
			err = closeErr
		}
		if err != nil && pc.PgConn().TxStatus() != 'I' {
			// 途中で失敗したトランザクションを片付けてから接続をプールに返す
			_, _ = pc.Exec(context.Background(), "ROLLBACK")
		}
		return err
	})
}

func readPipelinedResults(br pgx.BatchResults, stmts []pipelinedStmt) error {
	if _, err := br.Exec(); err != nil { // BEGIN
		return err
	}
	for _, s := range stmts {
		if s.scan == nil {
			if _, err := br.Exec(); err != nil {
				return err
			}
			continue
		}
		rows, err := br.Query()
		if err != nil {
			return err
		}
		err = s.scan(rows)
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			return err
		}
	}
	_, err := br.Exec() // COMMIT
	return err
}

// userScanDest は userColumns の順に並んだ列を読む先を返す。
func userScanDest(u *User) []any {
	return []any{&u.ID, &u.Username, &u.SecretKeyHash, &u.DisplayName, &u.Bio, &u.ProfileImageID, &u.Region, &u.AccountType, &u.Status}
}

// postScanDest は postColumns の順に並んだ列を読む先を返す。
func postScanDest(p *Post) []any {
	return []any{&p.ID, &p.AuthorID, &p.PostingWindowID, &p.ImageID, &p.Caption, &p.Visibility,
		&p.Kind, &p.Latitude, &p.Longitude, &p.Locality, &p.PostedAt, &p.OnTimeDeadline, &p.WindowEndedAt}
}

// postInteractionAllowedSQL は、閲覧者 viewer が投稿 post に対して操作できるかを返すSQLの式を作る。
// loadViewerContext と viewerContext.canView(コメントでは canComment)と同じ判定をSQLで行う。
//   - forComment=false: 閲覧できるか(RealMoji。投稿者本人は対象外)
//   - forComment=true: コメントできるか(投稿者本人は当日に投稿済みなら可。他人は public か友達のときだけ)
func postInteractionAllowedSQL(viewer, post string, forComment bool) string {
	authorRule := "false"
	visibilityRule := `p.visibility = 'public'
          OR (p.visibility = 'friends' AND is_friend.v)
          OR (p.visibility = 'friends_of_friends' AND (is_friend.v OR EXISTS (
                -- 閲覧者の友達 x がいて、x と投稿者が友達(loadViewerContext の FriendsOfFriends と同じ定義)
                SELECT 1 FROM friendships vf
                CROSS JOIN LATERAL (SELECT CASE WHEN vf.user_low_id = v.id THEN vf.user_high_id ELSE vf.user_low_id END AS x) fx
                JOIN friendships af ON af.user_low_id = least(fx.x, a.id) AND af.user_high_id = greatest(fx.x, a.id)
                WHERE vf.user_low_id = v.id OR vf.user_high_id = v.id)))`
	if forComment {
		authorRule = "true"
		visibilityRule = `p.visibility = 'public' OR (p.visibility IN ('friends', 'friends_of_friends') AND is_friend.v)`
	}
	return `EXISTS (
    SELECT 1
    FROM users v
    JOIN posts p ON p.id = ` + post + ` AND p.deleted_at IS NULL
    JOIN posting_windows w ON w.id = p.posting_window_id AND w.ended_at IS NULL
    JOIN users a ON a.id = p.author_id AND a.status = 'active'
    CROSS JOIN LATERAL (SELECT EXISTS (
      SELECT 1 FROM friendships f
      WHERE f.user_low_id = least(v.id, a.id) AND f.user_high_id = greatest(v.id, a.id)) AS v) is_friend
    WHERE v.id = ` + viewer + ` AND v.status = 'active'
      AND EXISTS (
        SELECT 1 FROM posts vp
        JOIN posting_windows posted ON posted.id = vp.posting_window_id
        JOIN posting_windows cur ON cur.region = v.region AND cur.ended_at IS NULL AND cur.local_date = posted.local_date
        WHERE vp.author_id = v.id AND vp.deleted_at IS NULL)
      AND (
        (a.id = v.id AND ` + authorRule + `)
        OR (a.id <> v.id
          AND NOT EXISTS (SELECT 1 FROM block_relations b
                          WHERE (b.blocker_id = v.id AND b.blocked_id = a.id) OR (b.blocker_id = a.id AND b.blocked_id = v.id))
          AND (` + visibilityRule + `))
      )
  )`
}

// scanFirstRow は最初の1行だけを dest に読み、行があったかを found に入れる。
func scanFirstRow(found *bool, dest ...any) func(pgx.Rows) error {
	return func(rows pgx.Rows) error {
		if !rows.Next() {
			return nil
		}
		*found = true
		return rows.Scan(dest...)
	}
}
