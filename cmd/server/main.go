package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/basketikun/chatgpt2api/internal/api"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/storage"
	"github.com/basketikun/chatgpt2api/internal/upstream"
	"github.com/basketikun/chatgpt2api/internal/web"
)

func main() {
	var (
		addr    = flag.String("addr", envOr("CHATGPT2API_ADDR", ":80"), "listen address")
		baseDir = flag.String("base-dir", "", "project root holding config.json / data / VERSION")
	)
	flag.Parse()

	if *baseDir == "" {
		*baseDir = config.ResolveBaseDir()
	}

	cfg, err := config.NewStore(*baseDir)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	backend, err := storage.NewFromEnv(cfg.DataDir())
	if err != nil {
		log.Fatalf("storage: %v", err)
	}

	logs, err := service.NewLogService(filepath.Join(cfg.DataDir(), "logs.jsonl"))
	if err != nil {
		log.Fatalf("log service: %v", err)
	}

	auth := service.NewAuthService(backend, cfg.AuthKey)
	accounts := service.NewAccountService(backend, cfg, logs)

	deps := &api.Deps{
		Config:  cfg,
		Storage: backend,
		Logs:    logs,
		Auth:    auth,
		Accts:   accounts,
		Proxy:   service.NewProxySettings(cfg),
		Images:  service.NewImageService(cfg),
		Tasks:   service.NewImageTaskService(cfg),
		Dist:    web.NewDist(),
	}
	// ModelFetcher：按账号类型聚合上游模型目录。
	// 传 nil 会让 /v1/models 永远只有图片模型（实测踩到），
	// 文本模型必须从这里动态拉。
	deps.Models = service.NewModelCatalog(accounts, func(ctx context.Context, accessToken string) ([]map[string]any, error) {
		client, err := upstream.NewClient(upstream.ClientConfig{
			AccessToken: accessToken,
			ProxyURL:    cfg.Proxy(),
		})
		if err != nil {
			return nil, err
		}
		defer client.Close()
		return client.ListModels(ctx)
	})
	deps.CPA = service.NewCPAConfig(cfg.DataDir())
	deps.Sub2API = service.NewSub2APIConfig(cfg.DataDir())
	deps.OAuth = service.NewOAuthLoginService()
	deps.ChatCache = protocol.NewChatCompletionCache(deps.ChatCacheSettings)
	deps.TextUpstream = func(ctx context.Context, accessToken string) (protocol.Upstream, func(), error) {
		client, err := upstream.NewClient(upstream.ClientConfig{AccessToken: accessToken, ProxyURL: cfg.Proxy()})
		if err != nil {
			return nil, nil, err
		}
		return client, client.Close, nil
	}
	deps.AccountInfoFetcher = func(ctx context.Context, accessToken string) (service.Account, error) {
		client, err := upstream.NewClient(upstream.ClientConfig{AccessToken: accessToken, ProxyURL: cfg.Proxy()})
		if err != nil {
			return nil, err
		}
		defer client.Close()
		return client.FetchAccountInfo(ctx)
	}
	deps.ImageUpstream = func(ctx context.Context, accessToken string) (protocol.ImageUpstream, func(), error) {
		client, err := upstream.NewClient(upstream.ClientConfig{AccessToken: accessToken, ProxyURL: cfg.Proxy()})
		if err != nil {
			return nil, nil, err
		}
		return client, client.Close, nil
	}

	// 注入异步图片任务的执行体。
	// 漏掉这一步会让 /api/image-tasks/* 全部失败（runner 恒为 nil），
	// 而生图页面默认走异步任务接口 —— 表现为「生图界面异常」。
	deps.RegisterImageTaskRunners()

	// 启动时清一次过期图片（Python 在 lifespan 里做）。
	if removed := cfg.CleanupOldImages(); removed > 0 {
		fmt.Printf("[startup] removed %d expired images\n", removed)
	}

	// 启动常驻后台任务。
	//
	// 这两个循环是 Python lifespan 里的对应物，缺了它们：
	//   - 限流账号在 restore_at 到期后永远不会自动恢复
	//   - 磁盘写满后所有生图静默失败（ENOSPC），且无自动清理
	watcher := service.NewAccountWatcher(accounts, cfg)
	watcher.Start(
		func(ctx context.Context, token, event string) (service.Account, error) {
			if deps.AccountInfoFetcher == nil {
				return nil, fmt.Errorf("account info fetcher is not configured")
			}
			return deps.AccountInfoFetcher(ctx, token)
		},
		nil, // keepalive 的 refresh_token 交换尚未实现，传 nil 时该分支自动跳过
	)

	cleanupScheduler := service.NewImageCleanupScheduler(service.CleanupActions{
		CleanupOldImages: cfg.CleanupOldImages,
		DiskFreeMB:       deps.Images.DiskFreeMB,
		DeleteToTarget:   deps.Images.DeleteToTarget,
	}, cfg)
	cleanupScheduler.Start()

	handler := api.NewRouter(deps)
	server := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 20 * time.Second,
		// 图片与可编辑文件链路是长连接，写超时必须留给上游，不能设短。
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		fmt.Printf("[server] chatgpt2api %s listening on %s (base=%s, storage=%s)\n",
			cfg.AppVersion(), *addr, cfg.BaseDir(), describeStorage(backend))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[server] shutdown: %v", err)
	}

	// 先停调度器再退出，避免关闭过程中还在写 data/。
	watcher.Stop()
	cleanupScheduler.Stop()

	// 最后等在途的图片任务写完 image_tasks.json。
	//
	// 顺序是刻意的：HTTP 已经停止接新请求（server.Shutdown），后台调度器也已停，
	// 此时唯一还在写 data/ 的就是任务 goroutine 与 resume-poll。
	// 放在最前面会让「关闭期间新提交的任务」在 Shutdown 之前插进来；
	// 放在 watcher 之前则会在等待任务时让 watcher 继续改账号文件。
	//
	// 超时不强杀任务（生图打一半被砍会在上游留下孤儿对话），只记录日志。
	taskCtx, taskCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer taskCancel()
	if err := deps.Tasks.Close(taskCtx); err != nil {
		log.Printf("[server] image tasks: %v", err)
	}

	fmt.Println("[server] stopped")
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func describeStorage(backend storage.Backend) string {
	info := backend.BackendInfo()
	if text, ok := info["type"].(string); ok {
		return text
	}
	return "unknown"
}
