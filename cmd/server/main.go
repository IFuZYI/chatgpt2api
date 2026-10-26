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

	"github.com/IFuZYI/chatgpt2api/internal/api"
	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/health"
	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
	"github.com/IFuZYI/chatgpt2api/internal/storage"
	"github.com/IFuZYI/chatgpt2api/internal/upstream"
	"github.com/IFuZYI/chatgpt2api/internal/web"
)

func main() {
	var (
		addr = flag.String("addr", envOr("CHATGPT2API_ADDR", ":8080"), "listen address")
		// 默认 8080 而不是 80：容器里以 nonroot（uid 65532）运行，
		// 绑定 <1024 的端口需要 CAP_NET_BIND_SERVICE，distroless 不授予。
		baseDir = flag.String("base-dir", "", "project root holding config.json / data / VERSION")
		// healthcheck 供容器 HEALTHCHECK 使用（distroless 没有 curl/wget）。
		healthcheck = flag.Bool("healthcheck", false, "probe /health on --addr and exit 0 if healthy")
		// init-proxy-config 替代已归档的 scripts/init_proxy_config.py：
		// WARP 部署的一次性 init 容器用它把 proxy_runtime 默认值合并进 config.json。
		initProxyConfig = flag.Bool("init-proxy-config", false,
			"merge WARP/FlareSolverr proxy_runtime defaults into config.json and exit")
	)
	flag.Parse()

	if *initProxyConfig {
		os.Exit(config.RunInitProxyRuntime())
	}

	if *healthcheck {
		probeAddr := *addr
		// :8080 → http://127.0.0.1:8080
		if strings.HasPrefix(probeAddr, ":") {
			probeAddr = "127.0.0.1" + probeAddr
		}
		probeURL := "http://" + probeAddr
		if err := health.Check(context.Background(), probeURL, 5*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

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
		// 敏感词与 AI 审核：不注入时设置页里配了也不生效。
		ContentFilter: service.NewContentFilter(service.ContentFilterOptions{
			SensitiveWords: cfg.SensitiveWords,
			AIReview:       cfg.AIReview,
		}),
		Accts:  accounts,
		Proxy:  service.NewProxySettings(cfg),
		Images: service.NewImageService(cfg),
		Tasks:  service.NewImageTaskService(cfg),
		// 可编辑文件（PPT/PSD）任务队列：产物落 data/files/{kind}/{task_id}/。
		EditableTasks: service.NewEditableFileTaskService(cfg.DataDir()),
		Dist:          web.NewDist(),
	}
	// ModelFetcher：按账号类型聚合上游模型目录。
	// 传 nil 会让 /v1/models 永远只有图片模型（实测踩到），
	// 文本模型必须从这里动态拉。
	deps.Models = service.NewModelCatalog(accounts, func(ctx context.Context, accessToken string) ([]map[string]any, error) {
		client, err := upstream.NewClient(upstream.ClientConfig{
			AccessToken: accessToken,
			ProxyURL:    accounts.ProxyFor(accessToken),
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
		client, err := upstream.NewClient(upstream.ClientConfig{AccessToken: accessToken, ProxyURL: accounts.ProxyFor(accessToken)})
		if err != nil {
			return nil, nil, err
		}
		return client, client.Close, nil
	}
	deps.AccountInfoFetcher = func(ctx context.Context, accessToken string) (service.Account, error) {
		client, err := upstream.NewClient(upstream.ClientConfig{AccessToken: accessToken, ProxyURL: accounts.ProxyFor(accessToken)})
		if err != nil {
			return nil, err
		}
		defer client.Close()
		return client.FetchAccountInfo(ctx)
	}
	// 密码重登的认证会话：Python 版 _login_with_password 用 impersonate="chrome110"，
	// 这里对齐同档 TLS profile。每个账号一次新会话（见 runRelogin）。
	deps.PasswordLoginClient = func() (service.PasswordLoginTransport, error) {
		client, err := upstream.NewClient(upstream.ClientConfig{Impersonate: "chrome110", ProxyURL: cfg.Proxy()})
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	// auto_relogin_after_refresh 用的执行体：复用同一套密码登录实现。
	// 必须在 NewRouter 之前接，否则第一次自动重登会因为未接线而被跳过。
	deps.Accts.SetAutoRelogin(deps.PasswordLoginFunc())
	deps.ImageUpstream = func(ctx context.Context, accessToken string) (protocol.ImageUpstream, func(), error) {
		client, err := upstream.NewClient(upstream.ClientConfig{AccessToken: accessToken, ProxyURL: accounts.ProxyFor(accessToken)})
		if err != nil {
			return nil, nil, err
		}
		return client, client.Close, nil
	}
	deps.SearchUpstream = func(ctx context.Context, accessToken string) (protocol.SearchUpstream, func(), error) {
		client, err := upstream.NewClient(upstream.ClientConfig{AccessToken: accessToken, ProxyURL: accounts.ProxyFor(accessToken)})
		if err != nil {
			return nil, nil, err
		}
		return client, client.Close, nil
	}
	deps.EditableUpstream = func(ctx context.Context, accessToken string) (protocol.EditableFileUpstream, func(), error) {
		client, err := upstream.NewClient(upstream.ClientConfig{AccessToken: accessToken, ProxyURL: accounts.ProxyFor(accessToken)})
		if err != nil {
			return nil, nil, err
		}
		return client, client.Close, nil
	}

	// 注入异步图片任务的执行体。
	// 漏掉这一步会让 /api/image-tasks/* 全部失败（runner 恒为 nil），
	// 而生图页面默认走异步任务接口 —— 表现为「生图界面异常」。
	deps.RegisterImageTaskRunners()
	// 同理：可编辑文件任务在后台 goroutine 里跑，执行体必须显式注入。
	deps.RegisterEditableFileTaskRunner()

	// 启动时清一次过期图片（Python 在 lifespan 里做）。
	//
	// 走 Images.CleanupExpiredImages 而不是 cfg.CleanupOldImages：
	// 后者只删本地文件，webdav 模式下空转、both 模式下留下「已过期但仍可读」
	// 的索引条目。前者复用 DeleteImages，本地/远端/索引/缩略图一起处理。
	if removed := deps.Images.CleanupExpiredImages(); removed > 0 {
		fmt.Printf("[startup] removed %d expired images\n", removed)
	}
	// 可编辑文件产物同样需要保留期清理：data/files/ 此前没有任何删除路径，
	// 实测 ~2 MiB/任务永久累积。
	if tasks, dirs := deps.EditableTasks.CleanupExpired(cfg.ImageRetentionDays()); tasks > 0 || dirs > 0 {
		fmt.Printf("[startup] removed %d expired editable tasks, %d artifact dirs\n", tasks, dirs)
	}

	// 启动常驻后台任务。
	//
	// 这两个循环是 Python lifespan 里的对应物，缺了它们：
	//   - 限流账号在 restore_at 到期后永远不会自动恢复
	//   - 磁盘写满后所有生图静默失败（ENOSPC），且无自动清理
	watcher := service.NewAccountWatcher(accounts, cfg)
	// 开了但不会生效的配置要在启动时明说，不能静默。
	deps.Accts.WarnUnsupportedAutoRelogin()

	watcher.Start(
		func(ctx context.Context, token, event string) (service.Account, error) {
			if deps.AccountInfoFetcher == nil {
				return nil, fmt.Errorf("account info fetcher is not configured")
			}
			return deps.AccountInfoFetcher(ctx, token)
		},
		// keepalive：用 refresh_token 换一套新 token。
		//
		// 返回完整三件套而不是只返回 access_token —— 上游每次换 AT 都会轮换
		// refresh_token（实测 196 → 211 字符），只取 AT 会把新 RT 丢掉。
		//
		// 不走账号代理：refresh_token 交换在 auth.openai.com，与号池出口无关。
		service.RefreshAccessToken,
	)

	cleanupScheduler := service.NewImageCleanupScheduler(service.CleanupActions{
		CleanupOldImages: deps.Images.CleanupExpiredImages,
		DiskFreeMB:       deps.Images.DiskFreeMB,
		DeleteToTarget:   deps.Images.DeleteToTarget,
	}, cfg)
	cleanupScheduler.Start()

	// 按 backup.interval_minutes 周期备份。没有它，设置页里开了「自动备份」
	// 也永远不会备份（配置只被保存，无人执行）。
	backupScheduler := deps.NewBackupScheduler()
	backupScheduler.Start()

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
	backupScheduler.Stop()

	// 最后等在途的图片任务写完 image_tasks.json。
	//
	// 顺序是刻意的：HTTP 已经停止接新请求（server.Shutdown），后台调度器也已停，
	// 此时唯一还在写 data/ 的就是任务 goroutine 与 resume-poll。
	// 放在最前面会让「关闭期间新提交的任务」在 Shutdown 之前插进来；
	// 放在 watcher 之前则会在等待任务时让 watcher 继续改账号文件。
	//
	// 超时不强杀任务（生图打一半被砍会在上游留下孤儿对话），只记录日志。
	// 每个收尾步骤用**独立**预算，不共用一个 ctx。
	//
	// 原来三个 Close 共享一个 30s 的 taskCtx：runImageTask 用
	// context.Background() 且不可取消，Tasks.Close 常常把预算全部耗尽，
	// 后面两个 Close 拿到的是已过期的 ctx，立即返回 ——
	// ADR-0012 承诺的「进程退出前不留下半截上传」在实际最需要它的场景下不成立。
	//
	// 顺序必须保持「任务先、上传后」：
	// ImageService.Close 会置 uploadClosed=true，此后 startBackgroundUpload
	// 直接早退。若先关 Images 再等 Tasks，仍在跑的生图任务把结果存到本地
	// 就再也不会传远端的（both 模式下静默丢上传）——
	// 用「半截上传」换来「完全没上传」是更差的结果。
	taskCtx, taskCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := deps.Tasks.Close(taskCtx); err != nil {
		log.Printf("[server] image tasks: %v", err)
	}
	taskCancel()
	// 可编辑文件任务同样会写 data/（editable_file_tasks.json 与产物文件），
	// 必须在退出前等它们收尾，否则任务文件可能只写了一半。
	if deps.EditableTasks != nil {
		editCtx, editCancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := deps.EditableTasks.Close(editCtx); err != nil {
			log.Printf("[server] editable file tasks: %v", err)
		}
		editCancel()
	}
	// 最后等异步 WebDAV 上传：此时所有任务都已收尾，不会再有新的上传入队，
	// 独立预算也不会被任务 goroutine 吃掉。
	uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := deps.Images.Close(uploadCtx); err != nil {
		log.Printf("[server] pending image uploads: %v", err)
	}
	uploadCancel()
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
