package isureal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"strconv"
	"testing"
)

// legacyComputeImageSignature は、毎回 hmac.New する元の実装(参考実装と同じ)。結果の一致確認に使う。
func legacyComputeImageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	for range imageSignatureIterations {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	return value
}

func TestComputeImageSignatureMatchesLegacy(t *testing.T) {
	keys := [][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{0xab}, 32), []byte("0123456789abcdef0123456789abcdef")}
	ids := []string{"img-0", "5c96f2db-5423-45a2-a4f7-e51dc672463c", ""}
	exps := []int64{0, 1793500000, 1793500061}
	for _, key := range keys {
		for _, id := range ids {
			for _, exp := range exps {
				got := computeImageSignature(key, id, exp)
				want := legacyComputeImageSignature(key, id, exp)
				if !bytes.Equal(got, want) {
					t.Fatalf("signature mismatch key=%x id=%q exp=%d", key, id, exp)
				}
			}
		}
	}
	// 戻り値が内部バッファを共有していないこと(続けて呼んでも前の結果が変わらない)
	a := computeImageSignature(keys[0], "a", 1)
	aCopy := append([]byte(nil), a...)
	computeImageSignature(keys[0], "b", 2)
	if !bytes.Equal(a, aCopy) {
		t.Fatal("returned signature was mutated by a later call")
	}
}

func BenchmarkImageSignatureLegacy(b *testing.B) {
	key := bytes.Repeat([]byte{1}, 32)
	for b.Loop() {
		legacyComputeImageSignature(key, "img", 1793500000)
	}
}

func TestImageSignatureCacheMatchesComputed(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	for i := range 3 {
		id := "img-" + strconv.Itoa(i)
		want := computeImageSignature(key, id, 1793500000)
		for range 2 {
			if got := imageSignature(key, id, 1793500000); !bytes.Equal(got, want) {
				t.Fatalf("signature mismatch for %s", id)
			}
		}
	}
	if bytes.Equal(imageSignature(key, "img-0", 1793500000), imageSignature(key, "img-0", 1793500060)) {
		t.Fatal("different exp must give different signature")
	}
	otherKey := bytes.Repeat([]byte{2}, 32)
	if bytes.Equal(imageSignature(key, "img-0", 1793500000), imageSignature(otherKey, "img-0", 1793500000)) {
		t.Fatal("different key must give different signature")
	}
}

func BenchmarkImageSignatureUncached(b *testing.B) {
	key := bytes.Repeat([]byte{1}, 32)
	for b.Loop() {
		computeImageSignature(key, "img", 1793500000)
	}
}

func BenchmarkImageSignatureCached(b *testing.B) {
	key := bytes.Repeat([]byte{1}, 32)
	imageSignature(key, "img", 1793500000)
	for b.Loop() {
		imageSignature(key, "img", 1793500000)
	}
}
