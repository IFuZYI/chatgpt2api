package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// NewRouter 组装全部路由。
//
// 三个必须注意的 Gin 行为（已实测，见计划书）：
//  1. 不能注册 /*full_path：与 /v1 等前缀冲突会 panic。SPA fallback 走 NoRoute。
//  2. HandleMethodNotAllowed=true 时 HEAD 会被判成 405，而 FastAPI 的 HEAD 落到 SPA。
//     所以 NoMethod 里对 HEAD 转 SPA，其余返回 405。
//  3. FastAPI 对「路径不存在 + 方法也不对」返回 405 + Allow 头，Gin 返回 404。
//     这是已知发散，golden 里标 intentional-divergence。
func NewRouter(d *Deps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())
	r.HandleMethodNotAllowed = true

	d.RegisterSystemRoutes(r)
	d.RegisterAuthRoutes(r)
	d.RegisterAccountRoutes(r)
	d.RegisterImageRoutes(r)
	d.RegisterSettingsRoutes(r)
	d.RegisterV1Routes(r)
	d.registerChatCompletions(r)
	d.registerImageGeneration(r)
	d.registerRemainingRoutes(r)
	d.registerIntegrationRoutes(r)

	spa := func(c *gin.Context) {
		requested := strings.TrimPrefix(c.Request.URL.Path, "/")
		if d.Dist == nil {
			WriteError(c, StatusError(http.StatusNotFound, "Not Found"))
			return
		}
		if payload, resolved, ok := d.Dist.Resolve(requested); ok {
			// 按**解析出的实际文件名**推断 MIME，不是按请求路径。
			// `/` 与 `/accounts` 这类无扩展名路径会解析到 index.html，
			// 按请求路径推断会得到 application/octet-stream（实测踩到）。
			c.Data(http.StatusOK, contentTypeFor(resolved), payload)
			return
		}
		if isNextAsset(requested) {
			WriteError(c, StatusError(http.StatusNotFound, "Not Found"))
			return
		}
		if payload, ok := d.Dist.Fallback(); ok {
			c.Data(http.StatusOK, "text/html; charset=utf-8", payload)
			return
		}
		WriteError(c, StatusError(http.StatusNotFound, "Not Found"))
	}

	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			spa(c)
			return
		}
		WriteError(c, StatusError(http.StatusNotFound, "Not Found"))
	})

	r.NoMethod(func(c *gin.Context) {
		// FastAPI 的 HEAD 与 GET 同路由，未知 HEAD 会落到 SPA 返回 200。
		if c.Request.Method == http.MethodHead {
			spa(c)
			return
		}
		allow := c.Writer.Header().Get("Allow")
		err := StatusError(http.StatusMethodNotAllowed, "Method Not Allowed")
		if allow != "" {
			err.Headers = map[string]string{"Allow": allow}
		}
		WriteError(c, err)
	})

	return r
}

func isNextAsset(requested string) bool {
	clean := strings.Trim(strings.ReplaceAll(requested, "\\", "/"), "/")
	return strings.HasPrefix(clean, "_next/")
}

// contentTypeFor 按扩展名给静态资源定 MIME。Go 的 mime 包对 .js/.css 已经够用，
// 这里只补 Next.js 导出常见但标准库缺的几种。
func contentTypeFor(requested string) string {
	lower := strings.ToLower(requested)
	switch {
	case strings.HasSuffix(lower, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".mjs"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(lower, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(lower, ".json"):
		return "application/json"
	case strings.HasSuffix(lower, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(lower, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(lower, ".woff"):
		return "font/woff"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// corsMiddleware 复刻 FastAPI 的 CORSMiddleware 配置：
// allow_origins=["*"], allow_credentials=False, allow_methods=["*"], allow_headers=["*"]。
// 实测 FastAPI 的预检返回 200 + max-age 600，这里保持一致。
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", "*")
			c.Header("Access-Control-Allow-Methods", "DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT")
			requestHeaders := c.GetHeader("Access-Control-Request-Headers")
			if requestHeaders == "" {
				requestHeaders = "*"
			}
			c.Header("Access-Control-Allow-Headers", requestHeaders)
			c.Header("Access-Control-Max-Age", "600")
			if c.Request.Method == http.MethodOptions {
				c.AbortWithStatus(http.StatusOK)
				return
			}
		}
		c.Next()
	}
}
