package isureal

import (
	"net"
	"net/url"
	"os"
)

type Config struct {
	Region     string
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	LogLevel   string

	// 読み取り用の手元のレプリカ。空なら使わない(従来どおりprimaryだけで動く)
	ReplicaDBHost string
	ReplicaDBPort string
}

func ConfigFromEnv() *Config {
	return &Config{
		Region:     envOrDefault("ISUREAL_REGION", "asia"),
		DBHost:     envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
		DBPort:     envOrDefault("ISUREAL_DB_PORT", "5432"),
		DBUser:     envOrDefault("ISUREAL_DB_USER", "isureal"),
		DBPassword: envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
		DBName:     envOrDefault("ISUREAL_DB_NAME", "isureal"),
		LogLevel:   envOrDefault("ISUREAL_LOG_LEVEL", "debug"),

		ReplicaDBHost: os.Getenv("ISUREAL_REPLICA_DB_HOST"),
		ReplicaDBPort: envOrDefault("ISUREAL_REPLICA_DB_PORT", "5432"),
	}
}

func (c *Config) DatabaseDSN() string {
	return c.dsn(c.DBHost, c.DBPort)
}

// ReplicaDatabaseDSN は読み取り用レプリカのDSNを返す。レプリカを使わない場合は空文字列。
func (c *Config) ReplicaDatabaseDSN() string {
	if c.ReplicaDBHost == "" {
		return ""
	}
	return c.dsn(c.ReplicaDBHost, c.ReplicaDBPort)
}

func (c *Config) dsn(host, port string) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.DBUser, c.DBPassword),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + c.DBName,
		// DBへの通信はプライベートネットワーク内(ルーター経由)のため、TLSの往復を省く
		// default_query_exec_mode=exec: 名前なしの文で準備と実行を1往復で送り、接続ごとの準備の往復をなくす
		RawQuery: "sslmode=disable&default_query_exec_mode=exec",
	}
	return u.String()
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
