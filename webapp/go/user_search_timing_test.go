package isureal

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// TestUserSearchBackfillTiming は約5,000人での一括更新の時間を測る(ISUREAL_TEST_DSN が無いときは飛ばす)。
func TestUserSearchBackfillTiming(t *testing.T) {
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
		`CREATE TABLE users (id varchar(36) PRIMARY KEY, username varchar(64) NOT NULL, secret_key_hash text NOT NULL,
		  display_name text NOT NULL, bio text NOT NULL DEFAULT '', profile_image_id varchar(36), region text NOT NULL,
		  account_type text NOT NULL DEFAULT 'regular', status text NOT NULL DEFAULT 'active')`,
		`INSERT INTO users (id, username, secret_key_hash, display_name, region)
		  SELECT 'id-'||g, 'user'||g, md5(g::text), 'Display Name '||g||' Straße', 'asia' FROM generate_series(1, 5043) g`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := prepareUserSearch(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Logf("first prepare (schema + 5043 rows): %v", time.Since(start))
	if _, err := db.ExecContext(ctx, `UPDATE users SET display_name = display_name || ' x' WHERE id IN (SELECT 'id-'||g FROM generate_series(1, 50) g)`); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err := backfillUserSearchColumns(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Logf("backfill after init (50 changed rows): %v", time.Since(start))
	start = time.Now()
	if err := backfillUserSearchColumns(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Logf("backfill with nothing to do: %v", time.Since(start))
}
