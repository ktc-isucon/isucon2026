package isureal

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jmoiron/sqlx"
)

// viewerContextSQL は閲覧者($1)のユーザー・当日投稿の有無・ブロック・友達・友達の友達を1行で返す。
// (IDはUUIDなので ',' と ':' を区切りに使える)
var viewerContextSQL = `
  WITH f AS (
    SELECT CASE WHEN user_low_id = $1 THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = $1 OR user_high_id = $1
  )
  SELECT ` + prefixedUserColumns("u") + `,
    EXISTS(
      SELECT 1 FROM posts p
      JOIN posting_windows posted ON posted.id = p.posting_window_id
      JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE p.author_id = u.id AND p.deleted_at IS NULL
    ) AS has_posted_today,
    COALESCE((SELECT string_agg(CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END, ',')
              FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1), '') AS blocked_ids,
    COALESCE((SELECT string_agg(id, ',') FROM f), '') AS friend_ids,
    -- 友達 f と友達関係にある相手ごとに、何人の友達とつながっているかを数える。
    -- (low, high) の両方向をインデックスで結合する(OR で全件走査しない)。両端が友達のペアは両方向で数える
    COALESCE((SELECT string_agg(other || ':' || n, ',')
              FROM (
                SELECT other, count(*) AS n FROM (
                  SELECT fs.user_high_id AS other FROM f JOIN friendships fs ON fs.user_low_id = f.id
                  UNION ALL
                  SELECT fs.user_low_id AS other FROM f JOIN friendships fs ON fs.user_high_id = f.id
                ) x GROUP BY other
              ) c), '') AS fof_counts
  FROM users u WHERE u.id = $1 AND u.status = 'active'
 `

// viewerRow は viewerContextSQL の1行。
type viewerRow struct {
	User
	HasPostedToday bool   `db:"has_posted_today"`
	BlockedIDs     string `db:"blocked_ids"`
	FriendIDs      string `db:"friend_ids"`
	// 友達の友達ごとの「共通の友達の数」("id:count" を ',' で連結)
	FriendOfFriendCounts string `db:"fof_counts"`
}

func (row viewerRow) toViewerContext() (viewerContext, error) {
	viewer := viewerContext{Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, Blocked: map[string]bool{}, MutualFriends: map[string]int{}}
	viewer.User = row.User
	if err := viewer.User.setImageKey(); err != nil {
		return viewer, err
	}
	viewer.HasPostedToday = row.HasPostedToday
	for _, id := range splitIDs(row.BlockedIDs) {
		viewer.Blocked[id] = true
	}
	for _, id := range splitIDs(row.FriendIDs) {
		viewer.Friends[id] = true
	}
	for _, entry := range splitIDs(row.FriendOfFriendCounts) {
		other, count, _ := strings.Cut(entry, ":")
		n, err := strconv.Atoi(count)
		if err != nil {
			return viewer, fmt.Errorf("invalid friend-of-friend count %q: %w", entry, err)
		}
		viewer.FriendsOfFriends[other] = true
		if other != viewer.ID {
			viewer.MutualFriends[other] += n
		}
	}
	return viewer, nil
}

// viewerStmt は viewerContextSQL をバッチの1文にする。閲覧者が見つかれば found を true にする。
func viewerStmt(viewerID string, viewer *viewerContext, found *bool) pipelinedStmt {
	return pipelinedStmt{
		sql:  viewerContextSQL,
		args: []any{viewerID},
		scan: func(rows pgx.Rows) error {
			if !rows.Next() {
				return nil
			}
			var row viewerRow
			dest := append(userScanDest(&row.User), &row.HasPostedToday, &row.BlockedIDs, &row.FriendIDs, &row.FriendOfFriendCounts)
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			v, err := row.toViewerContext()
			if err != nil {
				return err
			}
			*viewer = v
			*found = true
			return nil
		},
	}
}

// postsWithAuthorSQL は selectPostsWithAuthor と同じ列・結合で投稿を返すSQLを作る。
func postsWithAuthorSQL(conditions string) string {
	return `
  SELECT ` + postColumns + `, ` + prefixedUserColumns("u") + `
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL ` + conditions
}

