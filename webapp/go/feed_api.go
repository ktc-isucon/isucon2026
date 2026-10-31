package isureal

import (
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

type getFeedRequest struct {
	Feed           string     `param:"feed"`
	BeforeScore    *int64     `query:"beforeScore"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type feedItemResponse struct {
	createPostResponse
	Score *int64 `json:"score,omitempty"`
}

type getFeedResponse struct {
	Items   []feedItemResponse `json:"items"`
	HasMore bool               `json:"hasMore"`
}

func getFeedHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getFeedRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		switch input.Feed {
		case "friends", "friends-of-friends", "public", "following":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if (input.BeforePostedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ranked := input.Feed == "friends-of-friends"
		if ranked && (input.BeforeScore == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewerID := GetSession(c).UserID
		beforePostedAt, beforeID := input.BeforePostedAt, input.BeforeID
		if ranked {
			beforePostedAt, beforeID = nil, ""
		}
		// 閲覧者・フォロー・投稿の候補(・おすすめのスコアの元になる行)を、同じスナップショットで1回の往復で読む。
		// 候補はSQLで閲覧可否とフィードの条件で絞り、並べ替えのないフィードは51件で打ち切る。
		// SQLの条件は loadViewerContext・canView・matchesFeed と同じ定義で、Go側でも最終確認する。
		conditions := feedCandidateConditions(input.Feed)
		if ranked {
			// おすすめのスコアをSQLで求めて並べ替え、カーソルの後ろの51件だけを読む(候補全体や友達関係を転送しない)。
			// Go側の閲覧可否・フィードの判定に合わない行が1件でもあれば、従来の全件の経路で求め直す。
			var viewer viewerContext
			var viewerFound bool
			var rows []rankedFeedRow
			if err := runPipelinedSnapshot(ctx, h.ReadDB(c),
				viewerStmt(viewerID, &viewer, &viewerFound),
				rankedFeedStmt(&rows, viewerID, input.BeforeScore, input.BeforePostedAt, input.BeforeID),
			); err != nil {
				return err
			}
			if !viewerFound {
				return c.JSON(http.StatusOK, getFeedResponse{Items: []feedItemResponse{}})
			}
			if items, ok := rankedFeedItems(viewer, rows); ok {
				hasMore := len(items) > 50
				if hasMore {
					items = items[:50]
				}
				return c.JSON(http.StatusOK, getFeedResponse{Items: items, HasMore: hasMore})
			}
		}
		postConditions := conditions + ` ORDER BY p.posted_at DESC, p.id DESC`
		if !ranked {
			postConditions += ` LIMIT 51`
		}
		var viewer viewerContext
		var viewerFound bool
		var followeeIDs []string
		var posts []PostWithAuthor
		var scoreRows []recommendationRow
		stmts := []pipelinedStmt{
			viewerStmt(viewerID, &viewer, &viewerFound),
			{
				sql:  `SELECT followee_id FROM follows WHERE follower_id = $1`,
				args: []any{viewerID},
				scan: func(rows pgx.Rows) error {
					for rows.Next() {
						var id string
						if err := rows.Scan(&id); err != nil {
							return err
						}
						followeeIDs = append(followeeIDs, id)
					}
					return nil
				},
			},
			postsWithAuthorStmt(postConditions, &posts, viewerID, beforePostedAt, beforeID),
		}
		if ranked {
			stmts = append(stmts, recommendationRowsStmt(conditions, &scoreRows, viewerID, beforePostedAt, beforeID))
		}
		if err := runPipelinedSnapshot(ctx, h.ReadDB(c), stmts...); err != nil {
			return err
		}
		if !viewerFound {
			return c.JSON(http.StatusOK, getFeedResponse{Items: []feedItemResponse{}})
		}
		following := map[string]bool{}
		if input.Feed == "following" {
			for _, id := range followeeIDs {
				following[id] = true
			}
		}
		candidates := make([]PostWithAuthor, 0)
		for _, post := range posts {
			if viewer.canView(post.Post) && matchesFeed(viewer, following, post.Post, input.Feed) {
				candidates = append(candidates, post)
			}
		}
		var scores map[string]int64
		if ranked {
			scores = computeRecommendationScores(viewer, candidates, scoreRows)
			sort.SliceStable(candidates, func(i, j int) bool {
				return scores[candidates[i].ID] > scores[candidates[j].ID]
			})
		}
		items := make([]feedItemResponse, 0)
		for _, post := range candidates {
			score := scores[post.ID]
			if ranked && input.BeforeScore != nil && !rankedBefore(score, post.Post, *input.BeforeScore, *input.BeforePostedAt, input.BeforeID) {
				continue
			}
			item := feedItemResponse{createPostResponse: newPostResponse(post.Post, post.Author)}
			if ranked {
				item.Score = &score
			}
			items = append(items, item)
			if len(items) == 51 {
				break
			}
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getFeedResponse{Items: items, HasMore: hasMore})
	}
}

// rankedFeedItems は rankedFeedStmt の結果を応答の形にする。どれかが閲覧可否・フィードの判定に合わなければ ok=false。
func rankedFeedItems(viewer viewerContext, rows []rankedFeedRow) ([]feedItemResponse, bool) {
	items := make([]feedItemResponse, 0, len(rows))
	for _, row := range rows {
		if !viewer.canView(row.post.Post) || !matchesFeed(viewer, nil, row.post.Post, "friends-of-friends") {
			return nil, false
		}
		score := row.score
		item := feedItemResponse{createPostResponse: newPostResponse(row.post.Post, row.post.Author)}
		item.Score = &score
		items = append(items, item)
	}
	return items, true
}

func rankedBefore(score int64, post Post, beforeScore int64, beforePostedAt time.Time, beforeID string) bool {
	if score != beforeScore {
		return score < beforeScore
	}
	if !post.PostedAt.Equal(beforePostedAt) {
		return post.PostedAt.Before(beforePostedAt)
	}
	return post.ID < beforeID
}

// feedFriendIDsSQL は閲覧者($1)の友達のIDを返すサブクエリ(loadViewerContext の Friends と同じ)。
const feedFriendIDsSQL = `(SELECT CASE WHEN fr.user_low_id = $1 THEN fr.user_high_id ELSE fr.user_low_id END
     FROM friendships fr WHERE fr.user_low_id = $1 OR fr.user_high_id = $1)`

// feedFriendOfFriendIDsSQL は閲覧者の友達と友達関係にある相手のIDを返すサブクエリ(loadViewerContext の FriendsOfFriends と同じ)。
const feedFriendOfFriendIDsSQL = `(SELECT fs.user_high_id FROM friendships fs WHERE fs.user_low_id IN ` + feedFriendIDsSQL + `
     UNION ALL
     SELECT fs.user_low_id FROM friendships fs WHERE fs.user_high_id IN ` + feedFriendIDsSQL + `)`

// feedCandidateConditions は、フィードの候補の投稿を絞る条件(posts p, posting_windows w, users u の WHERE に続ける)。
// $1 は閲覧者、$2/$3 はページングのカーソル。canView と matchesFeed と同じ判定を SQL で行う。
func feedCandidateConditions(feed string) string {
	var feedRule string
	switch feed {
	case "public":
		feedRule = `p.visibility = 'public'`
	case "following":
		feedRule = `p.author_id IN (SELECT followee_id FROM follows WHERE follower_id = $1)`
	case "friends":
		feedRule = `p.author_id IN ` + feedFriendIDsSQL
	case "friends-of-friends":
		feedRule = `p.author_id NOT IN ` + feedFriendIDsSQL + ` AND p.author_id IN ` + feedFriendOfFriendIDsSQL
	default:
		feedRule = `false`
	}
	return `
   AND p.author_id <> $1 AND w.ended_at IS NULL
   AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
   -- 閲覧者が有効で、当日に投稿済み(loadViewerContext の has_posted_today と同じ)
   AND EXISTS (
     SELECT 1 FROM users v
     JOIN posts vp ON vp.author_id = v.id AND vp.deleted_at IS NULL
     JOIN posting_windows posted ON posted.id = vp.posting_window_id
     JOIN posting_windows cur ON cur.region = v.region AND cur.ended_at IS NULL AND cur.local_date = posted.local_date
     WHERE v.id = $1 AND v.status = 'active')
   -- どちらかがブロックしていない
   AND NOT EXISTS (SELECT 1 FROM block_relations b
                   WHERE (b.blocker_id = $1 AND b.blocked_id = p.author_id) OR (b.blocker_id = p.author_id AND b.blocked_id = $1))
   -- 公開範囲(canView と同じ)
   AND (p.visibility = 'public'
        OR (p.visibility = 'friends' AND p.author_id IN ` + feedFriendIDsSQL + `)
        OR (p.visibility = 'friends_of_friends' AND (p.author_id IN ` + feedFriendIDsSQL + ` OR p.author_id IN ` + feedFriendOfFriendIDsSQL + `)))
   AND ` + feedRule
}

// recommendationRow は、投稿者の友達関係(kind='f')と投稿への反応(kind='e')の1行。
type recommendationRow struct {
	Kind string
	A    string
	B    string
}

// recommendationRowsStmt は、候補(conditions で絞った投稿)の投稿者の友達関係と、候補への反応を読む。
// 候補の集合は投稿一覧と同じスナップショットで同じ条件から求めるので、Go側の候補と同じかその上位集合になる
// (上位集合の分は、候補でない投稿者・投稿のスコアにしか影響しない)。
func recommendationRowsStmt(conditions string, rows *[]recommendationRow, args ...any) pipelinedStmt {
	return pipelinedStmt{
		sql: `
  WITH cand AS (
    SELECT p.id, p.author_id
    FROM posts p
    JOIN posting_windows w ON w.id = p.posting_window_id
    JOIN users u ON u.id = p.author_id AND u.status = 'active'
    WHERE p.deleted_at IS NULL ` + conditions + `
  )
  SELECT 'f' AS kind, user_low_id AS a, user_high_id AS b FROM friendships
  WHERE user_low_id IN (SELECT author_id FROM cand) OR user_high_id IN (SELECT author_id FROM cand)
  UNION ALL
  SELECT 'e', post_id, user_id FROM (
    SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
    JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
    WHERE r.post_id IN (SELECT id FROM cand) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
    UNION
    SELECT c.post_id, c.author_id AS user_id FROM comments c
    JOIN users u ON u.id = c.author_id AND u.status = 'active'
    WHERE c.post_id IN (SELECT id FROM cand) AND c.deleted_at IS NULL
  ) engagements
 `,
		args: args,
		scan: func(r pgx.Rows) error {
			for r.Next() {
				var row recommendationRow
				if err := r.Scan(&row.Kind, &row.A, &row.B); err != nil {
					return err
				}
				*rows = append(*rows, row)
			}
			return nil
		},
	}
}

// computeRecommendationScores は、候補の投稿ごとのおすすめのスコアを求める(元の recommendationScores と同じ計算)。
func computeRecommendationScores(viewer viewerContext, posts []PostWithAuthor, rows []recommendationRow) map[string]int64 {
	scores := make(map[string]int64, len(posts))
	if len(posts) == 0 {
		return scores
	}
	connections := map[string]int64{}
	friendEngagements := map[string]int64{}
	for _, row := range rows {
		if row.Kind == "f" {
			connections[row.A] += int64(viewer.MutualFriends[row.B])
			connections[row.B] += int64(viewer.MutualFriends[row.A])
		} else if viewer.Friends[row.B] {
			friendEngagements[row.A]++
		}
	}

	for _, post := range posts {
		scores[post.ID] = 10*friendEngagements[post.ID] + 3*int64(viewer.MutualFriends[post.AuthorID]) + min(connections[post.AuthorID], 30)
	}
	return scores
}

func matchesFeed(viewer viewerContext, following map[string]bool, post Post, feed string) bool {
	switch feed {
	case "public":
		return post.Visibility == "public"
	case "following":
		return following[post.AuthorID]
	case "friends":
		return viewer.Friends[post.AuthorID]
	case "friends-of-friends":
		return !viewer.Friends[post.AuthorID] && viewer.FriendsOfFriends[post.AuthorID]
	default:
		return false
	}
}
