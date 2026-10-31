package isureal

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestLikeContainsPattern(t *testing.T) {
	cases := map[string]string{
		"abc":     "%abc%",
		"a%b":     `%a\%b%`,
		"a_b":     `%a\_b%`,
		`a\b`:     `%a\\b%`,
		`%_\`:     `%\%\_\\%`,
		"":        "%%",
		"ßtrasse": "%ßtrasse%",
	}
	for in, want := range cases {
		if got := likeContainsPattern(in); got != want {
			t.Errorf("likeContainsPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSearchUsersIndexedMatchesLegacy は実際の PostgreSQL で、索引を使う検索と今までの全件取得の検索が
// 同じ結果を返すことを確かめる。ISUREAL_TEST_DSN が無いときは飛ばす。
func TestSearchUsersIndexedMatchesLegacy(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	ctx := context.Background()
	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS block_relations, users`,
		`CREATE TABLE users (
		  id varchar(36) PRIMARY KEY, username varchar(64) NOT NULL, secret_key_hash text NOT NULL,
		  display_name text NOT NULL, bio text NOT NULL DEFAULT '', profile_image_id varchar(36),
		  region text NOT NULL, account_type text NOT NULL DEFAULT 'regular', status text NOT NULL DEFAULT 'active')`,
		`CREATE UNIQUE INDEX users_username ON users(username)`,
		`CREATE TABLE block_relations (id varchar(36) PRIMARY KEY, blocker_id varchar(36) NOT NULL, blocked_id varchar(36) NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	names := []struct{ username, display string }{
		{"alice", "Alice Liddell"}, {"ALICE2", "ALICE"}, {"bob", "Bob_the_builder"}, {"carol", "Straße"},
		{"dave", "STRASSE"}, {"eve", "ｅｖｅ全角"}, {"frank", "Café"}, {"grace", "Café"},
		{"heidi", "100% real"}, {"ivan", `back\slash`}, {"judy", "日本語のなまえ"}, {"mallory", "ǅemal"},
		{"oscar", "ΣΊΣΥΦΟΣ"}, {"peggy", "σίσυφος"}, {"trent", "a_b%c"}, {"victor", "İstanbul"},
		{"walter", "ﬁne"}, {"xena", "FINE"}, {"yolanda", "   spaced   "}, {"zed", "zed"},
	}
	for i := range 120 {
		names = append(names, struct{ username, display string }{fmt.Sprintf("user%03d", i), fmt.Sprintf("Display %d ali", i%7)})
	}
	for i, n := range names {
		status := "active"
		if i%11 == 5 {
			status = "deleted"
		}
		accountType := "regular"
		if i%4 == 0 {
			accountType = "certified_public"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO users (id, username, secret_key_hash, display_name, bio, region, account_type, status) VALUES ($1,$2,$3,$4,$5,'asia',$6,$7)`,
			fmt.Sprintf("id-%04d", i), n.username, fmt.Sprintf("%064x", i), n.display, "bio "+n.display, accountType, status); err != nil {
			t.Fatal(err)
		}
	}
	viewer := "id-0000"
	for _, blocked := range []struct{ blocker, blocked string }{{viewer, "id-0002"}, {"id-0007", viewer}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO block_relations VALUES ($1,$2,$3)`, blocked.blocker+blocked.blocked, blocked.blocker, blocked.blocked); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareUserSearch(ctx, db); err != nil {
		t.Fatal(err)
	}
	// 表示名を直接書き換えて食い違わせても、一括更新で合わせ直されること
	if _, err := db.ExecContext(ctx, `UPDATE users SET display_name = 'Renamed ALI' WHERE id = 'id-0030'`); err != nil {
		t.Fatal(err)
	}
	if err := backfillUserSearchColumns(ctx, db); err != nil {
		t.Fatal(err)
	}

	queries := []string{"a", "A", "ali", "ALI", "alice", "strasse", "straße", "STRAẞE", "café", "café", "eve", "ｅｖｅ",
		"%", "_", `\`, "a_b", "100%", "日本", "σίσυφος", "ΣΊΣΥΦΟΣ", "ς", "fine", "ﬁ", "i̇stanbul", "istanbul", " ", "display 3",
		"user0", "user1", "zzz", "Renamed", "ǆ", "é"}
	cursors := []struct{ username, id string }{{"", ""}, {"user050", "id-0070"}, {"bob", "id-0002"}, {"user119", "id-0139"}}
	for _, viewerID := range []string{viewer, "id-0010"} {
		for _, q := range queries {
			for _, cur := range cursors {
				input := searchUsersRequest{Q: q, BeforeUsername: cur.username, BeforeID: cur.id}
				legacy, err := searchUsersLegacy(ctx, db, viewerID, input)
				if err != nil {
					t.Fatal(err)
				}
				indexed, err := searchUsersIndexed(ctx, db, viewerID, input)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(legacy, indexed) {
					t.Errorf("viewer=%s q=%q cursor=%v: legacy %d users, indexed %d users", viewerID, q, cur, len(legacy), len(indexed))
				}
			}
		}
	}
}
