package isureal

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

type createCommentRequest struct {
	PostID string `param:"postId" json:"-"`
	Body   string `json:"body"`
}

type createCommentResponse struct {
	ID        string                   `json:"id"`
	Author    relationshipUserResponse `json:"author"`
	Body      string                   `json:"body"`
	CreatedAt time.Time                `json:"createdAt"`
}

func newCommentResponse(comment Comment, user User) createCommentResponse {
	return createCommentResponse{ID: comment.ID, Author: newRelationshipUserResponse(user), Body: comment.Body, CreatedAt: comment.CreatedAt}
}

func createCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Body, 1, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_interaction")
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		// BEGIN〜COMMITを1往復で送る。
		// 1文目: 自分と投稿者の行ロック(ORDER BY id でロック順を固定)と、応答に使う自分のユーザー情報の取得。
		//        投稿者は投稿IDから引く(投稿がなければ自分だけをロックし、2文目で403になる)
		// 2文目: ロック後のスナップショットでコメントできるか(canComment と同じ判定)を確かめ、できるときだけ作成する
		var user User
		var comment Comment
		var created bool
		if err := runPipelinedTx(ctx, h.DB,
			pipelinedStmt{
				sql: `SELECT ` + strings.Join(userColumns, ", ") + ` FROM users
   WHERE id = $1 OR id = (SELECT author_id FROM posts WHERE id = $2 AND deleted_at IS NULL)
   ORDER BY id FOR NO KEY UPDATE`,
				args: []any{actor, input.PostID},
				scan: func(rows pgx.Rows) error {
					for rows.Next() {
						var u User
						if err := rows.Scan(userScanDest(&u)...); err != nil {
							return err
						}
						if u.ID == actor {
							user = u
						}
					}
					return nil
				},
			},
			pipelinedStmt{
				sql: `INSERT INTO comments(id,post_id,author_id,body,created_at)
   SELECT $3::text, $2::text, $1::text, $4::text, clock_timestamp()
   WHERE ` + postInteractionAllowedSQL("$1::text", "$2::text", true) + `
   RETURNING id,post_id,author_id,body,created_at,deleted_at`,
				args: []any{actor, input.PostID, uuid.NewString(), input.Body},
				scan: scanFirstRow(&created, &comment.ID, &comment.PostID, &comment.AuthorID, &comment.Body, &comment.CreatedAt, &comment.DeletedAt),
			},
		); err != nil {
			return err
		}
		if !created {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		// 元の getUser と同じく、有効なユーザーでなければエラーにする
		if user.Status != "active" {
			return sql.ErrNoRows
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, newCommentResponse(comment, user))
	}
}

type getCommentsRequest struct {
	PostID         string     `param:"postId"`
	AfterCreatedAt *time.Time `query:"afterCreatedAt"`
	AfterID        string     `query:"afterId"`
}

type getCommentsResponse struct {
	Items   []createCommentResponse `json:"items"`
	HasMore bool                    `json:"hasMore"`
}

func getCommentsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getCommentsRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.AfterCreatedAt == nil) != (input.AfterID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		// 投稿・閲覧者・コメントを1回の往復で読み、コメントできるかで捨てるかを決める
		var comments []CommentWithAuthor
		post, postFound, viewer, viewerFound, err := loadPostAndViewer(ctx, h.ReadDB(c), input.PostID, GetSession(c).UserID,
			commentsWithAuthorStmt(`AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3)) ORDER BY c.created_at,c.id LIMIT 51`, &comments, input.PostID, input.AfterCreatedAt, input.AfterID))
		if err != nil {
			return err
		}
		if !canCommentOn(post, postFound, viewer, viewerFound) {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		items := make([]createCommentResponse, 0, len(comments))
		for _, comment := range comments {
			items = append(items, newCommentResponse(comment.Comment, comment.Author))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getCommentsResponse{Items: items, HasMore: hasMore})
	}
}

type deleteCommentRequest struct {
	CommentID string `param:"commentId" json:"-"`
}

func deleteCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		// BEGIN〜COMMITを1往復で送る。1文目で操作する人の行をロック、2文目でコメントを行ロックして読み、
		// 3文目は「書いた本人か、投稿者」のときだけ削除する
		var actorFound, found, deleted bool
		var record struct {
			AuthorID     string
			PostAuthorID string
		}
		if err := runPipelinedTx(ctx, h.DB,
			pipelinedStmt{
				sql:  `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`,
				args: []any{actor},
				scan: scanExists(&actorFound),
			},
			pipelinedStmt{
				sql:  `SELECT c.author_id,p.author_id AS post_author_id FROM comments c JOIN posts p ON p.id=c.post_id WHERE c.id=$1 AND c.deleted_at IS NULL FOR UPDATE OF c`,
				args: []any{input.CommentID},
				scan: scanFirstRow(&found, &record.AuthorID, &record.PostAuthorID),
			},
			pipelinedStmt{
				sql: `UPDATE comments c SET deleted_at=clock_timestamp()
   WHERE c.id=$1 AND c.deleted_at IS NULL AND EXISTS (SELECT 1 FROM users u WHERE u.id=$2)
     AND (c.author_id=$2 OR EXISTS (SELECT 1 FROM posts p WHERE p.id=c.post_id AND p.author_id=$2))
   RETURNING c.id`,
				args: []any{input.CommentID, actor},
				scan: scanExists(&deleted),
			},
		); err != nil {
			return err
		}
		if !actorFound {
			// 前の実装と同じく、操作する人の行がなければエラーにする
			return sql.ErrNoRows
		}
		if !found {
			return NewAPIError(http.StatusNotFound, "interaction_not_found")
		}
		if (actor != record.AuthorID && actor != record.PostAuthorID) || !deleted {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		return c.NoContent(http.StatusNoContent)
	}
}
