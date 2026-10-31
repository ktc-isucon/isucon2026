package isureal

import (
	"database/sql"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type replaceRealMojiRequest struct {
	PostID       string                `param:"postId" json:"-"`
	Image        *multipart.FileHeader `form:"image"`
	ReactionType string                `form:"reactionType"`
}

type replaceRealMojiResponse struct {
	ID           string                   `json:"id"`
	Reactor      relationshipUserResponse `json:"reactor"`
	ReactionType string                   `json:"reactionType"`
	ImageURL     string                   `json:"imageUrl"`
	CreatedAt    time.Time                `json:"createdAt"`
}

func newRealMojiResponse(reaction RealMoji, user User) replaceRealMojiResponse {
	return replaceRealMojiResponse{ID: reaction.ID, Reactor: newRelationshipUserResponse(user), ReactionType: reaction.ReactionType, ImageURL: signedImageURL(user, reaction.ImageID), CreatedAt: reaction.CreatedAt}
}

func replaceRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input replaceRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		switch input.ReactionType {
		case "like", "smile", "surprise", "heart", "laugh", "instant":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		file, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		// デコード失敗も上限超えも同じ400なので、上限超えは全体のデコードを省略してよい
		check := checkJPEG(data, 512)
		if !check.decoded || !check.ok {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		if check.width > 512 || check.height > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}

		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		// 画像本体はこのサーバーの手元に置く(isu3へのPUTは裏で続ける)。画像の行は保存に成功したときだけ作る
		imageID := uuid.NewString()
		imageOrigin, err := storeUploadedImage(imageID, data)
		if err != nil {
			return err
		}
		// BEGIN〜COMMITを1往復で送る。
		// 1文目: 自分と投稿者のペアのadvisory lockと、2人の行の FOR SHARE(beginRelationshipTx と同じロック)。
		//        投稿者は投稿IDから引く
		// 2文目: ロック後のスナップショットで閲覧できるか(canViewPost と同じ判定)を1回だけ評価し、
		//        結果をトランザクション内だけ有効な設定値に入れる。後続の文はこの値を見て、許可されたときだけ変更する
		//        (置き換え前の無効化と新規作成は、部分ユニークインデックスの判定順の都合で別の文にする必要があり、
		//         文ごとに判定し直すと、その間に関係が変わったとき置き換えだけが起きうるため)
		// 3〜5文目: 置き換え前の無効化、画像の行の作成、RealMojiの作成と応答用のユーザー取得
		const realmojiOK = `current_setting('isureal.realmoji_ok') = 'true'`
		var reaction RealMoji
		var user User
		var created bool
		if err := runPipelinedTx(ctx, h.DB,
			pipelinedStmt{
				sql: `
   WITH author AS (SELECT author_id AS id FROM posts WHERE id = $2 AND deleted_at IS NULL)
   SELECT id FROM users
   WHERE id IN ($1, (SELECT id FROM author)) AND status = 'active'
     AND (SELECT count(*) FROM (SELECT pg_advisory_xact_lock(hashtextextended(
           'relationship:' || least($1::text, (SELECT id FROM author)) || ':' || greatest($1::text, (SELECT id FROM author)), 0))) l) = 1
   ORDER BY id FOR SHARE`,
				args: []any{actor, input.PostID},
			},
			pipelinedStmt{
				sql:  `SELECT set_config('isureal.realmoji_ok', (` + postInteractionAllowedSQL("$1::text", "$2::text", false) + `)::text, true)`,
				args: []any{actor, input.PostID},
			},
			pipelinedStmt{
				sql:  `UPDATE realmojis SET replaced_at=clock_timestamp() WHERE post_id=$1 AND reactor_id=$2 AND replaced_at IS NULL AND deleted_at IS NULL AND ` + realmojiOK,
				args: []any{input.PostID, actor},
			},
			pipelinedStmt{
				sql:  `INSERT INTO image_objects(id,owner_user_id,data,media_type,origin_region) SELECT $1::text, $2::text, ''::bytea, 'image/jpeg', $3::text WHERE ` + realmojiOK,
				args: []any{imageID, actor, nullableOrigin(imageOrigin)},
			},
			pipelinedStmt{
				sql: `
   WITH r AS (
     INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at)
     SELECT $1::text, $2::text, $3::text, $4::text, $5::text, clock_timestamp() WHERE ` + realmojiOK + `
     RETURNING id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at
   )
   SELECT r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at, ` + prefixedUserColumns("u") + `
   FROM r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'`,
				args: []any{uuid.NewString(), input.PostID, actor, imageID, input.ReactionType},
				scan: scanFirstRow(&created, append([]any{&reaction.ID, &reaction.PostID, &reaction.ReactorID, &reaction.ImageID,
					&reaction.ReactionType, &reaction.CreatedAt, &reaction.ReplacedAt, &reaction.DeletedAt}, userScanDest(&user)...)...),
			},
		); err != nil {
			return err
		}
		if !created {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, newRealMojiResponse(reaction, user))
	}
}

type getRealMojisRequest struct {
	PostID          string     `param:"postId"`
	BeforeCreatedAt *time.Time `query:"beforeCreatedAt"`
	BeforeID        string     `query:"beforeId"`
}

type getRealMojisResponse struct {
	Items   []replaceRealMojiResponse `json:"items"`
	HasMore bool                      `json:"hasMore"`
}

func getRealMojisHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getRealMojisRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.BeforeCreatedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		allowed, err := canViewPost(ctx, h.ReadDB(c), GetSession(c).UserID, input.PostID)
		if err != nil {
			return err
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		reactions, err := selectCurrentRealMojis(ctx, h.ReadDB(c), `AND r.post_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3)) ORDER BY r.created_at DESC,r.id DESC LIMIT 51`, input.PostID, input.BeforeCreatedAt, input.BeforeID)
		if err != nil {
			return err
		}
		items := make([]replaceRealMojiResponse, 0, len(reactions))
		for _, reaction := range reactions {
			items = append(items, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getRealMojisResponse{Items: items, HasMore: hasMore})
	}
}

type deleteRealMojiRequest struct {
	RealMojiID string `param:"realMojiId" json:"-"`
}

func deleteRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var lockedID string
		if err := tx.GetContext(ctx, &lockedID, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, actor); err != nil {
			return err
		}
		var record struct {
			ReactorID string `db:"reactor_id"`
			AuthorID  string `db:"author_id"`
		}
		err = tx.GetContext(ctx, &record, `SELECT r.reactor_id,p.author_id FROM realmojis r JOIN posts p ON p.id=r.post_id WHERE r.id=$1 AND r.deleted_at IS NULL FOR UPDATE OF r`, input.RealMojiID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "interaction_not_found")
		}
		if err != nil {
			return err
		}
		if actor != record.ReactorID && actor != record.AuthorID {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE realmojis SET deleted_at=clock_timestamp() WHERE id=$1`, input.RealMojiID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
