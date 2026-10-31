package isureal

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

type certifyUserRequest struct {
	UserID string `param:"userId" json:"-"`
}

func certifyUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input certifyUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		// 本人のサーバーが持つ自分の情報のキャッシュ(account_type)を消すため、同じ文で通知を送る(コミット時に届く)
		var count int
		if err := h.DB.GetContext(c.Request().Context(), &count, `
   WITH u AS (
     UPDATE users SET account_type='certified_public', updated_at=clock_timestamp()
     WHERE id=$1 AND status='active' AND account_type='regular'
     RETURNING id
   )
   SELECT count(*) FROM u CROSS JOIN LATERAL (SELECT pg_notify('`+sessionInvalidateChannel+`', u.id::text)) AS notify
  `, input.UserID); err != nil {
			return err
		}
		if count == 0 {
			return NewAPIError(http.StatusConflict, "certification_conflict")
		}
		userViews.invalidateUser(input.UserID)
		return c.NoContent(http.StatusNoContent)
	}
}

// 2026-08-26: 認定されたユーザーを戻すことは禁止する
func deleteUserCertificationHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		return NewAPIError(http.StatusConflict, "certification_irreversible")
	}
}
