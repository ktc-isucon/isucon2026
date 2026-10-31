package isureal

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"time"

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
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 1, 0), input.BeforePostedAt, input.BeforeID)
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
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 0, 1), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMemoriesByDateResponse(response))
	}
}

func listMemories(ctx context.Context, db sqlx.QueryerContext, userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	posts, err := selectPostsWithAuthor(ctx, db, `
  AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
  AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
  ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
 `, userID, start, end, beforeAt, beforeID)
	if err != nil {
		return response, err
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
	if err := loadMemoryInteractions(ctx, db, items); err != nil {
		return response, err
	}
	response.Items = items
	return response, nil
}

func loadMemoryInteractions(ctx context.Context, db sqlx.QueryerContext, items []memoryPostResponse) error {
	if len(items) == 0 {
		return nil
	}
	postIDs := make([]string, len(items))
	itemByPostID := make(map[string]*memoryPostResponse, len(items))
	for i := range items {
		postIDs[i] = items[i].ID
		itemByPostID[items[i].ID] = &items[i]
	}

	query, args, err := sqlx.In(`AND r.post_id IN (?) ORDER BY r.created_at DESC, r.id DESC`, postIDs)
	if err != nil {
		return err
	}
	reactions, err := selectCurrentRealMojis(ctx, db, sqlx.Rebind(sqlx.DOLLAR, query), args...)
	if err != nil {
		return err
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
		if len(item.RealMojis) < 50 {
			item.RealMojis = append(item.RealMojis, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
	}

	query, args, err = sqlx.In(`AND c.post_id IN (?) ORDER BY c.created_at, c.id`, postIDs)
	if err != nil {
		return err
	}
	comments, err := selectCommentsWithAuthor(ctx, db, sqlx.Rebind(sqlx.DOLLAR, query), args...)
	if err != nil {
		return err
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
		if len(item.Comments) < 50 {
			item.Comments = append(item.Comments, newCommentResponse(comment.Comment, comment.Author))
		}
	}
	return nil
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
	exp := time.Now().Add(imageURLLifetime).Unix()
	if r := exp % imageURLExpGranularity; r != 0 {
		exp += imageURLExpGranularity - r
	}
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
