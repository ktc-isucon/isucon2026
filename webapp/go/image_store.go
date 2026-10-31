package isureal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// 画像本体はDBではなくisu3のファイル(nginx WebDAV)に置く。
// 初期データの画像は initial/、ベンチ中に増えた画像は runtime/ に置き、初期化では runtime/ だけを消す。
const (
	imageKindInitial = "initial"
	imageKindRuntime = "runtime"
	// isu0〜2のnginxが画像をキャッシュする場所(proxy_storeの保存先)
	localImageCacheRoot = "/home/isucon/image-cache"
	// ベンチ中に追加された画像の手元キャッシュ。nginx(www-data)とアプリ(isucon)の両方が書く
	localRuntimeImageDir = localImageCacheRoot + "/_img/" + imageKindRuntime
	// imageCacheResetChannel は初期化時に全サーバーの手元のruntimeキャッシュを消すためのLISTEN/NOTIFYのチャネル名
	imageCacheResetChannel = "image_cache_reset"
)

var (
	imageStoreURL     = envOrDefault("ISUREAL_IMAGE_STORE_URL", "http://192.168.4.10")
	imageStoreClient  = &http.Client{Timeout: 10 * time.Second}
	imageIDPattern    = regexp.MustCompile(`^[0-9a-f-]{36}$`)
	initialBoundary   time.Time
	initialBoundaryMu sync.Mutex
)

func validImageID(id string) bool {
	return imageIDPattern.MatchString(id)
}

// imageRelPath は initial/ab/<id>.jpg の形式のパスを返す。
func imageRelPath(kind, id string) string {
	return kind + "/" + id[:2] + "/" + id + ".jpg"
}

func getInitialBoundary(ctx context.Context, db sqlx.QueryerContext) (time.Time, error) {
	initialBoundaryMu.Lock()
	defer initialBoundaryMu.Unlock()
	if !initialBoundary.IsZero() {
		return initialBoundary, nil
	}
	var boundary time.Time
	if err := sqlx.GetContext(ctx, db, &boundary, `SELECT created_at FROM initial_data_boundary`); err != nil {
		return time.Time{}, err
	}
	initialBoundary = boundary
	return boundary, nil
}

func imageKind(ctx context.Context, db sqlx.QueryerContext, createdAt time.Time) (string, error) {
	boundary, err := getInitialBoundary(ctx, db)
	if err != nil {
		return "", err
	}
	if createdAt.After(boundary) {
		return imageKindRuntime, nil
	}
	return imageKindInitial, nil
}

// putRuntimeImageAsync はベンチ中に追加された画像をisu3へ非同期でPUTし、結果をチャネルで返す。
// DBのトランザクションとisu3への往復を並行させるために使う。
func putRuntimeImageAsync(id string, data []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- storeRequest(http.MethodPut, "/images/"+imageRelPath(imageKindRuntime, id), data)
	}()
	// アップロードしたサーバーでは手元にも置いておき、isu3からの取得を省く(失敗してもnginxがisu3から取得する)
	go writeLocalRuntimeImage(id, data)
	return done
}

// writeLocalRuntimeImage は手元のruntimeキャッシュに画像を書く(ベストエフォート)。
// 画像の中身はIDごとに不変で、閲覧可否は配信のたびにDBで確認するため、キャッシュが古くなることはない。
func writeLocalRuntimeImage(id string, data []byte) {
	path := localImageCacheRoot + "/_img/" + imageRelPath(imageKindRuntime, id)
	dir := path[:strings.LastIndex(path, "/")]
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return
	}
	// nginx(www-data)もこのディレクトリにproxy_storeで書くため、グループにも書き込みを許す
	_ = os.Chmod(dir, 0o2775)
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// resetLocalRuntimeImages は手元のruntimeキャッシュを消す。初期化を止めないよう、
// ディレクトリを退避してからバックグラウンドで削除する。
func resetLocalRuntimeImages() {
	trash := localRuntimeImageDir + ".trash-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Rename(localRuntimeImageDir, trash); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "failed to reset local runtime images: %v\n", err)
		}
		return
	}
	go func() {
		if err := os.RemoveAll(trash); err != nil {
			fmt.Fprintf(os.Stderr, "failed to remove local runtime images: %v\n", err)
		}
	}()
}

// notifyImageCacheReset は他サーバーにも手元のruntimeキャッシュを消すよう知らせる。
func notifyImageCacheReset(ctx context.Context, db sqlx.ExecerContext) error {
	_, err := db.ExecContext(ctx, `SELECT pg_notify('`+imageCacheResetChannel+`', '')`)
	return err
}

// deleteRuntimeImage はisu3上の画像を消す(ベストエフォート)。初期データの画像は初期化で戻せないため消さない。
func deleteRuntimeImage(kind, id string) {
	if kind != imageKindRuntime {
		return
	}
	go func() {
		if err := storeRequest(http.MethodDelete, "/images/"+imageRelPath(kind, id), nil); err != nil {
			fmt.Fprintf(os.Stderr, "failed to delete image %s: %v\n", id, err)
		}
	}()
}

// resetRuntimeImages は初期化時にベンチ中に追加された画像をまとめて消す。
func resetRuntimeImages() error {
	return storeRequest(http.MethodDelete, "/images/"+imageKindRuntime+"/", nil)
}

func storeRequest(method, path string, body []byte) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, imageStoreURL+path, reader)
	if err != nil {
		return err
	}
	resp, err := imageStoreClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound && method == http.MethodDelete {
		return nil
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("image store %s %s: status %d", method, path, resp.StatusCode)
	}
	return nil
}

// readImage は画像本体を返す(エクスポート用)。手元のnginxキャッシュ→isu3の順に探す。
func readImage(ctx context.Context, kind, id string) ([]byte, error) {
	rel := imageRelPath(kind, id)
	if data, err := os.ReadFile(localImageCacheRoot + "/_img/" + rel); err == nil {
		return data, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageStoreURL+"/_img/"+rel, nil)
	if err != nil {
		return nil, err
	}
	resp, err := imageStoreClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image store GET %s: status %d", rel, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
