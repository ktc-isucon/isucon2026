package isureal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type getNotificationsResponse struct {
	ID              string    `json:"id" db:"id"`
	PostingWindowID string    `json:"postingWindowId" db:"posting_window_id"`
	Region          string    `json:"region" db:"region"`
	CreatedAt       time.Time `json:"createdAt" db:"created_at"`
}

// notificationIDNamespace は通知IDを(利用者, 投稿期間)から決定的に作るためのUUID v5の名前空間
var notificationIDNamespace = uuid.MustParse("6f1b5c2e-8d0a-4f5e-9b8a-2c7d4e1f3a90")

// notificationID は同じ利用者・投稿期間に対して常に同じ通知IDを返す
func notificationID(userID, postingWindowID string) string {
	return uuid.NewSHA1(notificationIDNamespace, []byte(userID+":"+postingWindowID)).String()
}

func getNotificationsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		session := GetSession(c)
		userID := session.UserID
		var connectedAt time.Time
		if err := h.DB.GetContext(ctx, &connectedAt, `SELECT clock_timestamp()`); err != nil {
			return err
		}
		response := c.Response()
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(response)
		if err := controller.Flush(); err != nil {
			return nil
		}
		// 接続ごとのポーリングはせず、サーバーで1本の監視(notificationWatcher)から
		// 投稿期間の変化とユーザーの無効化を受け取る
		subscriber := h.Notifications.subscribe(userID, session.Region)
		defer h.Notifications.unsubscribe(subscriber)
		first := true
		for {
			// 通知を確認する前に取得し、確認後に開いた投稿期間を取りこぼさない
			changed := h.Notifications.changes(session.Region)
			if first {
				first = false
				user, err := getUser(ctx, h.DB, userID)
				if err != nil {
					if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
						c.Logger().Error("failed to get notification recipient", "error", err)
					}
					return nil
				}
				if user.Region != session.Region {
					return nil
				}
			}
			// 通知は投稿期間の開始時に行として作らず、ここで求める。
			// 開始時の「対象リージョンの有効な利用者」と同じ判定になる理由:
			// - 返すのは接続後(starts_at > connectedAt)に開いた投稿期間だけなので、利用者は開始時点で既に登録済み
			// - リージョン移動・退会があるとこの接続は閉じる(subscriber.gone)うえ、同じ文で今のリージョンと状態も確かめる
			var notification getNotificationsResponse
			err := h.DB.GetContext(ctx, &notification, `
    SELECT w.id AS posting_window_id, w.region, w.starts_at AS created_at
    FROM posting_windows w JOIN users u ON u.id = $1 AND u.region = w.region AND u.status = 'active'
    WHERE w.ended_at IS NULL AND w.starts_at > $2
    ORDER BY w.starts_at, w.id LIMIT 1
   `, userID, connectedAt)
			if err == nil {
				notification.ID = notificationID(userID, notification.PostingWindowID)
				data, err := json.Marshal(notification)
				if err != nil {
					c.Logger().Error("failed to encode notification", "error", err)
					return nil
				}
				if _, err := fmt.Fprintf(response, "data: %s\n\n", data); err != nil {
					return nil
				}
				_ = controller.Flush()
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				if ctx.Err() == nil {
					c.Logger().Error("failed to get notification", "error", err)
				}
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-subscriber.gone:
				return nil
			case <-changed:
			}
		}
	}
}
