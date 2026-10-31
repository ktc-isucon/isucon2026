package isureal

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"
)

func deleteAccountHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 退会・セッションの無効化・全サーバーへの通知を1文(1往復)で行う。
		// users の UPDATE が行ロックを取るので、前の実装の SELECT ... FOR NO KEY UPDATE と同じく、他の更新とは直列になる
		var deletedID string
		err := h.DB.GetContext(ctx, &deletedID, `
  WITH u AS (
    UPDATE users SET status = 'deleted', deleted_at = clock_timestamp(), updated_at = clock_timestamp()
    WHERE id = $1 AND status = 'active'
    RETURNING id
  ), s AS (
    UPDATE login_sessions SET active = false, invalidated_at = clock_timestamp(), reason = 'account_deletion'
    WHERE user_id IN (SELECT id FROM u) AND active = true
  )
  SELECT u.id FROM u CROSS JOIN LATERAL (SELECT pg_notify('`+sessionInvalidateChannel+`', u.id::text)) AS notify
 `, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		sessions.invalidateUser(userID)
		userViews.invalidateUser(userID)
		// 持ち主が有効でなくなったので、画像の署名鍵のキャッシュも消す(他サーバーは上の通知で消す)
		imageAuths.invalidateOwner(userID)
		return c.NoContent(http.StatusNoContent)
	}
}
