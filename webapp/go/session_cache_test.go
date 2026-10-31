package isureal

import "testing"

func newTestSessionCache() *sessionCache {
	return &sessionCache{byHash: map[string]Session{}, byUser: map[string]map[string]struct{}{}}
}

func TestSessionCacheDisabledUntilListening(t *testing.T) {
	c := newTestSessionCache()
	_, _, epoch := c.begin("h1")
	c.put(epoch, Session{UserID: "u1", TokenHash: "h1"})
	if _, hit, _ := c.begin("h1"); hit {
		t.Fatal("LISTEN前はキャッシュしない")
	}

	c.setEnabled(true)
	_, _, epoch = c.begin("h1")
	c.put(epoch, Session{UserID: "u1", TokenHash: "h1"})
	if _, hit, _ := c.begin("h1"); !hit {
		t.Fatal("LISTEN中はキャッシュする")
	}

	c.setEnabled(false)
	if _, hit, _ := c.begin("h1"); hit {
		t.Fatal("LISTENが切れたら使わない")
	}
	c.setEnabled(true)
	if _, hit, _ := c.begin("h1"); hit {
		t.Fatal("繋ぎ直した直後は空")
	}
}

func TestSessionCacheInvalidationDuringRead(t *testing.T) {
	c := newTestSessionCache()
	c.setEnabled(true)
	// DBを読んでいる間に無効化が届いたら、読んだ値は保存しない
	_, _, epoch := c.begin("h1")
	c.handleNotification("u1")
	c.put(epoch, Session{UserID: "u1", TokenHash: "h1"})
	if _, hit, _ := c.begin("h1"); hit {
		t.Fatal("読む間に無効化があった値を保存した")
	}
}

func TestSessionCacheInvalidateUserAndAll(t *testing.T) {
	c := newTestSessionCache()
	c.setEnabled(true)
	for _, s := range []Session{{UserID: "u1", TokenHash: "a"}, {UserID: "u1", TokenHash: "b"}, {UserID: "u2", TokenHash: "c"}} {
		_, _, epoch := c.begin(s.TokenHash)
		c.put(epoch, s)
	}
	c.handleNotification("u1")
	for _, h := range []string{"a", "b"} {
		if _, hit, _ := c.begin(h); hit {
			t.Fatalf("u1のセッション %s が残っている", h)
		}
	}
	if _, hit, _ := c.begin("c"); !hit {
		t.Fatal("u2のセッションまで消えた")
	}
	c.handleNotification(sessionInvalidateAll)
	if _, hit, _ := c.begin("c"); hit {
		t.Fatal("全消去で消えていない")
	}
}
