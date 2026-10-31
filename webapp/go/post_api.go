package isureal

import (
	"database/sql"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createPostRequest struct {
	Image        *multipart.FileHeader `form:"image"`
	Caption      string                `form:"caption"`
	Visibility   string                `form:"visibility"`
	LocationKind string                `form:"location_kind"`
	Latitude     *float64              `form:"latitude"`
	Longitude    *float64              `form:"longitude"`
	Locality     *string               `form:"locality"`
}

type postAuthorResponse struct {
	ID              string  `json:"id"`
	Username        string  `json:"username"`
	DisplayName     string  `json:"displayName"`
	ProfileImageURL *string `json:"profileImageUrl"`
	Bio             string  `json:"bio"`
	AccountType     string  `json:"accountType"`
}

type createPostResponse struct {
	ID              string             `json:"id"`
	Author          postAuthorResponse `json:"author"`
	PostingWindowID string             `json:"postingWindowId"`
	ImageURL        string             `json:"imageUrl"`
	Caption         string             `json:"caption"`
	Visibility      string             `json:"visibility"`
	Location        PostLocation       `json:"location"`
	PostedAt        time.Time          `json:"postedAt"`
	OnTime          bool               `json:"onTime"`
	LateSeconds     int64              `json:"lateSeconds"`
}

func createPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil || !validUserText(input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		switch input.Visibility {
		case "friends", "friends_of_friends", "public":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		location := PostLocation{
			Kind: input.LocationKind, Latitude: input.Latitude,
			Longitude: input.Longitude, Locality: input.Locality,
		}
		if !location.valid() {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		imageFile, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer imageFile.Close()
		imageData, err := io.ReadAll(imageFile)
		if err != nil {
			return err
		}
		// デコード失敗も上限超えも同じ400なので、上限超えは全体のデコードを省略してよい
		check := checkJPEG(imageData, 4096)
		if !check.decoded || !check.ok {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if check.width > 4096 || check.height > 4096 {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}

		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 画像本体はこのサーバーの手元に置き(isu3へのPUTは裏で続ける)、ロックを取る前に保存を終える。
		// 画像の行は、保存に成功したときだけ作る
		imageID := uuid.NewString()
		imageOrigin, err := storeUploadedImage(imageID, imageData)
		if err != nil {
			return err
		}
		postedAt := time.Now().UTC()
		postID := uuid.NewString()
		// BEGIN〜COMMITを1往復で送る。
		// 1文目: 投稿者の行ロック(当日の投稿数・削除数を並行する変更から守る)と、応答に使うユーザーの取得
		// 2文目: 投稿期間の共有ロック(FOR SHARE)、当日の投稿数の確認、画像と投稿の作成を1文で行う。
		//        条件を満たさないときは何も作らず、0行を返す
		// 3文目: 2文目の時点で投稿期間がなかった(切り替わり中だった)ときだけ、新しいスナップショットでもう一度試す
		var user User
		var userFound, postCreated, retryCreated bool
		var post, retryPost Post
		createStmt := func(created *bool, dest *Post) pipelinedStmt {
			return pipelinedStmt{
				sql: `
   WITH win AS (
     SELECT w.id, w.local_date, w.starts_at, w.on_time_deadline, w.ended_at
     FROM posting_windows w
     WHERE w.region = (SELECT region FROM users WHERE id = $2 AND status = 'active')
       AND w.ended_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM posts WHERE id = $1)
     FOR SHARE OF w
   ), ok AS (
     SELECT win.* FROM win
     WHERE (SELECT count(*) FROM posts p JOIN posting_windows pw ON pw.id = p.posting_window_id
            WHERE p.author_id = $2 AND pw.local_date = win.local_date AND p.deleted_at IS NULL) = 0
       AND (SELECT count(*) FROM posts p JOIN posting_windows pw ON pw.id = p.posting_window_id
            WHERE p.author_id = $2 AND pw.local_date = win.local_date AND p.deleted_at IS NOT NULL) < 2
       AND $10::timestamptz >= win.starts_at
   ), img AS (
     INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at, origin_region)
     SELECT $3::text, $2::text, ''::bytea, 'image/jpeg', $10::timestamptz, $11::text FROM ok
   ), p AS (
     INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                        location_kind, latitude, longitude, locality, posted_at)
     SELECT $1::text, $2::text, ok.id, $3::text, $4::text, $5::text, $6::text, $7::float8, $8::float8, $9::text, $10::timestamptz FROM ok
     RETURNING *
   )
   SELECT ` + postColumns + ` FROM p JOIN ok w ON w.id = p.posting_window_id`,
				args: []any{postID, userID, imageID, input.Caption, input.Visibility,
					location.Kind, location.Latitude, location.Longitude, location.Locality, postedAt, nullableOrigin(imageOrigin)},
				scan: scanFirstRow(created, postScanDest(dest)...),
			}
		}
		if err := runPipelinedTx(ctx, h.DB,
			pipelinedStmt{
				sql:  `SELECT ` + strings.Join(userColumns, ", ") + ` FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE`,
				args: []any{userID},
				scan: scanFirstRow(&userFound, userScanDest(&user)...),
			},
			createStmt(&postCreated, &post),
			createStmt(&retryCreated, &retryPost),
		); err != nil {
			return err
		}
		// コミット済み。今日の投稿と投稿数が変わりうるので、本人のキャッシュを消す
		userViews.invalidateUser(userID)
		if !userFound {
			return sql.ErrNoRows
		}
		if !postCreated {
			if !retryCreated {
				return NewAPIError(http.StatusConflict, "post_slot_unavailable")
			}
			post = retryPost
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, newPostResponse(post, user))
	}
}

func newPostResponse(post Post, user User) createPostResponse {
	response := createPostResponse{
		ID: post.ID,
		Author: postAuthorResponse{
			ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
			ProfileImageURL: newGetMeResponse(user).ProfileImageURL,
			Bio:             user.Bio, AccountType: user.AccountType,
		},
		PostingWindowID: post.PostingWindowID, ImageURL: signedImageURL(user, post.ImageID),
		Caption: post.Caption, Visibility: post.Visibility, Location: post.PostLocation,
		PostedAt: post.PostedAt, OnTime: !post.PostedAt.After(post.OnTimeDeadline),
	}
	if !response.OnTime {
		response.LateSeconds = int64(post.PostedAt.Sub(post.OnTimeDeadline) / time.Second)
	}
	return response
}

type getMyCurrentPostResponse createPostResponse

func getMyCurrentPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		cached, epoch := userViews.begin(userID)
		if cached.myPost != nil {
			if !cached.myPost.found {
				return NewAPIError(http.StatusNotFound, "post_not_found")
			}
			post := cached.myPost.post
			return c.JSON(http.StatusOK, getMyCurrentPostResponse(newPostResponse(post.Post, post.Author)))
		}
		// 投稿IDの特定・投稿・投稿者の取得を1往復にまとめる
		var post PostWithAuthor
		err := h.ReadDB(c).GetContext(ctx, &post, `
   SELECT `+postColumns+`, `+userColumnsAs("u", "author")+` FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   JOIN users u ON u.id = p.author_id AND u.status = 'active'
   JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                              AND current.local_date = w.local_date
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
  `, userID)
		if errors.Is(err, sql.ErrNoRows) {
			userViews.putMyPost(epoch, userID, cachedMyPost{found: false})
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if err := post.Author.setImageKey(); err != nil {
			return err
		}
		// 署名付きURLは exp が変わるのでキャッシュせず、応答のたびに作る
		userViews.putMyPost(epoch, userID, cachedMyPost{found: true, post: post})
		return c.JSON(http.StatusOK, getMyCurrentPostResponse(newPostResponse(post.Post, post.Author)))
	}
}

type getPostRequest struct {
	PostID string `param:"postId"`
}

type getPostResponse createPostResponse

func getPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		// 投稿と閲覧者を1回の往復で読む
		post, postFound, viewer, viewerFound, err := loadPostAndViewer(ctx, h.ReadDB(c), input.PostID, GetSession(c).UserID)
		if err != nil {
			return err
		}
		if !postFound || !viewerFound || !viewer.canView(post.Post) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		return c.JSON(http.StatusOK, getPostResponse(newPostResponse(post.Post, post.Author)))
	}
}

