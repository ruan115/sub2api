package main

import (
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/service"
)

func main() {
	service.Main(config.RoleHostAgent)
}
