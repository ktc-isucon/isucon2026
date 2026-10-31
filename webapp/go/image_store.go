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
	imageStoreURL = envOrDefault("ISUREAL_IMAGE_STORE_URL", "http://192.168.4.10")
	// selfImageRegion はこのサーバーのリージョン。アップロードされた画像の持ち主(origin_region)として記録する
	selfImageRegion = envOrDefault("ISUREAL_REGION", "asia")
	// imageOriginHosts はリージョンごとの、ベンチ中に追加された画像を持っているサーバー(プライベートIP)
	imageOriginHosts = map[string]string{
		"asia":    envOrDefault("ISUREAL_IMAGE_ORIGIN_ASIA", "192.168.1.10"),
		"europe":  envOrDefault("ISUREAL_IMAGE_ORIGIN_EUROPE", "192.168.2.10"),
		"america": envOrDefault("ISUREAL_IMAGE_ORIGIN_AMERICA", "192.168.3.10"),
	}
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

// storeUploadedImage はアップロードされた画像を保存し、画像を持っているリージョン(origin_region)を返す。
// 手元に書けたときは、isu3へのPUT(europe/americaでは帯域制限で数百ms)を待たずに返し、PUTは裏で続ける。
// 他サーバーは origin のサーバー(落ちていればisu3)から取得する。手元に書けなかったときは、
// 従来どおりisu3へのPUTの完了を待ち、originは空(=isu3から取得)にする。
func storeUploadedImage(id string, data []byte) (string, error) {
	put := putRuntimeImageToStore(id, data)
	if err := writeLocalRuntimeImage(id, data); err != nil {
		if err := <-put; err != nil {
			return "", err
		}
		return "", nil
	}
	go func() {
		if err := <-put; err != nil {
			fmt.Fprintf(os.Stderr, "failed to put image %s to store: %v\n", id, err)
		}
	}()
	return selfImageRegion, nil
}

func putRuntimeImageToStore(id string, data []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- storeRequest(http.MethodPut, "/images/"+imageRelPath(imageKindRuntime, id), data)
	}()
	return done
}

// nullableOrigin は origin_region をDBに書くための値に変換する(空ならNULL)。
func nullableOrigin(origin string) any {
	if origin == "" {
		return nil
	}
	return origin
}

// runtimeImageAccelPath は、ベンチ中に追加された画像をnginxに返させるパスを返す。
// 別のリージョンのサーバーが持っている画像は、そのサーバーから取得して手元に保存させる。
func runtimeImageAccelPath(origin, id string) string {
	if _, ok := imageOriginHosts[origin]; ok && origin != selfImageRegion {
		return "/_img/runtime-from/" + origin + "/" + id[:2] + "/" + id + ".jpg"
	}
	return "/_img/" + imageRelPath(imageKindRuntime, id)
}

// writeLocalRuntimeImage は手元のruntimeキャッシュに画像を書く。
// 画像の中身はIDごとに不変で、閲覧可否は配信のたびにDBで確認するため、キャッシュが古くなることはない。
func writeLocalRuntimeImage(id string, data []byte) error {
	path := localImageCacheRoot + "/_img/" + imageRelPath(imageKindRuntime, id)
	dir := path[:strings.LastIndex(path, "/")]
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return err
	}
	// nginx(www-data)もこのディレクトリにproxy_storeで書くため、グループにも書き込みを許す
	_ = os.Chmod(dir, 0o2775)
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
	data, err := fetchImage(ctx, imageStoreURL+"/_img/"+rel)
	if err == nil || kind != imageKindRuntime {
		return data, err
	}
	// isu3へのPUTがまだ終わっていない画像は、アップロードしたサーバーから取得する
	for region, host := range imageOriginHosts {
		if region == selfImageRegion {
			continue
		}
		if data, originErr := fetchImage(ctx, "http://"+host+"/_img_origin/"+rel); originErr == nil {
			return data, nil
		}
	}
	return nil, err
}

func fetchImage(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := imageStoreClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