type updatePostRequest struct {
	PostID     string  `param:"postId" json:"-"`
	Caption    *string `json:"caption"`
	Visibility *string `json:"visibility"`
}

type updatePostResponse createPostResponse

func updatePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input updatePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Caption == nil && input.Visibility == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Caption != nil && !validUserText(*input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Visibility != nil {
			switch *input.Visibility {
			case "friends", "friends_of_friends", "public":
			default:
				return NewAPIError(http.StatusBadRequest, "invalid_post")
			}
		}
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// 行ロックとユーザーの取得を1往復にまとめる(user は応答にも使う)
		user, err := lockUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		post, err := getPost(ctx, tx, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if post.AuthorID != userID {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		// getPost は投稿期間の ended_at も返すので getPostingWindow は不要
		if post.WindowEndedAt != nil {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		// caption/visibility の更新と読み直しを1往復にまとめる
		if err := tx.GetContext(ctx, &post, `
   WITH p AS (
     UPDATE posts SET caption = COALESCE($2, caption), visibility = COALESCE($3, visibility)
     WHERE id = $1 RETURNING *
   )
   SELECT `+postColumns+` FROM p JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.deleted_at IS NULL
  `, post.ID, input.Caption, input.Visibility); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		userViews.invalidateUser(user.ID)
		return c.JSON(http.StatusOK, updatePostResponse(newPostResponse(post, user)))
	}
}

type deletePostRequest struct {
	PostID string `param:"postId" json:"-"`
}

func deletePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deletePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var lockedUserID string
		if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
			return err
		}
		// 投稿の取得と、同じ日の削除済み投稿数の確認を1往復にまとめる
		var post struct {
			ID        string `db:"id"`
			AuthorID  string `db:"author_id"`
			Deletions int    `db:"deletions"`
		}
		err = tx.GetContext(ctx, &post, `
   SELECT p.id, p.author_id,
     (SELECT count(*) FROM posts dp JOIN posting_windows dw ON dw.id = dp.posting_window_id
      WHERE dp.author_id = $2 AND dw.local_date = w.local_date AND dp.deleted_at IS NOT NULL) AS deletions
   FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.id = $1 AND p.deleted_at IS NULL
  `, input.PostID, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if post.AuthorID != userID {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		if post.Deletions >= 2 {
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET deleted_at = clock_timestamp() WHERE id = $1`, post.ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		userViews.invalidateUser(userID)
		return c.NoContent(http.StatusNoContent)
	}
}
