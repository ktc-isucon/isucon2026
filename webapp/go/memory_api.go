package isureal

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getMemoriesRequest struct {
	Month          string     `query:"month"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type memoryPostResponse struct {
	createPostResponse
	RealMojis []replaceRealMojiResponse `json:"realmojis"`
	Comments  []createCommentResponse   `json:"comments"`
}

type getMemoriesResponse struct {
	Items   []memoryPostResponse `json:"items"`
	HasMore bool                 `json:"hasMore"`
}

type getMemoriesByDateRequest struct {
	Date           string     `param:"date"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type getMemoriesByDateResponse getMemoriesResponse

func getMemoriesHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse("2006-01", input.Month)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.ReadDB(c), GetSession(c).UserID, start, start.AddDate(0, 1, 0), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, response)
	}
}

func getMemoriesByDateHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesByDateRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse(time.DateOnly, input.Date)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.ReadDB(c), GetSession(c).UserID, start, start.AddDate(0, 0, 1), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMemoriesByDateResponse(response))
	}
}

// memoryPostsFrom は、メモリーズの対象の投稿を選ぶ FROM〜ORDER BY(LIMIT なし)。
// 投稿・RealMoji・コメントの3文で同じ条件を使い、同じスナップショットで同じ投稿の集合を得る。
const memoryPostsFrom = `
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL
    AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
    AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
  ORDER BY p.posted_at DESC, p.id DESC`

func listMemories(ctx context.Context, db *sqlx.DB, userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	// 投稿(最大51件)と、表示する50件分の RealMoji・コメントを、同じスナップショットで1回の往復で取得する
	args := []any{userID, start, end, beforeAt, beforeID}
	var posts []PostWithAuthor
	var reactions []RealMojiWithReactor
	var comments []CommentWithAuthor
	if err := runPipelinedSnapshot(ctx, db,
		pipelinedStmt{
			sql:  `SELECT ` + postColumns + `, ` + prefixedUserColumns("u") + memoryPostsFrom + ` LIMIT 51`,
			args: args,
			scan: func(rows pgx.Rows) error {
				for rows.Next() {
					var post PostWithAuthor
					if err := rows.Scan(append(postScanDest(&post.Post), userScanDest(&post.Author)...)...); err != nil {
						return err
					}
					posts = append(posts, post)
				}
				return nil
			},
		},
		pipelinedStmt{
			sql: `
  SELECT r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at, ` + prefixedUserColumns("ru") + `
  FROM realmojis r JOIN users ru ON ru.id = r.reactor_id AND ru.status = 'active'
  WHERE r.replaced_at IS NULL AND r.deleted_at IS NULL
    AND r.post_id IN (SELECT p.id ` + memoryPostsFrom + ` LIMIT 50)
  ORDER BY r.created_at DESC, r.id DESC`,
			args: args,
			scan: func(rows pgx.Rows) error {
				for rows.Next() {
					var reaction RealMojiWithReactor
					r := &reaction.RealMoji
					dest := []any{&r.ID, &r.PostID, &r.ReactorID, &r.ImageID, &r.ReactionType, &r.CreatedAt, &r.ReplacedAt, &r.DeletedAt}
					if err := rows.Scan(append(dest, userScanDest(&reaction.Reactor)...)...); err != nil {
						return err
					}
					reactions = append(reactions, reaction)
				}
				return nil
			},
		},
		pipelinedStmt{
			sql: `
  SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, ` + prefixedUserColumns("cu") + `
  FROM comments c JOIN users cu ON cu.id = c.author_id AND cu.status = 'active'
  WHERE c.deleted_at IS NULL
    AND c.post_id IN (SELECT p.id ` + memoryPostsFrom + ` LIMIT 50)
  ORDER BY c.created_at, c.id`,
			args: args,
			scan: func(rows pgx.Rows) error {
				for rows.Next() {
					var comment CommentWithAuthor
					cm := &comment.Comment
					dest := []any{&cm.ID, &cm.PostID, &cm.AuthorID, &cm.Body, &cm.CreatedAt, &cm.DeletedAt}
					if err := rows.Scan(append(dest, userScanDest(&comment.Author)...)...); err != nil {
						return err
					}
					comments = append(comments, comment)
				}
				return nil
			},
		},
	); err != nil {
		return response, err
	}
	for i := range posts {
		if err := posts[i].Author.setImageKey(); err != nil {
			return response, err
		}
	}
	for i := range reactions {
		if err := reactions[i].Reactor.setImageKey(); err != nil {
			return response, err
		}
	}
	for i := range comments {
		if err := comments[i].Author.setImageKey(); err != nil {
			return response, err
		}
	}
	if len(posts) > 50 {
		prewarmPostSignatures(posts[:50])
	} else {
		prewarmPostSignatures(posts)
	}
	items := make([]memoryPostResponse, 0, len(posts))
	for _, post := range posts {
		items = append(items, memoryPostResponse{
			createPostResponse: newPostResponse(post.Post, post.Author),
			RealMojis:          make([]replaceRealMojiResponse, 0),
			Comments:           make([]createCommentResponse, 0),
		})
	}
	response.HasMore = len(items) > 50
	if response.HasMore {
		items = items[:50]
	}
	attachMemoryInteractions(items, reactions, comments)
	response.Items = items
	return response, nil
}

