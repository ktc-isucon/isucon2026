package isureal

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

type User struct {
	ID             string  `db:"id" json:"id"`
	Username       string  `db:"username" json:"username"`
	SecretKeyHash  string  `db:"secret_key_hash" json:"-"`
	DisplayName    string  `db:"display_name" json:"displayName"`
	Bio            string  `db:"bio" json:"bio"`
	ProfileImageID *string `db:"profile_image_id" json:"-"`
	Region         string  `db:"region" json:"region"`
	AccountType    string  `db:"account_type" json:"accountType"`
	Status         string  `db:"status" json:"status"`
	ImageKey       []byte  `db:"-" json:"-"`
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

func validUserText(value string, minimum, maximum int) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && length >= minimum && length <= maximum
}

func validRegion(region string) bool {
	switch region {
	case "america", "europe", "asia":
		return true
	default:
		return false
	}
}

var userColumns = []string{"id", "username", "secret_key_hash", "display_name", "bio", "profile_image_id", "region", "account_type", "status"}

func userColumnsAs(alias, prefix string) string {
	columns := make([]string, len(userColumns))
	for i, column := range userColumns {
		columns[i] = fmt.Sprintf(`%s.%s AS "%s.%s"`, alias, column, prefix, column)
	}
	return strings.Join(columns, ", ")
}

// prefixedUserColumns は alias.column の形でユーザーの列を並べる(列名はそのまま)
func prefixedUserColumns(alias string) string {
	columns := make([]string, len(userColumns))
	for i, column := range userColumns {
		columns[i] = alias + "." + column
	}
	return strings.Join(columns, ", ")
}

// splitIDs は string_agg(',') で連結したIDを分割する
func splitIDs(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, ",")
}

func (user *User) setImageKey() error {
	key, err := imageSigningKey(user.SecretKeyHash)
	user.ImageKey = key
	return err
}

// lockUser は行ロック(FOR NO KEY UPDATE)と getUser を1往復で行う。
// 有効でないユーザーは getUser と同じく sql.ErrNoRows になる
func lockUser(ctx context.Context, db sqlx.QueryerContext, userID string) (User, error) {
	var user User
	err := sqlx.GetContext(ctx, db, &user, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE`, userID)
	if err != nil {
		return user, err
	}
	return user, user.setImageKey()
}

func getUser(ctx context.Context, db sqlx.QueryerContext, userID string) (User, error) {
	var user User
	err := sqlx.GetContext(ctx, db, &user, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = $1 AND status = 'active'`, userID)
	if err != nil {
		return user, err
	}
	return user, user.setImageKey()
}
