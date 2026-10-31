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
}{current: make(map[string][]byte)}

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
	imageSignatureCache.Unlock()

	sig := computeImageSignature(key, imageID, exp)

	imageSignatureCache.Lock()
	if len(imageSignatureCache.current) >= imageSignatureCacheGenerationSize {
		imageSignatureCache.previous = imageSignatureCache.current
		imageSignatureCache.current = make(map[string][]byte)
	}
	imageSignatureCache.current[cacheKey] = sig
	imageSignatureCache.Unlock()
	return sig
}

func computeImageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	for range imageSignatureIterations {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	return value
}

func signedImageURL(user User, imageID string) string {
	// 有効期限は24時間以上を保ったまま切り上げる(最大で imageURLExpGranularity 秒長くなる)
	exp := time.Now().Add(imageURLLifetime).Unix()
	if r := exp % imageURLExpGranularity; r != 0 {
		exp += imageURLExpGranularity - r
	}
	query := url.Values{
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {base64.RawURLEncoding.EncodeToString(imageSignature(user.ImageKey, imageID, exp))},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}
