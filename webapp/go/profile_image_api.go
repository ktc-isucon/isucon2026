package isureal

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func putProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Request().Header.Get("Content-Type") != "image/jpeg" {
			// 本文を読まずに返すと接続が閉じられ、nginxが502にしてしまうため読み捨てる
			_, _ = io.Copy(io.Discard, io.LimitReader(c.Request().Body, 32<<20))
			return NewAPIError(http.StatusUnsupportedMediaType, "unsupported_media_type")
		}
		data, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		// デコード失敗のときだけabuseを付けるため、上限超えでもデコードは省略しない(skipOver=0)
		check := checkJPEG(data, 0)
		if !check.ok {
			c.Set("abuse", "invalid_image")
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}
		if check.width > 512 || check.height > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}

		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 画像本体はこのサーバーの手元に置く(isu3へのPUTは裏で続ける)
		imageID := uuid.NewString()
		imageOrigin, err := storeUploadedImage(imageID, data)
		if err != nil {
			return err
		}
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var oldID *string
		if err := tx.GetContext(ctx, &oldID, `SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return NewAPIError(http.StatusNotFound, "account_not_found")
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO image_objects(id,owner_user_id,data,media_type,origin_region) VALUES ($1,$2,''::bytea,'image/jpeg',$3)`, imageID, userID, nullableOrigin(imageOrigin)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET profile_image_id=$2,updated_at=clock_timestamp() WHERE id=$1`, userID, imageID); err != nil {
			return err
		}
		var oldCreatedAt []time.Time
		if oldID != nil {
			if err := tx.SelectContext(ctx, &oldCreatedAt, `DELETE FROM image_objects WHERE id=$1 RETURNING created_at`, *oldID); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		userViews.invalidateUser(userID)
		for _, createdAt := range oldCreatedAt {
			if kind, err := imageKind(ctx, h.DB, createdAt); err == nil {
				deleteRuntimeImage(kind, *oldID)
			}
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func deleteProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var imageID *string
		err = tx.GetContext(ctx, &imageID, `SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		if imageID == nil {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET profile_image_id=NULL,updated_at=clock_timestamp() WHERE id=$1`, userID); err != nil {
			return err
		}
		var deletedCreatedAt []time.Time
		if err := tx.SelectContext(ctx, &deletedCreatedAt, `DELETE FROM image_objects WHERE id=$1 RETURNING created_at`, *imageID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		userViews.invalidateUser(userID)
		for _, createdAt := range deletedCreatedAt {
			if kind, err := imageKind(ctx, h.DB, createdAt); err == nil {
				deleteRuntimeImage(kind, *imageID)
			}
		}
		return c.NoContent(http.StatusNoContent)
	}
}
