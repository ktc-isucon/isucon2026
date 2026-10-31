package isureal

import (
	"bytes"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 同じ署名を同時に求めたとき、全員が直接計算と同じ値を受け取り、計算は1回分の時間で終わること
func TestImageSignatureInflightShared(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	exp := time.Now().Add(48 * time.Hour).Unix()
	want := computeImageSignature(key, "inflight-image", exp)

	start := time.Now()
	computeImageSignature(key, "inflight-warmup", exp)
	single := time.Since(start)

	const callers = 32
	results := make([][]byte, callers)
	var wg sync.WaitGroup
	begin := time.Now()
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = imageSignature(key, "inflight-image", exp)
		}()
	}
	wg.Wait()
	elapsed := time.Since(begin)

	for i, got := range results {
		if !bytes.Equal(got, want) {
			t.Fatalf("caller %d got a different signature", i)
		}
	}
	// 共有しなければ 32回分(GOMAXPROCS 並列でも数倍)かかる。1回分の数倍に収まれば共有できている
	if elapsed > 8*single+20*time.Millisecond {
		t.Fatalf("concurrent callers took %v, single computation %v: signature was not shared", elapsed, single)
	}
}

// 同時に同じ署名の集合を求めるリクエスト(例: 同じ人のプロフィール画像)を模したベンチマーク。
// 毎回新しい exp で、メモに無い状態から8並列で同じ16件を求める。
func BenchmarkImageSignatureConcurrentSameTargets(b *testing.B) {
	key := bytes.Repeat([]byte{0x33}, 32)
	base := time.Now().Add(72 * time.Hour).Unix()
	for i := 0; b.Loop(); i++ {
		exp := base + int64(i)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range 16 {
					imageSignature(key, "bench-image-"+strconv.Itoa(j), exp)
				}
			}()
		}
		wg.Wait()
	}
}
