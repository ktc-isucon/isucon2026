package isureal

import (
	"context"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type searchUsersRequest struct {
	Q              string `query:"q"`
	BeforeUsername string `query:"beforeUsername"`
	BeforeID       string `query:"beforeId"`
}

type searchUsersResponse struct {
	Items   []relationshipUserResponse `json:"items"`
	HasMore bool                       `json:"hasMore"`
}

func normalizeSearchText(value string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
}

func searchUsersHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input searchUsersRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Q, 1, 100) {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		if (input.BeforeUsername == "") != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		var users []User
		var err error
		if userSearchReady.Load() {
			users, err = searchUsersIndexed(ctx, h.ReadDB(c), viewer, input)
		} else {
			users, err = searchUsersLegacy(ctx, h.ReadDB(c), viewer, input)
		}
		if err != nil {
			return err
		}
		items := make([]relationshipUserResponse, 0, len(users))
		for _, user := range users {
			if err := user.setImageKey(); err != nil {
				return err
			}
			if user.AccountType != "certified_public" {
				user.Bio = ""
			}
			items = append(items, newRelationshipUserResponse(user))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
	}
}

// searchUsersIndexed は、正規化した列の pg_trgm インデックスで部分一致を絞り、並び順・カーソル・ブロックの除外は
// 今までと同じ条件で、最大51人を返す。最後に Go 側でも今までと同じ判定(strings.Contains)をかけ、
// 万一外れた行があっても足りない分は続きから読み直すので、結果は全件取得の判定と一致する。
func searchUsersIndexed(ctx context.Context, db sqlx.QueryerContext, viewer string, input searchUsersRequest) ([]User, error) {
	query := normalizeSearchText(input.Q)
	pattern := likeContainsPattern(query)
	const pageSize = 64
	afterUsername, afterID := input.BeforeUsername, input.BeforeID
	matched := make([]User, 0, 51)
	for {
		var page []User
		if err := sqlx.SelectContext(ctx, db, &page, `
   SELECT `+prefixedUserColumns("u")+` FROM users u
   WHERE u.status='active' AND u.id<>$1 AND ($2='' OR (u.username,u.id)>($2,$3))
     AND (u.search_username LIKE $4 ESCAPE '\' OR u.search_display_name LIKE $4 ESCAPE '\')
     AND NOT EXISTS (
       SELECT 1 FROM block_relations b
       WHERE (b.blocker_id=$1 AND b.blocked_id=u.id) OR (b.blocked_id=$1 AND b.blocker_id=u.id)
     )
   ORDER BY u.username, u.id
   LIMIT $5
  `, viewer, afterUsername, afterID, pattern, pageSize); err != nil {
			return nil, err
		}
		for _, user := range page {
			if !strings.Contains(normalizeSearchText(user.Username), query) && !strings.Contains(normalizeSearchText(user.DisplayName), query) {
				continue
			}
			matched = append(matched, user)
			if len(matched) == 51 {
				return matched, nil
			}
		}
		if len(page) < pageSize {
			return matched, nil
		}
		last := page[len(page)-1]
		afterUsername, afterID = last.Username, last.ID
	}
}

// searchUsersLegacy は列の準備ができていないときの、今までの検索(絞り込みに必要な列だけを全件取り、アプリで判定する)。
func searchUsersLegacy(ctx context.Context, db sqlx.QueryerContext, viewer string, input searchUsersRequest) ([]User, error) {
	{
		// 絞り込みに必要な列(id, username, display_name)だけを全件取り、部分一致はアプリで判定する。
		// 正規化はUnicodeのcase folding + NFCなので、SQLでは同じ結果を保証できない。
		// europe/americaは帯域制限があるため、secret_key_hashやbioを含む全列の全件取得を避ける
		var candidates []struct {
			ID          string `db:"id"`
			Username    string `db:"username"`
			DisplayName string `db:"display_name"`
		}
		if err := sqlx.SelectContext(ctx, db, &candidates, `
   SELECT u.id, u.username, u.display_name FROM users u
   WHERE u.status='active' AND u.id<>$1 AND ($2='' OR (u.username,u.id)>($2,$3))
     AND NOT EXISTS (
       SELECT 1 FROM block_relations b
       WHERE (b.blocker_id=$1 AND b.blocked_id=u.id) OR (b.blocked_id=$1 AND b.blocker_id=u.id)
     )
   ORDER BY u.username, u.id
  `, viewer, input.BeforeUsername, input.BeforeID); err != nil {
			return nil, err
		}
		query := normalizeSearchText(input.Q)
		matchedIDs := make([]string, 0, 51)
		for _, candidate := range candidates {
			if !strings.Contains(normalizeSearchText(candidate.Username), query) && !strings.Contains(normalizeSearchText(candidate.DisplayName), query) {
				continue
			}
			matchedIDs = append(matchedIDs, candidate.ID)
			if len(matchedIDs) == 51 {
				break
			}
		}
		// 一致した最大51人分だけ、応答に必要な全列を取る
		result := make([]User, 0, len(matchedIDs))
		if len(matchedIDs) > 0 {
			var users []User
			if err := sqlx.SelectContext(ctx, db, &users, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = ANY($1) AND status='active'`, matchedIDs); err != nil {
				return nil, err
			}
			byID := make(map[string]User, len(users))
			for _, user := range users {
				byID[user.ID] = user
			}
			for _, id := range matchedIDs {
				if user, ok := byID[id]; ok {
					result = append(result, user)
				}
			}
		}
		return result, nil
	}
}

type getUserRequest struct {
	UserID string `param:"userId"`
}

type getUserResponse relationshipUserResponse

func getUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		// ユーザー・ブロック・友達かを1回の往復で読む(getUser, isBlocked, getFriendIDs と同じ条件)
		var user User
		var found, blocked, friend bool
		if err := runPipelinedSnapshot(ctx, h.ReadDB(c),
			pipelinedStmt{
				sql:  `SELECT ` + strings.Join(userColumns, ", ") + ` FROM users WHERE id = $1 AND status = 'active'`,
				args: []any{input.UserID},
				scan: scanFirstRow(&found, userScanDest(&user)...),
			},
			pipelinedStmt{
				sql:  `SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))`,
				args: []any{viewer, input.UserID},
				scan: scanFirstRow(new(bool), &blocked),
			},
			pipelinedStmt{
				sql:  `SELECT EXISTS(SELECT 1 FROM friendships WHERE (user_low_id=$1 AND user_high_id=$2) OR (user_low_id=$2 AND user_high_id=$1))`,
				args: []any{viewer, input.UserID},
				scan: scanFirstRow(new(bool), &friend),
			},
		); err != nil {
			return err
		}
		if !found {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		if blocked {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if user.AccountType != "certified_public" && !friend {
			user.Bio = ""
		}
		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(user)))
	}
}

func newRelationshipUserResponse(user User) relationshipUserResponse {
	return relationshipUserResponse{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, ProfileImageURL: newGetMeResponse(user).ProfileImageURL, Bio: user.Bio, AccountType: user.AccountType}
}
