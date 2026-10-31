package isureal

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type openPostingWindowRequest struct {
	Region        string `json:"region"`
	LocalDate     string `json:"localDate"`
	OnTimeSeconds *int64 `json:"onTimeSeconds"`
}

type openPostingWindowResponse struct {
	ID             string    `json:"id"`
	Region         string    `json:"region"`
	LocalDate      string    `json:"localDate"`
	StartsAt       time.Time `json:"startsAt"`
	OnTimeDeadline time.Time `json:"onTimeDeadline"`
}

func openPostingWindowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input openPostingWindowRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		region := input.Region
		localDate, err := time.Parse(time.DateOnly, input.LocalDate)
		if !validRegion(region) || err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_posting_window")
		}
		seconds := int64(24 * 60 * 60)
		if input.OnTimeSeconds != nil {
			seconds = *input.OnTimeSeconds
		}
		ctx := c.Request().Context()
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// usersのテーブルロックは取らず、同じリージョンの開始処理だけを直列化する。
		// 競合の確認はロック取得後の文で行う(同じ文だとロック待ち前のスナップショットで判定してしまう)。
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('posting_window:' || $1))`, region); err != nil {
			return err
		}
		var opened struct {
			Conflict bool      `db:"conflict"`
			StartsAt time.Time `db:"starts_at"`
		}
		if err := tx.GetContext(ctx, &opened, `
   WITH checked AS MATERIALIZED (
     SELECT EXISTS(SELECT 1 FROM posting_windows WHERE region = $1 AND local_date >= $2) AS conflict, clock_timestamp() AS starts_at
   ), ended AS (
     UPDATE posting_windows SET ended_at = (SELECT starts_at FROM checked)
     WHERE region = $1 AND ended_at IS NULL AND NOT (SELECT conflict FROM checked)
   )
   SELECT conflict, starts_at FROM checked
  `, region, localDate); err != nil {
			return err
		}
		if opened.Conflict {
			return NewAPIError(http.StatusConflict, "posting_window_conflict")
		}
		startsAt := opened.StartsAt
		// 終了させたUPDATEと同じ文でINSERTすると、リージョンごとの部分ユニークインデックスに当たり得るため文を分ける。
		var window PostingWindow
		if err := tx.GetContext(ctx, &window, `
   WITH window_row AS (
     INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
     VALUES ($1, $2, $3, $4, $5)
     RETURNING id, region, local_date, starts_at, on_time_deadline, ended_at
   ), notified AS (
     INSERT INTO notifications (id, user_id, posting_window_id, created_at)
     SELECT gen_random_uuid()::text, u.id, w.id, w.starts_at FROM window_row w JOIN users u ON u.region = w.region AND u.status = 'active'
   )
   SELECT id, region, local_date, starts_at, on_time_deadline, ended_at FROM window_row
   CROSS JOIN (SELECT pg_notify('posting_window', $2::text)) AS notify
  `, uuid.NewString(), region, localDate, startsAt, startsAt.Add(time.Duration(seconds)*time.Second)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, openPostingWindowResponse{
			ID: window.ID, Region: window.Region, LocalDate: window.LocalDate.Format(time.DateOnly),
			StartsAt: window.StartsAt, OnTimeDeadline: window.OnTimeDeadline,
		})
	}
}
