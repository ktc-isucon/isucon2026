package isureal

import (
	"bytes"
	"strconv"
	"testing"
)

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
