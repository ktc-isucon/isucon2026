package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1000

// signedImageURLのexpをこの秒数単位で切り上げ、同じ画像の署名を使い回せるようにする
const imageURLExpGranularity = 60

// 署名(HMAC 1000回)のメモ。1世代の件数が上限に達したら新しい世代に切り替え、古い世代を捨てる
const imageSignatureCacheGenerationSize = 100000

var imageSignatureCache = struct {
	sync.Mutex
	current  map[string][]byte
	previous map[string][]byte
	// 計算中の署名。同じ署名を同時に求めたリクエストは、先に始めた計算の完了を待って結果を共有する
	inflight map[string]*imageSignatureCall
}{current: make(map[string][]byte), inflight: make(map[string]*imageSignatureCall)}

type imageSignatureCall struct {
	done chan struct{}
	sig  []byte
}

func imageSigningKey(secretKeyHash string) ([]byte, error) {
	key, err := hex.DecodeString(secretKeyHash)
	if err != nil || len(key) != sha256.Size {
		return nil, fmt.Errorf("invalid image signing key")
	}
	return key, nil
}

// imageSignatureは計算結果をメモし、生成(signedImageURL)と検証(getImageHandler)で共有する。
// 返り値は共有されるため、呼び出し側で書き換えないこと。
func imageSignature(key []byte, imageID string, exp int64) []byte {
	cacheKey := string(key) + "\x00" + strconv.FormatInt(exp, 10) + ":" + imageID

	imageSignatureCache.Lock()
	if sig, ok := imageSignatureCache.current[cacheKey]; ok {
		imageSignatureCache.Unlock()
		return sig
	}
	if sig, ok := imageSignatureCache.previous[cacheKey]; ok {
		imageSignatureCache.current[cacheKey] = sig
		imageSignatureCache.Unlock()
		return sig
	}
	if call, ok := imageSignatureCache.inflight[cacheKey]; ok {
		imageSignatureCache.Unlock()
		<-call.done
		return call.sig
	}
	call := &imageSignatureCall{done: make(chan struct{})}
	imageSignatureCache.inflight[cacheKey] = call
	imageSignatureCache.Unlock()

	call.sig = computeImageSignature(key, imageID, exp)

	imageSignatureCache.Lock()
	if len(imageSignatureCache.current) >= imageSignatureCacheGenerationSize {
		imageSignatureCache.previous = imageSignatureCache.current
		imageSignatureCache.current = make(map[string][]byte)
	}
	imageSignatureCache.current[cacheKey] = call.sig
	delete(imageSignatureCache.inflight, cacheKey)
	imageSignatureCache.Unlock()
	close(call.done)
	return call.sig
}

// imageURLExp は署名URLの有効期限を返す。24時間以上を保ったまま imageURLExpGranularity 秒単位に切り上げる
// (最大で imageURLExpGranularity 秒長くなる)。signedImageURL と署名の事前計算で同じ値を使う。
func imageURLExp(now time.Time) int64 {
	exp := now.Add(imageURLLifetime).Unix()
	if r := exp % imageURLExpGranularity; r != 0 {
		exp += imageURLExpGranularity - r
	}
	return exp
}

func computeImageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	// hmac.New をループの外で1回だけ作り、Reset で使い回す。
	// crypto/hmac は Reset 時に鍵の前処理(ipad/opad)済みの状態を復元するので、毎回の鍵処理が不要になる。
	// 入力と出力を別の固定バッファで交互に使い、アロケーションも避ける。
	mac := hmac.New(sha256.New, key)
	var bufA, bufB [sha256.Size]byte
	in, out := bufA[:0], bufB[:0]
	mac.Write(value)
	in = mac.Sum(in)
	for range imageSignatureIterations - 1 {
		mac.Reset()
		mac.Write(in)
		out = mac.Sum(out[:0])
		in, out = out, in
	}
	return append([]byte(nil), in...)
}

func signedImageURL(user User, imageID string) string {
	exp := imageURLExp(time.Now())
	query := url.Values{
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {base64.RawURLEncoding.EncodeToString(imageSignature(user.ImageKey, imageID, exp))},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}
