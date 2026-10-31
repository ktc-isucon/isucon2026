package isureal

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"
	"time"
)

func resetImageSignaturePrecomputeForTest() {
	p := &imageSignaturePrecompute
	p.Lock()
	p.exp = 0
	p.targets = nil
	p.done = nil
	p.Unlock()
}

func randomSigningKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestNextImageURLExpSwitch(t *testing.T) {
	// 枠の切り替わりの直前までは今の exp、直後からは次の exp になること
	cur := imageURLExp(time.Now())
	sw := nextImageURLExpSwitch(cur)
	if got := imageURLExp(sw.Add(-time.Millisecond)); got != cur {
		t.Fatalf("before switch: got %d, want %d", got, cur)
	}
	if got := imageURLExp(sw.Add(time.Second)); got != cur+imageURLExpGranularity {
		t.Fatalf("after switch: got %d, want %d", got, cur+imageURLExpGranularity)
	}
}

func TestPrecomputeNextImageSignatures(t *testing.T) {
	resetImageSignaturePrecomputeForTest()
	defer resetImageSignaturePrecomputeForTest()

	// 実時間の先回り計算と重ならないよう、遠い未来の枠を使う
	cur := imageURLExp(time.Date(2031, 1, 1, 0, 0, 30, 0, time.UTC))
	sw := nextImageURLExpSwitch(cur)
	nearSwitch := sw.Add(-10 * time.Second)
	if imageURLExp(nearSwitch) != cur {
		t.Fatalf("test setup: exp mismatch")
	}
	clockAt := func(tm time.Time) func() time.Time { return func() time.Time { return tm } }
	idle := func() bool { return true }
	busy := func() bool { return false }

	key := randomSigningKey(t)
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("img-%d", i)
		imageSignatureForURL(key, ids[i], cur)
	}

	// 切り替わりまで余裕があるうちは計算しない
	if n := precomputeNextImageSignatures(clockAt(sw.Add(-30*time.Second)), idle); n != 0 {
		t.Fatalf("too early: computed %d", n)
	}
	// CPU が空いていなければ計算しない
	if n := precomputeNextImageSignatures(clockAt(nearSwitch), busy); n != 0 {
		t.Fatalf("busy: computed %d", n)
	}

	before := imageSignatureComputeCount.Load()
	if n := precomputeNextImageSignatures(clockAt(nearSwitch), idle); n != len(ids) {
		t.Fatalf("near switch: computed %d, want %d", n, len(ids))
	}
	if got := imageSignatureComputeCount.Load() - before; got != int64(len(ids)) {
		t.Fatalf("HMAC computed %d times, want %d", got, len(ids))
	}
	// 2回目は計算済みなので何もしない
	if n := precomputeNextImageSignatures(clockAt(nearSwitch), idle); n != 0 {
		t.Fatalf("second run: computed %d", n)
	}

	// 次の枠になってから応答で求めた署名は、直接計算と一致し、HMAC を計算し直さない
	next := cur + imageURLExpGranularity
	before = imageSignatureComputeCount.Load()
	for _, id := range ids {
		got := imageSignatureForURL(key, id, next)
		if want := computeImageSignature(key, id, next); !bytes.Equal(got, want) {
			t.Fatalf("signature mismatch for %s", id)
		}
	}
	if got := imageSignatureComputeCount.Load() - before; got != 0 {
		t.Fatalf("next window recomputed %d signatures, want 0", got)
	}
}
