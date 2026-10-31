package isureal

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

type deleteFriendshipRequest struct {
	UserID string `param:"userId" json:"-"`
}

func deleteFriendshipHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteFriendshipRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2`, min(actor, input.UserID), max(actor, input.UserID))
		if err := requireRelationshipChange(result, err, http.StatusConflict, "relationship_conflict"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type followRequest struct {
	UserID string `param:"userId" json:"-"`
}

func followHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input followRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		if actor == input.UserID || input.UserID == "" {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		// ロック・相手の種別/ブロック/既存フォローの確認・登録を1往復(BEGIN〜COMMIT)で行う。
		// 2文目はロック後のスナップショットで条件を確かめ、満たすときだけ INSERT する
		var locked int
		var inserted bool
		if err := runPipelinedTx(ctx, h.DB,
			relationshipLockStmt(actor, input.UserID, &locked),
			pipelinedStmt{
				sql: `
   WITH st AS (
     SELECT ` + relationshipBothActiveSQL("$1", "$2") + ` AS both_active,
       EXISTS(SELECT 1 FROM users WHERE id = $2 AND status = 'active' AND account_type = 'certified_public') AS certified,
       EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1)) AS blocked,
       EXISTS(SELECT 1 FROM follows WHERE follower_id=$1 AND followee_id=$2) AS already_following
   ), ins AS (
     INSERT INTO follows (id, follower_id, followee_id)
     SELECT $3, $1, $2 FROM st WHERE both_active AND certified AND NOT blocked AND NOT already_following
     RETURNING id
   )
   SELECT EXISTS(SELECT 1 FROM ins)
  `,
				args: []any{actor, input.UserID, uuid.NewString()},
				scan: func(rows pgx.Rows) error {
					var found bool
					return scanFirstRow(&found, &inserted)(rows)
				},
			},
		); err != nil {
			return err
		}
		if locked != 2 {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if !inserted {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type unfollowRequest struct {
	UserID string `param:"userId" json:"-"`
}

func unfollowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input unfollowRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE follower_id=$1 AND followee_id=$2`, actor, input.UserID)
		if err := requireRelationshipChange(result, err, http.StatusConflict, "relationship_conflict"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type blockRequest struct {
	UserID string `param:"userId" json:"-"`
}

func blockHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input blockRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		if actor == input.UserID || input.UserID == "" {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		// ロック・既存確認・ブロック登録・友達/フォロー解除・保留中リクエストの取り消しを1往復(BEGIN〜COMMIT)で行う。
		// 2人とも有効でないとき、または既にブロック済みなら INSERT を行わず、後続の変更も行わない
		var locked, inserted int
		if err := runPipelinedTx(ctx, h.DB, relationshipLockStmt(actor, input.UserID, &locked), pipelinedStmt{
			sql: `
   WITH ins AS (
     INSERT INTO block_relations (id, blocker_id, blocked_id)
     SELECT $1, $2, $3 WHERE ` + relationshipBothActiveSQL("$2", "$3") + `
     ON CONFLICT (blocker_id, blocked_id) DO NOTHING RETURNING id
   ), del_friendships AS (
     DELETE FROM friendships WHERE user_low_id=$4 AND user_high_id=$5 AND EXISTS (SELECT 1 FROM ins)
   ), del_follows AS (
     DELETE FROM follows WHERE ((follower_id=$2 AND followee_id=$3) OR (follower_id=$3 AND followee_id=$2)) AND EXISTS (SELECT 1 FROM ins)
   ), cancel_requests AS (
     UPDATE friend_requests SET status='cancelled', updated_at=clock_timestamp()
     WHERE status='pending' AND ((requester_id=$2 AND recipient_id=$3) OR (requester_id=$3 AND recipient_id=$2)) AND EXISTS (SELECT 1 FROM ins)
   )
   SELECT count(*) FROM ins
  `,
			args: []any{uuid.NewString(), actor, input.UserID, min(actor, input.UserID), max(actor, input.UserID)},
			scan: func(rows pgx.Rows) error {
				var found bool
				return scanFirstRow(&found, &inserted)(rows)
			},
		}); err != nil {
			return err
		}
		if locked != 2 {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if inserted == 0 {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type unblockRequest struct {
	UserID string `param:"userId" json:"-"`
}

func unblockHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input unblockRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2`, actor, input.UserID)
		if err := requireRelationshipChange(result, err, http.StatusForbidden, "relationship_action_forbidden"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
