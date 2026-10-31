package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
)

type getImageRequest struct {
	ImageID string `param:"imageId" json:"-"`
	Exp     string `query:"exp" json:"-"`
	Sig     string `query:"sig" json:"-"`
}

func getImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		var input getImageRequest
		if err := c.Bind(&input); err != nil {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		query, err := url.ParseQuery(c.Request().URL.RawQuery)
		if err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		exp, err := strconv.ParseInt(input.Exp, 10, 64)
		if err != nil || strconv.FormatInt(exp, 10) != input.Exp || exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		sig, err := base64.RawURLEncoding.Strict().DecodeString(input.Sig)
		if err != nil || len(sig) != sha256.Size || base64.RawURLEncoding.EncodeToString(sig) != input.Sig {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		if !validImageID(input.ImageID) {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		// 削除状態を毎回確認する。即時反映も5秒以内の許容範囲に含まれる。
		// 署名鍵と閲覧可否を1回の問い合わせで取得する。
		ctx := c.Request().Context()
		var object struct {
			SecretKeyHash string    `db:"secret_key_hash"`
			CreatedAt     time.Time `db:"created_at"`
			Visible       bool      `db:"visible"`
		}
		err = h.ReadDB(c).GetContext(ctx, &object, `
   SELECT u.secret_key_hash, i.created_at, (
     u.profile_image_id IS NOT DISTINCT FROM i.id
     OR EXISTS (
       SELECT 1 FROM posts p
       WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
     )
     OR EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   ) AS visible
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id
   WHERE i.id = $1 AND u.status = 'active'
  `, input.ImageID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if err != nil {
			return err
		}
		key, err := imageSigningKey(object.SecretKeyHash)
		if err != nil {
			return err
		}
		if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
			c.Set("abuse", "invalid_signature")
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if !object.Visible {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		kind, err := imageKind(ctx, h.ReadDB(c), object.CreatedAt)
		if err != nil {
			return err
		}
		// 画像本体はnginxが返す(isu0〜2は初期データの画像を手元にキャッシュし、それ以外はisu3から取得する)
		// 成功時だけブラウザにキャッシュさせる。投稿削除後の取得禁止(5秒の猶予)に収まるよう5秒にする
		c.Response().Header().Set("Cache-Control", "private, max-age=5")
		c.Response().Header().Set("X-Accel-Redirect", "/_img/"+imageRelPath(kind, input.ImageID))
		c.Response().Header().Set(echo.HeaderContentType, "image/jpeg")
		return c.NoContent(http.StatusOK)
	}
}
