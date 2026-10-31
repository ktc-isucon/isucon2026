package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"

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
		// 絞り込みに必要な列(id, username, display_name)だけを全件取り、部分一致はアプリで判定する。
		// 正規化はUnicodeのcase folding + NFCなので、SQLでは同じ結果を保証できない。
		// europe/americaは帯域制限があるため、secret_key_hashやbioを含む全列の全件取得を避ける
		var candidates []struct {
			ID          string `db:"id"`
			Username    string `db:"username"`
			DisplayName string `db:"display_name"`
		}
		if err := h.ReadDB(c).SelectContext(ctx, &candidates, `
   SELECT u.id, u.username, u.display_name FROM users u
   WHERE u.status='active' AND u.id<>$1 AND ($2='' OR (u.username,u.id)>($2,$3))
     AND NOT EXISTS (
       SELECT 1 FROM block_relations b
       WHERE (b.blocker_id=$1 AND b.blocked_id=u.id) OR (b.blocked_id=$1 AND b.blocker_id=u.id)
     )
   ORDER BY u.username, u.id
  `, viewer, input.BeforeUsername, input.BeforeID); err != nil {
			return err
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
		items := make([]relationshipUserResponse, 0, len(matchedIDs))
		if len(matchedIDs) > 0 {
			var users []User
			if err := h.ReadDB(c).SelectContext(ctx, &users, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = ANY($1) AND status='active'`, matchedIDs); err != nil {
				return err
			}
			byID := make(map[string]User, len(users))
			for _, user := range users {
				byID[user.ID] = user
			}
			for _, id := range matchedIDs {
				user, ok := byID[id]
				if !ok {
					continue
				}
				if err := user.setImageKey(); err != nil {
					return err
				}
				if user.AccountType != "certified_public" {
					user.Bio = ""
				}
				items = append(items, newRelationshipUserResponse(user))
			}
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
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
		user, err := getUser(ctx, h.ReadDB(c), input.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		blocked, err := isBlocked(ctx, h.ReadDB(c), viewer, user.ID)
		if err != nil {
			return err
		}
		if blocked {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		friends, err := getFriendIDs(ctx, h.ReadDB(c), viewer)
		if err != nil {
			return err
		}
		if user.AccountType != "certified_public" && !slices.Contains(friends, user.ID) {
			user.Bio = ""
		}
		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(user)))
	}
}

func newRelationshipUserResponse(user User) relationshipUserResponse {
	return relationshipUserResponse{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, ProfileImageURL: newGetMeResponse(user).ProfileImageURL, Bio: user.Bio, AccountType: user.AccountType}
}
