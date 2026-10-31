package isureal

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

func testSigningUser(i int) User {
	key := sha256.Sum256([]byte(fmt.Sprintf("user-%d", i)))
	return User{ImageKey: key[:]}
}

func TestPrewarmMatchesDirectSignature(t *testing.T) {
	var targets imageSignatureTargets
	users := make([]User, 20)
	for i := range users {
		users[i] = testSigningUser(i)
		targets.add(users[i], fmt.Sprintf("prewarm-test-%d", i))
		targets.add(users[i], fmt.Sprintf("prewarm-test-%d", i)) // 重複は1件にまとめる
	}
	if len(targets.targets) != len(users) {
		t.Fatalf("targets = %d, want %d", len(targets.targets), len(users))
	}
	targets.prewarm()

	exp := time.Now().Add(imageURLLifetime).Unix()
	if r := exp % imageURLExpGranularity; r != 0 {
		exp += imageURLExpGranularity - r
	}
	for i, user := range users {
		imageID := fmt.Sprintf("prewarm-test-%d", i)
		got := imageSignature(user.ImageKey, imageID, exp)
		want := computeImageSignature(user.ImageKey, imageID, exp)
		if !bytes.Equal(got, want) {
			t.Fatalf("signature mismatch for %s", imageID)
		}
	}
}

// 毎回別の画像IDを使い、メモに当たらない状態(メモリーズと同じ)で比べる
func benchmarkSignatures(b *testing.B, parallel bool) {
	user := testSigningUser(0)
	exp := time.Now().Add(imageURLLifetime).Unix()
	n := 0
	for b.Loop() {
		var targets imageSignatureTargets
		for range 50 {
			n++
			targets.add(user, fmt.Sprintf("bench-%t-%d", parallel, n))
		}
		if parallel {
			targets.prewarm()
			continue
		}
		for _, target := range targets.targets {
			imageSignature(target.key, target.imageID, exp)
		}
	}
}

func BenchmarkSignaturesSequential(b *testing.B) { benchmarkSignatures(b, false) }
func BenchmarkSignaturesPrewarm(b *testing.B)    { benchmarkSignatures(b, true) }
