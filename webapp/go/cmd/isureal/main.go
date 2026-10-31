package main

import (
	"os"

	"example.com/isureal-company/isureal"
	"github.com/kaz/pprotein/integration/standalone"
)

// 2026-09-23

func main() {
	// 最終計測では pprotein のデバッグサーバー(pprof・ログの配信)を止める(scripts/final_mode.sh)
	if os.Getenv("ISUREAL_DISABLE_DEBUG_SERVER") == "" {
		go standalone.Integrate(":8383")
	}

	config := isureal.ConfigFromEnv()
	server := isureal.NewServer(config)
	if err := server.Start(":8080"); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
