package main

import (
	"example.com/isureal-company/isureal"
	"github.com/kaz/pprotein/integration/standalone"
)

// 2026-09-23

func main() {
	go standalone.Integrate(":8383")

	config := isureal.ConfigFromEnv()
	server := isureal.NewServer(config)
	if err := server.Start(":8080"); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