// attachMemoryInteractions は、取得済みの RealMoji とコメントを投稿ごとに最大50件ずつ付ける。
func attachMemoryInteractions(items []memoryPostResponse, reactions []RealMojiWithReactor, comments []CommentWithAuthor) {
	if len(items) == 0 {
		return
	}
	itemByPostID := make(map[string]*memoryPostResponse, len(items))
	for i := range items {
		itemByPostID[items[i].ID] = &items[i]
	}

	var targets imageSignatureTargets
	perPost := make(map[string]int, len(items))
	for _, reaction := range reactions {
		if perPost[reaction.PostID] < 50 {
			perPost[reaction.PostID]++
			targets.add(reaction.Reactor, reaction.ImageID)
			targets.addProfile(reaction.Reactor)
		}
	}
	targets.prewarm()
	for _, reaction := range reactions {
		item := itemByPostID[reaction.PostID]
		if item != nil && len(item.RealMojis) < 50 {
			item.RealMojis = append(item.RealMojis, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
	}

	targets = imageSignatureTargets{}
	clear(perPost)
	for _, comment := range comments {
		if perPost[comment.PostID] < 50 {
			perPost[comment.PostID]++
			targets.addProfile(comment.Author)
		}
	}
	targets.prewarm()
	for _, comment := range comments {
		item := itemByPostID[comment.PostID]
		if item != nil && len(item.Comments) < 50 {
			item.Comments = append(item.Comments, newCommentResponse(comment.Comment, comment.Author))
		}
	}
}

// imageSignatureTargets は、応答の組み立てで signedImageURL が計算する署名の (鍵, 画像ID) を重複なく集める。
// prewarm で並列に計算してメモ(imageSignature)に入れておくと、そのあとの組み立てはメモに当たる。
type imageSignatureTargets struct {
	seen    map[string]struct{}
	targets []imageSignatureTarget
}

type imageSignatureTarget struct {
	key     []byte
	imageID string
}

func (t *imageSignatureTargets) add(owner User, imageID string) {
	if t.seen == nil {
		t.seen = make(map[string]struct{})
	}
	k := string(owner.ImageKey) + "\x00" + imageID
	if _, ok := t.seen[k]; ok {
		return
	}
	t.seen[k] = struct{}{}
	t.targets = append(t.targets, imageSignatureTarget{key: owner.ImageKey, imageID: imageID})
}

func (t *imageSignatureTargets) addProfile(user User) {
	if user.ProfileImageID != nil {
		t.add(user, *user.ProfileImageID)
	}
}

// prewarm は signedImageURL と同じ exp で署名を GOMAXPROCS 並列で計算する。
// 途中で exp の切り替わり(60秒単位)をまたぐと組み立て時にメモが外れるが、その分を直接計算するだけで結果は変わらない。
func (t *imageSignatureTargets) prewarm() {
	if len(t.targets) < 2 {
		return
	}
	exp := imageURLExp(time.Now())
	workers := min(runtime.GOMAXPROCS(0), len(t.targets))
	ch := make(chan imageSignatureTarget)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range ch {
				imageSignature(target.key, target.imageID, exp)
			}
		}()
	}
	for _, target := range t.targets {
		ch <- target
	}
	close(ch)
	wg.Wait()
}

// prewarmPostSignatures は投稿の画像と作者のプロフィール画像の署名を温める。
func prewarmPostSignatures(posts []PostWithAuthor) {
	var targets imageSignatureTargets
	for _, post := range posts {
		targets.add(post.Author, post.ImageID)
		targets.addProfile(post.Author)
	}
	targets.prewarm()
}
