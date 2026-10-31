package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type currentPostingWindowResponse struct {
	ID             string    `json:"id"`
	Region         string    `json:"region"`
	LocalDate      string    `json:"localDate"`
	StartsAt       time.Time `json:"startsAt"`
	OnTimeDeadline time.Time `json:"onTimeDeadline"`
}

type getCurrentPostingWindowResponse struct {
	Window        *currentPostingWindowResponse `json:"window"`
	HasPosted     bool                          `json:"hasPosted"`
	CanPost       bool                          `json:"canPost"`
	DeletionCount int                           `json:"deletionCount"`
}

func getCurrentPostingWindowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		userID := GetSession(c).UserID
		cached, epoch := userViews.begin(userID)
		if cached.window != nil {
			return c.JSON(http.StatusOK, newGetCurrentPostingWindowResponse(*cached.window))
		}
		var state struct {
			PostingWindow
			Active  int `db:"active"`
			Deleted int `db:"deleted"`
		}
		err := h.ReadDB(c).GetContext(c.Request().Context(), &state, `
   SELECT w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline, w.ended_at,
          count(p.id) FILTER (WHERE p.deleted_at IS NULL) AS active,
          count(p.id) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
   FROM users u JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
   LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date
   LEFT JOIN posts p ON p.author_id = u.id AND p.posting_window_id = posted_window.id
   WHERE u.id = $1
   GROUP BY w.id
  `, userID)
		var result cachedPostingWindowState
		if errors.Is(err, sql.ErrNoRows) {
			result = cachedPostingWindowState{}
		} else if err != nil {
			return err
		} else {
			result = cachedPostingWindowState{found: true, window: state.PostingWindow, active: state.Active, deleted: state.Deleted}
		}
		userViews.putWindow(epoch, userID, result)
		return c.JSON(http.StatusOK, newGetCurrentPostingWindowResponse(result))
	}
}

// newGetCurrentPostingWindowResponse はDBの結果(キャッシュ)から応答を作る。canPost は時刻に依存するので毎回計算する。
func newGetCurrentPostingWindowResponse(state cachedPostingWindowState) getCurrentPostingWindowResponse {
	if !state.found {
		return getCurrentPostingWindowResponse{}
	}
	w := state.window
	return getCurrentPostingWindowResponse{
		Window: &currentPostingWindowResponse{
			ID: w.ID, Region: w.Region, LocalDate: w.LocalDate.Format(time.DateOnly),
			StartsAt: w.StartsAt, OnTimeDeadline: w.OnTimeDeadline,
		},
		HasPosted:     state.active > 0,
		CanPost:       state.active == 0 && state.deleted < 2 && !time.Now().Before(w.StartsAt),
		DeletionCount: state.deleted,
	}
}
