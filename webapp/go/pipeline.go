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