// postsWithAuthorStmt は postsWithAuthorSQL をバッチの1文にし、結果を posts に読む。
func postsWithAuthorStmt(conditions string, posts *[]PostWithAuthor, args ...any) pipelinedStmt {
	return pipelinedStmt{
		sql:  postsWithAuthorSQL(conditions),
		args: args,
		scan: func(rows pgx.Rows) error {
			for rows.Next() {
				var post PostWithAuthor
				dest := append(postScanDest(&post.Post), userScanDest(&post.Author)...)
				if err := rows.Scan(dest...); err != nil {
					return err
				}
				if err := post.Author.setImageKey(); err != nil {
					return err
				}
				*posts = append(*posts, post)
			}
			return nil
		},
	}
}

// commentsWithAuthorStmt は selectCommentsWithAuthor と同じ列・結合のコメントをバッチの1文で読む。
func commentsWithAuthorStmt(conditions string, comments *[]CommentWithAuthor, args ...any) pipelinedStmt {
	return pipelinedStmt{
		sql: `
  SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, ` + prefixedUserColumns("u") + `
  FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.deleted_at IS NULL ` + conditions,
		args: args,
		scan: func(rows pgx.Rows) error {
			for rows.Next() {
				var comment CommentWithAuthor
				dest := append([]any{&comment.ID, &comment.PostID, &comment.AuthorID, &comment.Body, &comment.CreatedAt, &comment.DeletedAt},
					userScanDest(&comment.Author)...)
				if err := rows.Scan(dest...); err != nil {
					return err
				}
				if err := comment.Author.setImageKey(); err != nil {
					return err
				}
				*comments = append(*comments, comment)
			}
			return nil
		},
	}
}

// currentRealMojisStmt は selectCurrentRealMojis と同じ列・結合のRealMojiをバッチの1文で読む。
func currentRealMojisStmt(conditions string, reactions *[]RealMojiWithReactor, args ...any) pipelinedStmt {
	return pipelinedStmt{
		sql: `
  SELECT r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at,
         ` + prefixedUserColumns("u") + `
  FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
  WHERE r.replaced_at IS NULL AND r.deleted_at IS NULL ` + conditions,
		args: args,
		scan: func(rows pgx.Rows) error {
			for rows.Next() {
				var reaction RealMojiWithReactor
				dest := append([]any{&reaction.ID, &reaction.PostID, &reaction.ReactorID, &reaction.ImageID, &reaction.ReactionType,
					&reaction.CreatedAt, &reaction.ReplacedAt, &reaction.DeletedAt}, userScanDest(&reaction.Reactor)...)
				if err := rows.Scan(dest...); err != nil {
					return err
				}
				if err := reaction.Reactor.setImageKey(); err != nil {
					return err
				}
				*reactions = append(*reactions, reaction)
			}
			return nil
		},
	}
}

// loadPostAndViewer は投稿(作者つき)と閲覧者の情報を、extra の文と一緒に1回の往復で読む。
// 投稿が無ければ postFound=false、閲覧者が無効なら viewerFound=false を返す。
func loadPostAndViewer(ctx context.Context, db *sqlx.DB, postID, viewerID string, extra ...pipelinedStmt) (post PostWithAuthor, postFound bool, viewer viewerContext, viewerFound bool, err error) {
	var posts []PostWithAuthor
	stmts := append([]pipelinedStmt{
		postsWithAuthorStmt(`AND p.id = $1`, &posts, postID),
		viewerStmt(viewerID, &viewer, &viewerFound),
	}, extra...)
	if err = runPipelinedSnapshot(ctx, db, stmts...); err != nil {
		return
	}
	if len(posts) > 0 {
		post, postFound = posts[0], true
	}
	return
}

// canCommentOn は canComment と同じ判定を、読み込み済みの投稿と閲覧者で行う。
func canCommentOn(post PostWithAuthor, postFound bool, viewer viewerContext, viewerFound bool) bool {
	if !postFound || !viewerFound {
		return false
	}
	if !viewer.canView(post.Post) || post.WindowEndedAt != nil {
		return false
	}
	if viewer.ID == post.AuthorID {
		return viewer.HasPostedToday
	}
	if post.Visibility == "public" {
		return true
	}
	return viewer.Friends[post.AuthorID]
}
