package isureal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type Session struct {
	Region    string    `db:"region" json:"-"`
	Token     string    `db:"-" json:"-"`
	UserID    string    `db:"user_id"`
	TokenHash string    `db:"token_hash"`
	CreatedAt time.Time `db:"created_at"`
}

func GetSession(c *echo.Context) *Session {
	sess, ok := c.Get("session").(*Session)
	if !ok {
		return nil
	}
	return sess
}

func getSessionFromHeader(c *echo.Context, db *sqlx.DB) (*Session, error) {
	authHeader := c.Request().Header.Get(echo.HeaderAuthorization)
	if authHeader == "" {
		return nil, nil
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		c.Logger().Warn("Invalid authorization header format(not started with Bearer)")
		return nil, NewAPIError(http.StatusUnauthorized, "invalid_session")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")

	tokenHashBytes := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	cached, hit, epoch := sessions.begin(tokenHash)
	if hit {
		return &cached, nil
	}
	var session Session
	err := db.GetContext(c.Request().Context(), &session, `
		SELECT s.user_id, s.token_hash, s.created_at, u.region
		FROM login_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.active = true AND u.status = 'active'
	`, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	sessions.put(epoch, session)
	return &session, nil
}

func SessionMiddleware(handlerContext *HandlerContext, authRequired bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			session, err := getSessionFromHeader(c, handlerContext.ReadDB(c))
			if err != nil {
				return err
			}
			if session == nil && authRequired {
				return NewAPIError(http.StatusUnauthorized, "invalid_session")
			}
			c.Set("session", session)
			return next(c)
		}
	}
}

func InvalidateLoginSession(db *sqlx.DB, userID string, tokenHash string) error {
	// 行ロックとセッションの無効化、他サーバーへのキャッシュ無効化の通知を1文(1往復、暗黙のトランザクション)で行う
	var locked int
	if err := db.Get(&locked, `
		WITH u AS (
			SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE
		), invalidated AS (
			UPDATE login_sessions
			SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
			WHERE user_id IN (SELECT id FROM u) AND active = true AND token_hash = $2
		)
		SELECT count(*) FROM u CROSS JOIN (SELECT pg_notify('`+sessionInvalidateChannel+`', $1::text)) AS notify
	`, userID, tokenHash); err != nil {
		return fmt.Errorf("failed to invalidate sessions: %w", err)
	}
	sessions.invalidateUser(userID)
	if locked == 0 {
		return fmt.Errorf("failed to lock user: %w", sql.ErrNoRows)
	}

	return nil
}

// 2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか
func CreateLoginSession(db *sqlx.DB, userID string) (*Session, error) {
	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	session, err := createLoginSessionTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}
	sessions.invalidateUser(userID)
	return session, nil
}

func createLoginSessionTx(ctx context.Context, tx *sqlx.Tx, userID string) (*Session, error) {
	var lockedUserID string
	if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE`, userID); err != nil {
		return nil, fmt.Errorf("failed to lock user: %w", err)
	}
	return replaceLoginSessionTx(ctx, tx, userID)
}

// replaceLoginSessionTx は、呼び出し側でユーザーの行ロックと有効性の確認が済んでいる前提で、
// 既存セッションの無効化・新しいセッションの作成・読み直しを1往復で行う。
// CTE内のUPDATEは同じスナップショットを見るので、新しく作るセッションは無効化されない
func replaceLoginSessionTx(ctx context.Context, tx *sqlx.Tx, userID string) (*Session, error) {
	randomToken := uuid.NewString()
	tokenHashBytes := sha256.Sum256([]byte(randomToken))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	var session Session
	if err := tx.GetContext(ctx, &session, `
  WITH invalidated AS (
    UPDATE login_sessions
    SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
    WHERE user_id = $2 AND active = true
  )
  INSERT INTO login_sessions (id, user_id, token_hash)
  SELECT $1, $2, $3 FROM (SELECT pg_notify('`+sessionInvalidateChannel+`', $2::text)) AS notify
  RETURNING user_id, token_hash, created_at
 `, uuid.NewString(), userID, tokenHash); err != nil {
		return nil, fmt.Errorf("failed to replace session: %w", err)
	}
	session.Token = randomToken
	return &session, nil
}

func getLoginSession(ctx context.Context, db sqlx.QueryerContext, sessionID string) (*Session, error) {
	var session Session
	if err := sqlx.GetContext(ctx, db, &session, `SELECT user_id, token_hash, created_at FROM login_sessions WHERE id = $1`, sessionID); err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	return &session, nil
}
