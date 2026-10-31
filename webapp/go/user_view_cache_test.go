package isureal

import "testing"

func newTestUserViewCache() *userViewCache {
	c := &userViewCache{byUser: map[string]*userViewEntry{}}
	c.setEnabled(true)
	return c
}

func TestUserViewCacheDisabledDoesNotStore(t *testing.T) {
	c := &userViewCache{byUser: map[string]*userViewEntry{}}
	_, epoch := c.begin("u1")
	c.putMe(epoch, "u1", User{ID: "u1"})
	if got, _ := c.begin("u1"); got.me != nil {
		t.Fatal("disabled cache must not store values")
	}
}

func TestUserViewCachePutAndHit(t *testing.T) {
	c := newTestUserViewCache()
	_, epoch := c.begin("u1")
	c.putMe(epoch, "u1", User{ID: "u1", DisplayName: "a"})
	c.putWindow(epoch, "u1", cachedPostingWindowState{found: true, active: 1})
	c.putMyPost(epoch, "u1", cachedMyPost{found: false})
	got, _ := c.begin("u1")
	if got.me == nil || got.me.DisplayName != "a" {
		t.Fatalf("me not cached: %+v", got.me)
	}
	if got.window == nil || !got.window.found || got.window.active != 1 {
		t.Fatalf("window not cached: %+v", got.window)
	}
	if got.myPost == nil || got.myPost.found {
		t.Fatalf("negative my post not cached: %+v", got.myPost)
	}
}

func TestUserViewCacheInvalidateDuringReadIsNotStored(t *testing.T) {
	c := newTestUserViewCache()
	_, epoch := c.begin("u1")
	// DBを読んでいる間に、別のユーザーの無効化が来ても保存しない(安全側)
	c.invalidateUser("u2")
	c.putMe(epoch, "u1", User{ID: "u1"})
	if got, _ := c.begin("u1"); got.me != nil {
		t.Fatal("value read before invalidation must not be stored")
	}
}

func TestUserViewCacheInvalidateUserAndNotifications(t *testing.T) {
	c := newTestUserViewCache()
	_, epoch := c.begin("u1")
	c.putMe(epoch, "u1", User{ID: "u1"})
	c.putMe(epoch, "u2", User{ID: "u2"})

	c.handleSessionNotification("u1")
	if got, _ := c.begin("u1"); got.me != nil {
		t.Fatal("u1 must be invalidated by its notification")
	}
	if got, _ := c.begin("u2"); got.me == nil {
		t.Fatal("u2 must remain after u1's notification")
	}

	c.handleSessionNotification(sessionInvalidateAll)
	if got, _ := c.begin("u2"); got.me != nil {
		t.Fatal("all entries must be cleared by the all-notification")
	}
}

func TestUserViewCacheSetEnabledClears(t *testing.T) {
	c := newTestUserViewCache()
	_, epoch := c.begin("u1")
	c.putMe(epoch, "u1", User{ID: "u1"})
	c.setEnabled(false)
	c.setEnabled(true)
	if got, _ := c.begin("u1"); got.me != nil {
		t.Fatal("reconnecting must discard cached values")
	}
}
