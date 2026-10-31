package isureal

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"golang.org/x/sync/errgroup"
)

type exportPostResponse struct {
	createPostResponse
	ImagePath string `json:"imagePath"`
}

func exportPostsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		author, err := getUser(ctx, h.ReadDB(c), userID)
		if err != nil {
			return err
		}
		var posts []struct {
			Post
			ImageCreatedAt time.Time `db:"image_created_at"`
		}
		if err := h.ReadDB(c).SelectContext(ctx, &posts, `
   SELECT `+postColumns+`, i.created_at AS image_created_at
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   JOIN image_objects i ON i.id = p.image_id
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
   ORDER BY p.posted_at, p.id
  `, userID); err != nil {
			return err
		}
		// 画像本体はisu3にあるので並行して取得する
		images := make([][]byte, len(posts))
		var eg errgroup.Group
		eg.SetLimit(16)
		for i, post := range posts {
			eg.Go(func() error {
				kind, err := imageKind(ctx, h.ReadDB(c), post.ImageCreatedAt)
				if err != nil {
					return err
				}
				data, err := readImage(ctx, kind, post.ImageID)
				if err != nil {
					return err
				}
				images[i] = data
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return err
		}

		var body bytes.Buffer
		writer := zip.NewWriter(&body)
		defer writer.Close()
		items := make([]exportPostResponse, 0, len(posts))
		for i, post := range posts {
			imagePath := fmt.Sprintf("images/%06d.jpg", len(items)+1)
			image, err := writer.CreateHeader(&zip.FileHeader{Name: imagePath, Method: zip.Store})
			if err != nil {
				return err
			}
			if _, err := image.Write(images[i]); err != nil {
				return err
			}
			items = append(items, exportPostResponse{createPostResponse: newPostResponse(post.Post, author), ImagePath: imagePath})
		}
		manifest, err := writer.Create("posts.json")
		if err != nil {
			return err
		}
		if err := json.NewEncoder(manifest).Encode(items); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="isureal-posts.zip"`)
		c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
		return c.Blob(http.StatusOK, "application/zip", body.Bytes())
	}
}
