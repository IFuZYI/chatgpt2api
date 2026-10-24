package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/protocol"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/storage"
	"github.com/basketikun/chatgpt2api/internal/web"
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
	ChatCache     *protocol.ChatCompletionCache

	// ContentFilter 在请求进上游之前做敏感词与 AI 审核。
	// 为 nil 时跳过检查（测试与未接线场景）；生产在 NewDeps 里注入。
	ContentFilter *service.ContentFilter

	// AccountInfoFetcher 拉取账号信息（刷新链路用）。未注入时刷新会报错。
	AccountInfoFetcher func(ctx context.Context, accessToken string) (service.Account, error)
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
