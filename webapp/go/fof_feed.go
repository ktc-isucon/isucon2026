package isureal

import (
	"time"

	"github.com/jackc/pgx/v5"
)

// rankedFeedRow は friends-of-friends フィードの1件(投稿・作者・おすすめのスコア)。
type rankedFeedRow struct {
	post  PostWithAuthor
	score int64
}

// rankedFeedStmt は friends-of-friends フィードの候補ごとのおすすめのスコアをSQLで求め、
// スコア・投稿時刻・ID の順に並べてカーソルの後ろから51件だけを返す。
//
// スコアは computeRecommendationScores と同じ定義:
//
//	10 × (閲覧者の友達で、その投稿に反応(有効なRealMoji・コメント)した人の数)
//	+ 3 × (閲覧者と投稿者の共通の友達の数)
//	+ min(投稿者の友達それぞれの「閲覧者との共通の友達の数」の合計, 30)
//
// 共通の友達の数は viewerContextSQL の fof_counts と同じ数え方(閲覧者自身は除く)。
// 各投稿のスコアは、その投稿と投稿者だけで決まり候補の集合に依存しないので、SQL で並べ替えて打ち切ってよい。
// 並び順は、元の「投稿時刻・ID の降順に並べてからスコアで安定ソート」と同じ。
// カーソルの ID の比較は Go の文字列比較(バイト順)と同じになるよう COLLATE "C" にする。
//
// $1 閲覧者、$2/$3/$4 は beforeScore / beforePostedAt / beforeId(なければ NULL)。
func rankedFeedStmt(rows *[]rankedFeedRow, viewerID string, beforeScore *int64, beforePostedAt *time.Time, beforeID string) pipelinedStmt {
	var cursorID *string
	if beforeScore != nil {
		cursorID = &beforeID
	}
	return pipelinedStmt{
		sql: `
  WITH vf AS (
    SELECT CASE WHEN user_low_id = $1 THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = $1 OR user_high_id = $1
  ), mutual AS MATERIALIZED (
    SELECT other, count(*) AS n FROM (
      SELECT fs.user_high_id AS other FROM vf JOIN friendships fs ON fs.user_low_id = vf.id
      UNION ALL
      SELECT fs.user_low_id AS other FROM vf JOIN friendships fs ON fs.user_high_id = vf.id
    ) x WHERE other <> $1 GROUP BY other
  ), open_posts AS MATERIALIZED (
    -- 開いている投稿期間の投稿を先に posting_window_id のインデックスで取り出す
    -- (投稿者ごとに全期間の投稿を読んでから期間で捨てると、友達の多い閲覧者で非常に重くなる)
    SELECT p.id, p.author_id, p.posted_at, p.visibility
    FROM posting_windows w
    JOIN posts p ON p.posting_window_id = w.id
    WHERE w.ended_at IS NULL AND p.deleted_at IS NULL
  ), cand AS MATERIALIZED (
    -- feedCandidateConditions("friends-of-friends") と同じ候補を、開いている投稿期間の投稿から選ぶ。
    -- 友達の友達で友達ではない投稿者なので、公開範囲は public と friends_of_friends だけが見える(friends は見えない)
    SELECT p.id, p.author_id, p.posted_at
    FROM open_posts p
    JOIN users u ON u.id = p.author_id AND u.status = 'active'
    WHERE p.author_id IN (SELECT other FROM mutual)
      AND p.author_id NOT IN (SELECT id FROM vf)
      AND p.author_id <> $1
      AND p.visibility IN ('public', 'friends_of_friends')
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
  ), authors AS (
    SELECT DISTINCT author_id FROM cand
  ), conn AS MATERIALIZED (
    -- 投稿者の友達それぞれの「閲覧者との共通の友達の数」の合計(friendships の両方向をインデックスで結合する)
    SELECT author_id, sum(n) AS c FROM (
      SELECT a.author_id, m.n FROM authors a
      JOIN friendships fs ON fs.user_low_id = a.author_id
      JOIN mutual m ON m.other = fs.user_high_id
      UNION ALL
      SELECT a.author_id, m.n FROM authors a
      JOIN friendships fs ON fs.user_high_id = a.author_id
      JOIN mutual m ON m.other = fs.user_low_id
    ) z GROUP BY author_id
  ), eng AS MATERIALIZED (
    SELECT post_id, count(*) AS e FROM (
      SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
      JOIN users ru ON ru.id = r.reactor_id AND ru.status = 'active'
      WHERE r.post_id IN (SELECT id FROM cand) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
      UNION
      SELECT c.post_id, c.author_id AS user_id FROM comments c
      JOIN users cu ON cu.id = c.author_id AND cu.status = 'active'
      WHERE c.post_id IN (SELECT id FROM cand) AND c.deleted_at IS NULL
    ) g WHERE user_id IN (SELECT id FROM vf) GROUP BY post_id
  ), maps AS MATERIALIZED (
    -- CTE の行数の見積もりが外れて入れ子ループの結合になるのを避けるため、
    -- 反応数・共通の友達の数・つながりの合計をそれぞれ jsonb のオブジェクトにまとめ(1回だけ計算)、投稿ごとにキーで引く
    SELECT (SELECT jsonb_object_agg(post_id, e) FROM eng) AS em,
           (SELECT jsonb_object_agg(other, n) FROM mutual) AS mm,
           (SELECT jsonb_object_agg(author_id, c) FROM conn) AS cm
  ), scored AS (
    SELECT cand.id, cand.posted_at,
      (10 * COALESCE((maps.em ->> cand.id)::bigint, 0)
       + 3 * COALESCE((maps.mm ->> cand.author_id)::bigint, 0)
       + LEAST(COALESCE((maps.cm ->> cand.author_id)::numeric, 0), 30))::bigint AS score
    FROM cand CROSS JOIN maps
  ), page AS (
    SELECT id, posted_at, score FROM scored
    WHERE $2::bigint IS NULL
       OR score < $2::bigint
       OR (score = $2::bigint AND (posted_at < $3::timestamptz
                                   OR (posted_at = $3::timestamptz AND id COLLATE "C" < $4::text)))
    ORDER BY score DESC, posted_at DESC, id DESC
    LIMIT 51
  )
  SELECT ` + postColumns + `, ` + prefixedUserColumns("u") + `, page.score
  FROM page
  JOIN posts p ON p.id = page.id
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id
  ORDER BY page.score DESC, page.posted_at DESC, page.id DESC
 `,
		args: []any{viewerID, beforeScore, beforePostedAt, cursorID},
		scan: func(r pgx.Rows) error {
			for r.Next() {
				var row rankedFeedRow
				dest := append(postScanDest(&row.post.Post), userScanDest(&row.post.Author)...)
				dest = append(dest, &row.score)
				if err := r.Scan(dest...); err != nil {
					return err
				}
				if err := row.post.Author.setImageKey(); err != nil {
					return err
				}
				*rows = append(*rows, row)
			}
			return nil
		},
	}
}
