package isureal

import (
	"testing"
	"time"
)

func TestClassifyBanFailure(t *testing.T) {
	cases := []struct {
		action string
		status int
		ip     string
		abuse  string
		want   banFailureKind
	}{
		{"login", 401, "1.2.3.4", "", banLoginFailure},
		{"login", 400, "1.2.3.4", "", ""},
		{"login", 200, "1.2.3.4", "", ""},
		{"login", 401, "", "", ""},
		{"put_profile_image", 400, "1.2.3.4", "invalid_image", banInvalidImage},
		{"put_profile_image", 400, "1.2.3.4", "", ""},
		{"put_profile_image", 415, "1.2.3.4", "invalid_image", ""},
		{"get_image", 403, "1.2.3.4", "invalid_signature", banInvalidSignature},
		{"get_image", 403, "1.2.3.4", "", ""},
		{"get_image", 404, "1.2.3.4", "invalid_signature", ""},
		{"register", 401, "1.2.3.4", "", ""},
		{"", 401, "1.2.3.4", "", ""},
	}
	for _, c := range cases {
		if got := classifyBanFailure(c.action, c.status, c.ip, c.abuse); got != c.want {
			t.Errorf("classify(%q,%d,%q,%q)=%q want %q", c.action, c.status, c.ip, c.abuse, got, c.want)
		}
	}
}

func TestBanDetectorThresholdAndWindow(t *testing.T) {
	base := time.Unix(1000, 0)
	for _, tc := range []struct {
		name      string
		threshold int
		window    time.Duration
	}{
		{"login", 10, 10 * time.Second},
		{"invalid_image", 2, 5 * time.Second},
		{"invalid_signature", 3, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newBanDetector(tc.threshold, tc.window)
			// 閾値-1回では BAN しない
			for i := 0; i < tc.threshold-1; i++ {
				if d.observe("a", base.Add(time.Duration(i)*time.Millisecond)) {
					t.Fatalf("banned too early at %d", i)
				}
			}
			// 閾値回目で BAN
			if !d.observe("a", base.Add(time.Duration(tc.threshold)*time.Millisecond)) {
				t.Fatalf("not banned at threshold")
			}
			// BAN 後は数え直す
			for i := 0; i < tc.threshold-1; i++ {
				if d.observe("a", base.Add(time.Second+time.Duration(i)*time.Millisecond)) {
					t.Fatalf("banned too early after reset at %d", i)
				}
			}
			if !d.observe("a", base.Add(time.Second+time.Duration(tc.threshold)*time.Millisecond)) {
				t.Fatalf("not banned after reset")
			}
			// 窓を過ぎた失敗は数えない
			d2 := newBanDetector(tc.threshold, tc.window)
			for i := 0; i < tc.threshold-1; i++ {
				d2.observe("b", base)
			}
			if tc.threshold > 1 && d2.observe("b", base.Add(tc.window)) {
				t.Fatalf("counted failures outside window")
			}
			// 別の IP は別に数える
			d3 := newBanDetector(tc.threshold, tc.window)
			for i := 0; i < tc.threshold-1; i++ {
				d3.observe("c", base)
			}
			if tc.threshold > 1 && d3.observe("d", base) {
				t.Fatalf("other ip affected")
			}
		})
	}
}

func TestBanGuardObserveAddsLocalBan(t *testing.T) {
	g := newBanGuard(10, 10*time.Second, 2*time.Minute)
	bans := &IPBanList{}
	g.bans = bans
	now := time.Now()
	if g.observe("put_profile_image", 400, "5.6.7.8", "invalid_image", now) {
		t.Fatal("banned after 1")
	}
	if !g.observe("put_profile_image", 400, "5.6.7.8", "invalid_image", now.Add(time.Millisecond)) {
		t.Fatal("not banned after 2")
	}
	if !bans.Banned("5.6.7.8") {
		t.Fatal("local ban not applied")
	}
	if bans.Banned("9.9.9.9") {
		t.Fatal("unexpected ban")
	}
	// 種類ごとに別に数える: login 失敗1回では BAN しない
	if g.observe("login", 401, "1.1.1.1", "", now) {
		t.Fatal("login banned too early")
	}
	// ip が空なら対象外
	for i := 0; i < 5; i++ {
		if g.observe("get_image", 403, "", "invalid_signature", now) {
			t.Fatal("empty ip banned")
		}
	}
	bans.resetLocal()
}
