// Package main 服务入口。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"phanthywork2api/internal/server"
)

var (
	configPath = flag.String("config", "config.json", "配置文件路径")
	listen     = flag.String("listen", "", "覆盖 config.listen")
	apiKey     = flag.String("api-key", "", "覆盖 config.api_key（空=不鉴权）")
)

func main() {
	flag.Parse()
	cfg := server.DefaultConfig()

	targetConfig := *configPath
	if _, err := os.Stat(targetConfig); os.IsNotExist(err) && targetConfig == "config.json" {
		if _, err := os.Stat("config.example.json"); err == nil {
			targetConfig = "config.example.json"
		}
	}

	if f, err := os.ReadFile(targetConfig); err == nil {
		if err := cfg.Load(f); err != nil {
			log.Fatalf("加载配置 %s: %v", targetConfig, err)
		}
	} else if !os.IsNotExist(err) {
		log.Fatalf("读取配置 %s: %v", targetConfig, err)
	}

	if envPort := os.Getenv("PORT"); envPort != "" {
		if envPort[0] != ':' {
			cfg.Listen = ":" + envPort
		} else {
			cfg.Listen = envPort
		}
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	if envKey := os.Getenv("API_KEY"); envKey != "" {
		cfg.APIKey = envKey
	}
	if *apiKey != "" {
		cfg.APIKey = *apiKey
	}

	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("初始化: %v", err)
	}
	log.Printf("phanthywork2api 监听 %s（auths=%s，账号=%d）", cfg.Listen, cfg.AuthDir, len(srv.Accounts()))
	if err := srv.Run(); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
	_ = fmt.Sprint()
}
