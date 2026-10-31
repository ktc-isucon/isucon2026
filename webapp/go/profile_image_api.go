package isureal

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// removedImage はDBから消した画像のIDと作成日時(初期データかどうかの判定に使う)。
type removedImage struct {
	ID        string
	CreatedAt time.Time
}

func scanRemovedImages(dest *[]removedImage) func(pgx.Rows) error {
	return func(rows pgx.Rows) error {
		for rows.Next() {
			var img removedImage
			if err := rows.Scan(&img.ID, &img.CreatedAt); err != nil {
				return err
			}
			*dest = append(*dest, img)
		}
		return nil
	}
}

// deleteRemovedRuntimeImages はDBから消した画像のうち、ベンチ中に追加されたもののファイルを消す(ベストエフォート)
func deleteRemovedRuntimeImages(ctx context.Context, h *HandlerContext, removed []removedImage) {
	for _, img := range removed {
		if kind, err := imageKind(ctx, h.DB, img.CreatedAt); err == nil {
			deleteRuntimeImage(kind, img.ID)
		}
	}
}

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
		// BEGIN〜COMMITを1往復で送る。1文目でユーザー行をロックし、2文目はロック後のスナップショットで
		// 新しい画像の作成・付け替え・古い画像の削除をまとめて行う(ユーザーがいなければ何もしない)
		var userFound bool
		var removed []removedImage
		if err := runPipelinedTx(ctx, h.DB,
			pipelinedStmt{
				sql:  `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`,
				args: []any{userID},
				scan: scanExists(&userFound),
			},
			pipelinedStmt{
				sql: `
  WITH u AS (
    SELECT id, profile_image_id AS old FROM users WHERE id = $1
  ), ins AS (
    INSERT INTO image_objects (id, owner_user_id, data, media_type, origin_region)
    SELECT $2::text, id, ''::bytea, 'image/jpeg', $3::text FROM u
  ), upd AS (
    UPDATE users SET profile_image_id = $2::text, updated_at = clock_timestamp() WHERE id IN (SELECT id FROM u)
  ), del AS (
    DELETE FROM image_objects WHERE id IN (SELECT old FROM u) RETURNING id, created_at
  )
  SELECT del.id, del.created_at FROM del
  CROSS JOIN LATERAL (SELECT pg_notify('` + sessionInvalidateChannel + `', $1::text)) AS notify
 `,
				args: []any{userID, imageID, nullableOrigin(imageOrigin)},
				scan: scanRemovedImages(&removed),
			},
		); err != nil {
			return err
		}
		if !userFound {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		userViews.invalidateUser(userID)
		if len(removed) > 0 {
			// 消した画像の署名鍵をこのサーバーのキャッシュから消す(他サーバーへはコミット時にNOTIFYが届く)
			imageAuths.invalidateOwner(userID)
		}
		deleteRemovedRuntimeImages(ctx, h, removed)
		return c.NoContent(http.StatusNoContent)
	}
}

func deleteProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// BEGIN〜COMMITを1往復で送る。1文目でユーザー行をロックし、2文目はプロフィール画像があるときだけ外して消す
		var userFound bool
		var imageID *string
		var removed []removedImage
		if err := runPipelinedTx(ctx, h.DB,
			pipelinedStmt{
				sql:  `SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE`,
				args: []any{userID},
				scan: scanFirstRow(&userFound, &imageID),
			},
			pipelinedStmt{
				sql: `
  WITH u AS (
    SELECT id, profile_image_id AS old FROM users WHERE id = $1 AND profile_image_id IS NOT NULL
  ), upd AS (
    UPDATE users SET profile_image_id = NULL, updated_at = clock_timestamp() WHERE id IN (SELECT id FROM u)
  ), del AS (
    DELETE FROM image_objects WHERE id IN (SELECT old FROM u) RETURNING id, created_at
  )
  SELECT del.id, del.created_at FROM del
  CROSS JOIN LATERAL (SELECT pg_notify('` + sessionInvalidateChannel + `', $1::text)) AS notify
 `,
				args: []any{userID},
				scan: scanRemovedImages(&removed),
			},
		); err != nil {
			return err
		}
		if !userFound || imageID == nil {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		userViews.invalidateUser(userID)
		if len(removed) > 0 {
			// 消した画像の署名鍵をこのサーバーのキャッシュから消す(他サーバーへはコミット時にNOTIFYが届く)
			imageAuths.invalidateOwner(userID)
		}
		deleteRemovedRuntimeImages(ctx, h, removed)
		return c.NoContent(http.StatusNoContent)
	}
}
