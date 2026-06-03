package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"claude-code-gateway/internal/app"
)

// main 是服务进程入口，它只负责装配运行时配置、启动 HTTP 服务并处理优雅退出。
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runtimeConfig := app.LoadRuntimeConfig()
	server, err := app.NewServer(ctx, runtimeConfig)
	if err != nil {
		log.Fatalf("启动 Claude Code Gateway 失败: %v", err)
	}
	defer server.Close()

	httpServer := &http.Server{
		Addr:              server.ListenAddress(),
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP 服务关闭失败: %v", err)
		}
	}()

	log.Printf("Claude Code Gateway 正在监听 %s", server.ListenAddress())
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP 服务运行失败: %v", err)
	}

	if ctx.Err() == nil {
		os.Exit(0)
	}
}
