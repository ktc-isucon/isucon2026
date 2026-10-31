package isureal

import (
	"testing"
	"time"
)

func testImageAuthEntry(owner string) imageAuthEntry {
	return imageAuthEntry{OwnerID: owner, SecretKeyHash: "hash-" + owner, CreatedAt: time.Unix(1, 0)}
}

func TestImageAuthCacheDisabled(t *testing.T) {
	c := newImageAuthCache()
	_, _, epoch := c.begin("img1")
	c.put(epoch, "img1", testImageAuthEntry("u1"))
	if _, hit, _ := c.begin("img1"); hit {
		t.Fatal("LISTEN が繋がっていない間はキャッシュを使わない")
	}
}

func TestImageAuthCachePutAndHit(t *testing.T) {
	c := newImageAuthCache()
	c.setEnabled(true)
	_, hit, epoch := c.begin("img1")
	if hit {
		t.Fatal("空のキャッシュに当たった")
	}
	c.put(epoch, "img1", testImageAuthEntry("u1"))
	got, hit, _ := c.begin("img1")
	if !hit || got.OwnerID != "u1" || got.SecretKeyHash != "hash-u1" {
		t.Fatalf("保存した値が引けない: hit=%v got=%+v", hit, got)
	}
}

func TestImageAuthCacheSkipsStalePut(t *testing.T) {
	c := newImageAuthCache()
	c.setEnabled(true)
	_, _, epoch := c.begin("img1")
	// DB を読んでいる間に、持ち主の退会などで無効化が来た
	c.invalidateOwner("u1")
	c.put(epoch, "img1", testImageAuthEntry("u1"))
	if _, hit, _ := c.begin("img1"); hit {
		t.Fatal("読んでいる間に無効化があったのに保存された")
	}
}

func TestImageAuthCacheInvalidateOwner(t *testing.T) {
	c := newImageAuthCache()
	c.setEnabled(true)
	for _, id := range []string{"a1", "a2"} {
		_, _, epoch := c.begin(id)
		c.put(epoch, id, testImageAuthEntry("u1"))
	}
	_, _, epoch := c.begin("b1")
	c.put(epoch, "b1", testImageAuthEntry("u2"))

	c.handleSessionNotification("u1")
	for _, id := range []string{"a1", "a2"} {
		if _, hit, _ := c.begin(id); hit {
			t.Fatalf("退会した持ち主の画像 %s が残っている", id)
		}
	}
	if _, hit, _ := c.begin("b1"); !hit {
		t.Fatal("別の持ち主の画像まで消えた")
	}

	c.handleSessionNotification(sessionInvalidateAll)
	if _, hit, _ := c.begin("b1"); hit {
		t.Fatal("全消去の通知で消えていない")
	}
}

func TestImageAuthCacheReconnectClears(t *testing.T) {
	c := newImageAuthCache()
	c.setEnabled(true)
	_, _, epoch := c.begin("img1")
	c.put(epoch, "img1", testImageAuthEntry("u1"))
	// LISTEN が切れて繋ぎ直した(切れている間の通知を取りこぼしたかもしれない)
	c.setEnabled(false)
	c.setEnabled(true)
	if _, hit, _ := c.begin("img1"); hit {
		t.Fatal("繋ぎ直したのに古い中身が残っている")
	}
}
