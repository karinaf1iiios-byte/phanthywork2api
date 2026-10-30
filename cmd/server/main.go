// Package main 服务入口。
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"phanthywork2api/internal/server"
)

var (
	configPath = flag.String("config", "config.json", "配置文件路径")
	listen     = flag.String("listen", "", "覆盖 config.listen")
	apiKey     = flag.String("api-key", "", "覆盖 config.api_key（空=不鉴权）")
)

func restoreAuths(dir, b64 string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return fmt.Errorf("base64 解码: %w", err)
	}
	var files map[string]string
	if err := json.Unmarshal(raw, &files); err != nil {
		return fmt.Errorf("json 解析: %w", err)
	}
	for name, content := range files {
		clean := filepath.Base(name)
		if clean == "." || clean == "/" || strings.Contains(clean, "..") {
			continue
		}
		target := filepath.Join(dir, clean)
		if err := os.WriteFile(target, []byte(content), 0600); err != nil {
			return fmt.Errorf("写入 %s: %w", clean, err)
		}
	}
	return nil
}

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

	if b64 := os.Getenv("AUTHS_BASE64"); b64 != "" {
		if err := restoreAuths(cfg.AuthDir, b64); err != nil {
			log.Printf("[auth] 从 AUTHS_BASE64 恢复凭证失败: %v", err)
		} else {
			log.Printf("[auth] 成功从 AUTHS_BASE64 恢复凭证到 %s", cfg.AuthDir)
		}
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
