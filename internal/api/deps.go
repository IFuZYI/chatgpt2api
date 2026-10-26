package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/IFuZYI/chatgpt2api/internal/config"
	"github.com/IFuZYI/chatgpt2api/internal/protocol"
	"github.com/IFuZYI/chatgpt2api/internal/service"
	"github.com/IFuZYI/chatgpt2api/internal/storage"
	"github.com/IFuZYI/chatgpt2api/internal/web"
	"github.com/gin-gonic/gin"
)

// Deps 是所有 handler 需要的依赖集合。
// 显式传递而不是用全局单例，方便测试时替换。
type Deps struct {
	Config  *config.Store
	Storage storage.Backend
	Logs    *service.LogService
	Auth    *service.AuthService
	Accts   *service.AccountService
	Proxy   *service.ProxySettings
	Images  *service.ImageService
	Tasks   *service.ImageTaskService
	Models  *service.ModelCatalog
	CPA     *service.CPAConfig
	Sub2API *service.Sub2APIConfig
	OAuth   *service.OAuthLoginService

	// backupState 维护备份运行状态（内存 running + 磁盘状态文件）。
	backupStateStore *backupStateStore
	Dist             *web.Dist

	// TextUpstream 为文本请求构造上游客户端；ChatCache 是短 TTL 缓存。
	TextUpstream  TextUpstreamFactory
	ImageUpstream ImageUpstreamFactory
	// SearchUpstream 为 /v1/search 构造上游客户端。
	SearchUpstream SearchUpstreamFactory
	// EditableUpstream 为 PPT/PSD 导出构造上游客户端。
	EditableUpstream EditableUpstreamFactory
	// EditableTasks 是可编辑文件任务队列（PPT/PSD 异步任务 + /files/* 产物）。
	EditableTasks *service.EditableFileTaskService
	ChatCache     *protocol.ChatCompletionCache

	// ContentFilter 在请求进上游之前做敏感词与 AI 审核。
	// 为 nil 时跳过检查（测试与未接线场景）；生产在 NewDeps 里注入。
	ContentFilter *service.ContentFilter

	// AccountInfoFetcher 拉取账号信息（刷新链路用）。未注入时刷新会报错。
	AccountInfoFetcher func(ctx context.Context, accessToken string) (service.Account, error)

	// PasswordLoginClient 为密码重登构造一个认证会话（带 TLS 指纹的上游客户端）。
	// 未注入时重登会逐条报 password_login_not_configured，而不是假装成功。
	PasswordLoginClient service.PasswordLoginClientFactory
}

// Identity 是认证后的调用者身份，字段与 Python 的 require_identity 返回值一致。
type Identity struct {
	ID   string
	Name string
	Role string
}

const identityKey = "dsh.identity"

// RequireIdentityCtx 鉴权并把调用方名字放进 gin context。
//
// 存在的理由是 /v1 调用日志要记 key_name（前端日志页按它展示调用方）。
// handler 里直接 RequireIdentity 拿不到 gin context，逐个 Set 又会漏，
// 所以统一走这个包装。
func (d *Deps) RequireIdentityCtx(c *gin.Context) (*Identity, *HTTPError) {
	identity, err := d.RequireIdentity(c.GetHeader("Authorization"))
	if err != nil {
		return nil, err
	}
	c.Set("key_name", identity.Name)
	return identity, nil
}

// ExtractBearerToken 复刻 Python 的 extract_bearer_token：
// scheme 必须（大小写不敏感）是 bearer，且值非空。
func ExtractBearerToken(authorization string) string {
	scheme, value, found := strings.Cut(strings.TrimSpace(authorization), " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return trimmed
}

// legacyAdminIdentity 是 config.auth-key 这条后门身份，role 固定 admin。
func (d *Deps) legacyAdminIdentity(token string) *Identity {
	authKey := strings.TrimSpace(d.Config.AuthKey())
	if authKey != "" && token == authKey {
		return &Identity{ID: "admin", Name: "管理员", Role: "admin"}
	}
	return nil
}

// RequireIdentity 解析身份，失败时返回 401 的 HTTPError。
func (d *Deps) RequireIdentity(authorization string) (*Identity, *HTTPError) {
	token := ExtractBearerToken(authorization)
	if identity := d.legacyAdminIdentity(token); identity != nil {
		return identity, nil
	}
	if token == "" {
		return nil, Errorf(http.StatusUnauthorized, "密钥无效或已失效，请重新登录")
	}
	item := d.Auth.Authenticate(token)
	if item == nil {
		return nil, Errorf(http.StatusUnauthorized, "密钥无效或已失效，请重新登录")
	}
	return &Identity{
		ID:   stringOf(item["id"]),
		Name: stringOf(item["name"]),
		Role: stringOf(item["role"]),
	}, nil
}

// RequireAdmin 在 RequireIdentity 之上加角色校验。
func (d *Deps) RequireAdmin(authorization string) (*Identity, *HTTPError) {
	identity, err := d.RequireIdentity(authorization)
	if err != nil {
		return nil, err
	}
	if identity.Role != "admin" {
		return nil, Errorf(http.StatusForbidden, "需要管理员权限才能执行这个操作")
	}
	return identity, nil
}

// ChatCacheSettings 供 protocol.ChatCompletionCache 读取运行时配置。
func (d *Deps) ChatCacheSettings() protocol.CacheSettings {
	return d.chatCacheSettings()
}

// Middleware 把 Bearer token 解析成 Identity 放进 context。
// 注意：它只做解析，不做拒绝——是否必须登录由 handler 决定，
// 因为不同路由的鉴权等级不同（有些只认 admin）。
func (d *Deps) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authorization := c.GetHeader("Authorization")
		if authorization == "" {
			authorization = "Bearer " + c.GetHeader("x-api-key")
			if c.GetHeader("x-api-key") == "" {
				authorization = ""
			}
		}
		if identity, err := d.RequireIdentity(authorization); err == nil {
			c.Set(identityKey, identity)
		}
		c.Next()
	}
}

// currentIdentity 返回 middleware 已经解析好的身份（可能为 nil）。
func currentIdentity(c *gin.Context) *Identity {
	value, ok := c.Get(identityKey)
	if !ok {
		return nil
	}
	identity, _ := value.(*Identity)
	return identity
}

// writeErr 是 handler 里最常用的出口。
func writeErr(c *gin.Context, err *HTTPError) {
	WriteError(c, err)
	c.Abort()
}

// writeAdminErr 强制使用管理面信封（用于 /api/* 内部调用 /v1 逻辑的场景）。
func writeAdminErr(c *gin.Context, err *HTTPError) {
	WriteErrorEnvelope(c, err, EnvelopeAdmin)
	c.Abort()
}

// goSafe 在后台 goroutine 里跑 fn，panic 时 recover 并记日志。
//
// 为什么需要：gin.Recovery() 只兜 handler goroutine。api 层有多处
// `go func()`（刷新、重登、CPA 导入、sub2api 导入、图片任务）在 handler
// 返回后继续跑 —— 其中任何一个 panic 都会终止整个进程，所有在途任务
// 与刷新一起丢。service 层的 ImageTaskService.runTask 已因同样理由
// 自己加了 recover 并写了注释，api 层这几处没跟上。
//
// logs 为 nil 时只 recover 不记录（测试用）。
func goSafe(logs *service.LogService, name string, fn func()) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if logs != nil {
					logs.Add(service.LogTypeAccount, name+" 后台任务 panic 已拦截", map[string]any{
						"panic": fmt.Sprintf("%v", recovered),
					})
				}
			}
		}()
		fn()
	}()
}
