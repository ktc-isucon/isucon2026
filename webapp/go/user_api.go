package isureal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// PostgreSQLのunique_violation
const pgUniqueViolation = "23505"

type registerRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Region      string `json:"region"`
}

type registerResponse struct {
	SecretKey string              `json:"secretKey"`
	User      sessionUserResponse `json:"user"`
}

type loginRequest struct {
	Username  string `json:"username"`
	SecretKey string `json:"secretKey"`
}

type loginResponse struct {
	Token string              `json:"token"`
	User  sessionUserResponse `json:"user"`
}

type sessionUserResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Bio         string `json:"bio"`
	Region      string `json:"region"`
	AccountType string `json:"accountType"`
	Status      string `json:"status"`
}

type getMeResponse struct {
	sessionUserResponse
	ProfileImageURL *string `json:"profileImageUrl"`
}

type updateMeRequest struct {
	DisplayName *string `json:"displayName"`
	Bio         *string `json:"bio"`
	Region      *string `json:"region"`
}

type updateMeResponse getMeResponse

func newSessionUserResponse(user User) sessionUserResponse {
	return sessionUserResponse{
		ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
		Bio: user.Bio, Region: user.Region, AccountType: user.AccountType, Status: user.Status,
	}
}

func newGetMeResponse(user User) getMeResponse {
	response := getMeResponse{sessionUserResponse: newSessionUserResponse(user)}
	if user.ProfileImageID != nil {
		url := signedImageURL(user, *user.ProfileImageID)
		response.ProfileImageURL = &url
	}
	return response
}

func registerHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input registerRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) || !validUserText(input.DisplayName, 1, 100) || !validRegion(input.Region) {
			return NewAPIError(http.StatusBadRequest, "invalid_registration")
		}
		secretKey := uuid.NewString()
		hash := sha256.Sum256([]byte(secretKey))
		ctx := c.Request().Context()
		// ユーザー名の一意性はusers_usernameのユニークインデックスで保証し、テーブルロックは取らない。
		var user User
		// 検索用の列には、Go で正規化した値を一緒に書く(user_search.go)
		insertSQL := `INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES ($1, $2, $3, $4, $5) RETURNING ` + strings.Join(userColumns, ", ")
		args := []any{uuid.NewString(), input.Username, hex.EncodeToString(hash[:]), input.DisplayName, input.Region}
		if userSearchReady.Load() {
			insertSQL = `INSERT INTO users (id, username, secret_key_hash, display_name, region, search_username, search_display_name) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING ` + strings.Join(userColumns, ", ")
			args = append(args, normalizeSearchText(input.Username), normalizeSearchText(input.DisplayName))
		}
		err := h.DB.GetContext(ctx, &user, insertSQL, args...)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return NewAPIError(http.StatusConflict, "username_taken")
		}
		if err != nil {
			return err
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, registerResponse{SecretKey: secretKey, User: newSessionUserResponse(user)})
	}
}

func loginHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input loginRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		ctx := c.Request().Context()
		hash := sha256.Sum256([]byte(input.SecretKey))
		secretKeyHash := hex.EncodeToString(hash[:])
		randomToken := uuid.NewString()
		tokenHashBytes := sha256.Sum256([]byte(randomToken))
		// BEGIN〜COMMITを1往復で送る。1文目でユーザー行をロックし、2文目はロック後のスナップショットで
		// 秘密鍵と有効性を確かめ、合っていたときだけ既存セッションの無効化と新規作成を行う
		var user User
		var userFound, sessionCreated bool
		var session Session
		if err := runPipelinedTx(ctx, h.DB,
			pipelinedStmt{
				sql:  `SELECT id, username, secret_key_hash, display_name, bio, profile_image_id, region, account_type, status FROM users WHERE username = $1 FOR NO KEY UPDATE`,
				args: []any{input.Username},
				scan: scanFirstRow(&userFound, &user.ID, &user.Username, &user.SecretKeyHash, &user.DisplayName, &user.Bio, &user.ProfileImageID, &user.Region, &user.AccountType, &user.Status),
			},
			pipelinedStmt{
				sql: `
  WITH ok AS (
    SELECT id FROM users WHERE username = $1 AND status = 'active' AND secret_key_hash = $2
  ), invalidated AS (
    UPDATE login_sessions
    SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
    WHERE user_id IN (SELECT id FROM ok) AND active = true
  )
  INSERT INTO login_sessions (id, user_id, token_hash)
  SELECT $3, ok.id, $4 FROM ok
  CROSS JOIN LATERAL (SELECT pg_notify('` + sessionInvalidateChannel + `', ok.id::text)) AS notify
  RETURNING user_id, token_hash, created_at
 `,
				args: []any{input.Username, secretKeyHash, uuid.NewString(), hex.EncodeToString(tokenHashBytes[:])},
				scan: scanFirstRow(&sessionCreated, &session.UserID, &session.TokenHash, &session.CreatedAt),
			},
		); err != nil {
			return err
		}
		if !userFound || !sessionCreated || user.Status != "active" || user.SecretKeyHash != secretKeyHash {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		session.Token = randomToken
		// 置き換えた古いセッションをこのサーバーのキャッシュから消す(他サーバーへはコミット時にNOTIFYが届く)
		sessions.invalidateUser(user.ID)
		return c.JSON(http.StatusOK, loginResponse{Token: session.Token, User: newSessionUserResponse(user)})
	}
}

func logoutHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		if err := InvalidateLoginSession(h.DB, session.UserID, session.TokenHash); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func getMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		cached, epoch := userViews.begin(session.UserID)
		if cached.me != nil {
			return c.JSON(http.StatusOK, newGetMeResponse(*cached.me))
		}
		user, err := getUser(c.Request().Context(), h.ReadDB(c), session.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		userViews.putMe(epoch, session.UserID, user)
		return c.JSON(http.StatusOK, newGetMeResponse(user))
	}
}

func updateMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		var input updateMeRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.DisplayName == nil && input.Bio == nil && input.Region == nil) ||
			(input.DisplayName != nil && !validUserText(*input.DisplayName, 1, 100)) ||
			(input.Bio != nil && !validUserText(*input.Bio, 0, 500)) ||
			(input.Region != nil && !validRegion(*input.Region)) {
			return NewAPIError(http.StatusBadRequest, "invalid_profile")
		}
		ctx := c.Request().Context()
		// 指定された項目の更新と読み直しを1文(1往復)で行う。有効なユーザーだけを更新するので、
		// 前の実装(更新後に getUser で見つからなければロールバックして404)と同じく、無効なユーザーは何も変わらない。
		// リージョンを変えるときは、セッションのキャッシュがリージョンを持つので全サーバーで消す(通知はコミット時に届く)
		var user User
		// 表示名を変えるときは、検索用の列にも Go で正規化した値を書く(user_search.go)
		searchDisplayNameSet := ""
		args := []any{session.UserID, input.DisplayName, input.Bio, input.Region}
		if userSearchReady.Load() {
			searchDisplayNameSet = "search_display_name = COALESCE($5::text, search_display_name),"
			var normalized *string
			if input.DisplayName != nil {
				n := normalizeSearchText(*input.DisplayName)
				normalized = &n
			}
			args = append(args, normalized)
		}
		err := sqlx.GetContext(ctx, h.DB, &user, `
  WITH u AS (
    UPDATE users SET
      display_name = COALESCE($2::text, display_name),
      `+searchDisplayNameSet+`
      bio = COALESCE($3::text, bio),
      region = COALESCE($4::text, region),
      updated_at = clock_timestamp()
    WHERE id = $1 AND status = 'active'
    RETURNING `+strings.Join(userColumns, ", ")+`
  )
  SELECT `+prefixedUserColumns("u")+` FROM u
  LEFT JOIN LATERAL (SELECT pg_notify('`+sessionInvalidateChannel+`', u.id::text) WHERE $4::text IS NOT NULL) AS notify ON true
 `, args...)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		if input.Region != nil {
			sessions.invalidateUser(session.UserID)
		}
		userViews.invalidateUser(session.UserID)
		return c.JSON(http.StatusOK, updateMeResponse(newGetMeResponse(user)))
	}
}
