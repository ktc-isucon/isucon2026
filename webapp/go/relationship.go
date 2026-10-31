package isureal

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type FriendRequest struct {
	ID          string    `db:"id"`
	RequesterID string    `db:"requester_id"`
	RecipientID string    `db:"recipient_id"`
	Status      string    `db:"status"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

func getFriendRequest(ctx context.Context, db sqlx.QueryerContext, id string) (FriendRequest, error) {
	var request FriendRequest
	err := sqlx.GetContext(ctx, db, &request, `SELECT id, requester_id, recipient_id, status, created_at, updated_at FROM friend_requests WHERE id=$1`, id)
	return request, err
}

func beginRelationshipTx(ctx context.Context, db *sqlx.DB, actor, target string) (*sqlx.Tx, error) {
	if actor == target || target == "" {
		return nil, NewAPIError(http.StatusBadRequest, "invalid_relationship")
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// 2人分のロックを1往復で取る。
	// - 同じ2人の間の操作(友達リクエスト・フォロー・ブロック)は、ペア単位のadvisory lockで直列にする
	// - users行は FOR SHARE: 退会(UPDATE)やコメント等の FOR NO KEY UPDATE とは衝突させつつ、
	//   別のペアの関係操作どうしでは同じユーザーを取り合わない
	// advisory lockはInitPlan(LockRowsより下のOne-Time Filter)で評価され、必ず行ロックより先に取られる。
	// ORDER BY id で行ロックの順も固定し、デッドロックを避ける
	var lockedIDs []string
	if err := tx.SelectContext(ctx, &lockedIDs, `
  SELECT id FROM users
  WHERE id IN ($1, $2) AND status = 'active'
    AND (SELECT count(*) FROM (SELECT pg_advisory_xact_lock(hashtextextended('relationship:' || $1 || ':' || $2, 0))) l) = 1
  ORDER BY id FOR SHARE
 `, min(actor, target), max(actor, target)); err != nil {
		tx.Rollback()
		return nil, err
	}
	if len(lockedIDs) != 2 {
		tx.Rollback()
		return nil, NewAPIError(http.StatusNotFound, "relationship_not_found")
	}
	return tx, nil
}

func isBlocked(ctx context.Context, db sqlx.QueryerContext, actor, target string) (bool, error) {
	var blocked bool
	err := sqlx.GetContext(ctx, db, &blocked, `SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))`, actor, target)
	return blocked, err
}

func insertFriendship(ctx context.Context, tx *sqlx.Tx, request FriendRequest) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES ($1,$2,$3,$4)`, uuid.NewString(), min(request.RequesterID, request.RecipientID), max(request.RequesterID, request.RecipientID), request.ID)
	return err
}

func requireRelationshipChange(result sql.Result, err error, status int, code string) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return NewAPIError(status, code)
	}
	return nil
}
