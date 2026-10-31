package isureal

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createFriendRequestRequest struct {
	RecipientID string `json:"recipientId"`
}

type createFriendRequestResponse struct {
	RelationID string    `json:"relationId"`
	Status     string    `json:"status"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func createFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		if actor == input.RecipientID || input.RecipientID == "" {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		// ロック・ブロック/友達/保留中リクエストの確認・(相手からの保留中があれば)承認と友達作成・(なければ)新規作成を
		// 1往復(BEGIN〜COMMIT)で行う。2文目はロック後のスナップショットで条件を確かめ、満たすときだけ変更する
		var locked int
		var state struct {
			found              bool
			blocked            bool
			friends            bool
			pendingRequesterID *string
		}
		var request FriendRequest
		var requestID *string
		var requestStatus *string
		var requestUpdatedAt *time.Time
		low, high := min(actor, input.RecipientID), max(actor, input.RecipientID)
		if err := runPipelinedTx(ctx, h.DB, relationshipLockStmt(actor, input.RecipientID, &locked), pipelinedStmt{
			sql: `
   WITH st AS (
     SELECT s.both_active, s.blocked, s.friends, r.id AS pending_id, r.requester_id AS pending_requester_id
     FROM (
       SELECT ` + relationshipBothActiveSQL("$1", "$2") + ` AS both_active,
         EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1)) AS blocked,
         EXISTS(SELECT 1 FROM friendships WHERE user_low_id=$3 AND user_high_id=$4) AS friends
     ) s
     LEFT JOIN LATERAL (
       SELECT id, requester_id FROM friend_requests WHERE status='pending' AND
        ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))
       LIMIT 1
     ) r ON true
   ), ok AS (
     SELECT * FROM st WHERE both_active AND NOT blocked AND NOT friends
   ), acc AS (
     UPDATE friend_requests SET status='accepted', updated_at=clock_timestamp()
     WHERE id = (SELECT pending_id FROM ok WHERE pending_requester_id = $2)
     RETURNING id, status, updated_at
   ), fs AS (
     INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
     SELECT $5, $3, $4, id FROM acc
   ), ins AS (
     INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
     SELECT $6, $1, $2, 'pending', clock_timestamp(), clock_timestamp() FROM ok WHERE pending_id IS NULL
     RETURNING id, status, updated_at
   )
   SELECT st.blocked, st.friends, st.pending_requester_id, res.id, res.status, res.updated_at
   FROM st LEFT JOIN (SELECT * FROM acc UNION ALL SELECT * FROM ins) res ON true
  `,
			args: []any{actor, input.RecipientID, low, high, uuid.NewString(), uuid.NewString()},
			scan: scanFirstRow(&state.found, &state.blocked, &state.friends, &state.pendingRequesterID, &requestID, &requestStatus, &requestUpdatedAt),
		}); err != nil {
			return err
		}
		if locked != 2 {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if !state.found {
			return fmt.Errorf("friend request state not found")
		}
		if state.blocked || state.friends {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if state.pendingRequesterID != nil && *state.pendingRequesterID == actor {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if requestID == nil || requestStatus == nil || requestUpdatedAt == nil {
			return fmt.Errorf("friend request was not created")
		}
		request.ID, request.Status, request.UpdatedAt = *requestID, *requestStatus, *requestUpdatedAt
		return c.JSON(http.StatusCreated, createFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}

type transitionFriendRequestRequest struct {
	RequestID string `param:"requestId" json:"-"`
	Action    string `param:"action" json:"-"`
}

type transitionFriendRequestResponse createFriendRequestResponse

func transitionFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input transitionFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		var status string
		switch input.Action {
		case "accept":
			status = "accepted"
		case "reject":
			status = "rejected"
		case "cancel":
			status = "cancelled"
		default:
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		request, err := getFriendRequest(ctx, h.DB, input.RequestID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		if (status == "cancelled" && actor != request.RequesterID) || (status != "cancelled" && actor != request.RecipientID) {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		// ロック・保留中かの再確認・(承認なら)ブロック確認と友達作成・状態の更新を1往復(BEGIN〜COMMIT)で行う。
		// 2文目はロック後のスナップショットで条件を確かめ、満たすときだけ変更する
		var locked int
		var state struct {
			found   bool
			pending bool
			blocked bool
		}
		var updatedID, updatedStatus *string
		var updatedAt *time.Time
		requesterID, recipientID := request.RequesterID, request.RecipientID
		if err := runPipelinedTx(ctx, h.DB, relationshipLockStmt(requesterID, recipientID, &locked), pipelinedStmt{
			sql: `
   WITH st AS (
     SELECT r.status = 'pending' AS pending,
       EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$6 AND blocked_id=$7) OR (blocker_id=$7 AND blocked_id=$6)) AS blocked,
       ` + relationshipBothActiveSQL("$6", "$7") + ` AS both_active
     FROM friend_requests r WHERE r.id = $1
   ), ok AS (
     SELECT 1 FROM st WHERE both_active AND pending AND ($2 <> 'accepted' OR NOT blocked)
   ), fs AS (
     INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
     SELECT $5, $3, $4, $1 FROM ok WHERE $2 = 'accepted'
   ), upd AS (
     UPDATE friend_requests SET status=$2, updated_at=clock_timestamp()
     WHERE id=$1 AND EXISTS (SELECT 1 FROM ok)
     RETURNING id, status, updated_at
   )
   SELECT st.pending, st.blocked, upd.id, upd.status, upd.updated_at FROM st LEFT JOIN upd ON true
  `,
			args: []any{request.ID, status, min(requesterID, recipientID), max(requesterID, recipientID), uuid.NewString(), requesterID, recipientID},
			scan: scanFirstRow(&state.found, &state.pending, &state.blocked, &updatedID, &updatedStatus, &updatedAt),
		}); err != nil {
			return err
		}
		if locked != 2 {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if !state.found {
			return fmt.Errorf("friend request %s disappeared", request.ID)
		}
		if !state.pending {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		if status == "accepted" && state.blocked {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if updatedID == nil || updatedStatus == nil || updatedAt == nil {
			return fmt.Errorf("friend request %s was not updated", request.ID)
		}
		request.ID, request.Status, request.UpdatedAt = *updatedID, *updatedStatus, *updatedAt
		return c.JSON(http.StatusOK, transitionFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}
