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

		// 「画像の行があり、持ち主が有効」と分かっている画像は、DB を引く前に署名を確かめ、不正なら弾く。
		// 応答と abuse は、DB を引いてから署名が合わなかった場合(下)と同じ。
		cached, hit, epoch := imageAuths.begin(input.ImageID)
		if hit {
			key, err := imageSigningKey(cached.SecretKeyHash)
			if err != nil {
				return err
			}
			if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
				c.Set("abuse", "invalid_signature")
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			}
		}

		// 削除状態を毎回確認する。即時反映も5秒以内の許容範囲に含まれる。
		// 署名鍵と閲覧可否を1回の問い合わせで取得する。
		ctx := c.Request().Context()
		var object struct {
			OwnerID       string    `db:"owner_id"`
			SecretKeyHash string    `db:"secret_key_hash"`
			CreatedAt     time.Time `db:"created_at"`
			OriginRegion  *string   `db:"origin_region"`
			Visible       bool      `db:"visible"`
		}
		err = h.ReadDB(c).GetContext(ctx, &object, `
   SELECT u.id AS owner_id, u.secret_key_hash, i.created_at, i.origin_region, (
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
		if !hit {
			imageAuths.put(epoch, input.ImageID, imageAuthEntry{
				OwnerID: object.OwnerID, SecretKeyHash: object.SecretKeyHash,
				CreatedAt: object.CreatedAt, OriginRegion: object.OriginRegion,
			})
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
		accelPath := "/_img/" + imageRelPath(kind, input.ImageID)
		if kind == imageKindRuntime && object.OriginRegion != nil {
			// 別のリージョンのサーバーが持っている画像は、そのサーバー(落ちていればisu3)から取得させる
			accelPath = runtimeImageAccelPath(*object.OriginRegion, input.ImageID)
		}
		c.Response().Header().Set("X-Accel-Redirect", accelPath)
		c.Response().Header().Set(echo.HeaderContentType, "image/jpeg")
		return c.NoContent(http.StatusOK)
	}
}
