package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"github.com/basketikun/chatgpt2api/internal/protocol"
)

// 上游默认常量，逐字取自 services/openai_backend_api.py。
const (
	BaseURL               = "https://chatgpt.com"
	DefaultClientVersion  = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
	DefaultClientBuild    = "6708908"
	DefaultPowScript      = "https://chatgpt.com/backend-api/sentinel/sdk.js"
	SearchModel           = "gpt-5-5"
	CodexResponsesModel   = "gpt-5.5"
	EditableFileModel     = "gpt-5-5-thinking"
	EditableClientVersion = "prod-bede35f9dcd856d080e012478f0c1031faa2588e"
	EditableClientBuild   = "6631702"
)

// Requirements 是一次 sentinel 握手的结果。
type Requirements struct {
	Token          string
	ProofToken     string
	TurnstileToken string
	SOToken        string
	Raw            map[string]any
}

// Client 是上游客户端。一个 Client 绑定一个账号（或匿名），
// 复刻 Python 的 OpenAIBackendAPI 生命周期。
type Client struct {
	http          tls_client.HttpClient
	profile       BrowserProfile
	accessToken   string
	userAgent     string
	deviceID      string
	sessionID     string
	clientVersion string
	clientBuild   string

	powScriptSources []string
	powDataBuild     string
	bootstrapped     bool
}

// ClientConfig 是构造 Client 的输入。
type ClientConfig struct {
	AccessToken string
	Impersonate string
	ProxyURL    string
	SkipVerify  bool
	// UserAgent / DeviceID / SessionID 来自账号的 fp 字段，可留空。
	UserAgent string
	DeviceID  string
	SessionID string
	// ClientVersion / ClientBuild 为空时用默认值。
	ClientVersion string
	ClientBuild   string
}

// NewClient 构造上游客户端。
func NewClient(cfg ClientConfig) (*Client, error) {
	client, profile, err := NewHTTPClient(ClientOptions{
		Impersonate: cfg.Impersonate,
		ProxyURL:    cfg.ProxyURL,
		SkipVerify:  cfg.SkipVerify,
		// 客户端级超时设 0（不限制），由每次请求的 context 控制。
		//
		// 早期实现固定 60s：tls-client 的 WithTimeoutSeconds 覆盖**含 body 读取**
		// 的整段请求，于是图片生成（/f/conversation 的 SSE 流，Python 用 300s）
		// 与任何超过 60s 的流都会被硬切断，表现为 context deadline exceeded。
		// 各请求现在显式带自己的超时（见 doGet/doPostJSON 的 timeout 参数）。
		TimeoutSec: 0,
	})
	if err != nil {
		return nil, err
	}

	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = profile.UserAgent
	}
	deviceID := strings.TrimSpace(cfg.DeviceID)
	if deviceID == "" {
		deviceID = newUUID()
	}
	sessionID := strings.TrimSpace(cfg.SessionID)
	if sessionID == "" {
		sessionID = newUUID()
	}
	clientVersion := strings.TrimSpace(cfg.ClientVersion)
	if clientVersion == "" {
		clientVersion = DefaultClientVersion
	}
	clientBuild := strings.TrimSpace(cfg.ClientBuild)
	if clientBuild == "" {
		clientBuild = DefaultClientBuild
	}

	return &Client{
		http:          client,
		profile:       profile,
		accessToken:   cfg.AccessToken,
		userAgent:     userAgent,
		deviceID:      deviceID,
		sessionID:     sessionID,
		clientVersion: clientVersion,
		clientBuild:   clientBuild,
	}, nil
}

// Close 释放连接。
func (c *Client) Close() {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// headers 构造带 target path 的请求头。
func (c *Client) headers(path string, extra map[string]string) http.Header {
	merged := map[string]string{
		"X-OpenAI-Target-Path":  path,
		"X-OpenAI-Target-Route": path,
	}
	for name, value := range extra {
		merged[name] = value
	}
	return BuildHeaders(c.profile, c.deviceID, c.sessionID, c.clientVersion, c.clientBuild, c.accessToken, merged)
}

// Bootstrap 预热首页并提取 PoW 脚本引用。
//
// 不调用它也能工作（powScriptSources 会退回默认值），但脚本引用与
// data_build 是 PoW token 的一部分，缺了会降低通过率。
func (c *Client) Bootstrap(ctx context.Context) error {
	if c.bootstrapped {
		return nil
	}
	headers := http.Header{}
	setHeader(headers, "user-agent", c.userAgent)
	setHeader(headers, "accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	setHeader(headers, "accept-language", "zh-CN,zh;q=0.9,en;q=0.8")
	setHeader(headers, "sec-ch-ua", c.profile.SecChUa)
	setHeader(headers, "sec-ch-ua-mobile", "?0")
	setHeader(headers, "sec-ch-ua-platform", `"Windows"`)
	setHeader(headers, "sec-fetch-dest", "document")
	setHeader(headers, "sec-fetch-mode", "navigate")
	setHeader(headers, "sec-fetch-site", "none")
	setHeader(headers, "sec-fetch-user", "?1")
	setHeader(headers, "upgrade-insecure-requests", "1")
	headers[http.HeaderOrderKey] = []string{
		"user-agent", "accept", "accept-language", "sec-ch-ua",
		"sec-ch-ua-mobile", "sec-ch-ua-platform", "sec-fetch-dest",
		"sec-fetch-mode", "sec-fetch-site", "sec-fetch-user",
		"upgrade-insecure-requests",
	}

	body, status, err := c.do(ctx, http.MethodGet, BaseURL+"/", headers, nil, 30*time.Second)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if status >= 400 {
		return fmt.Errorf("bootstrap failed: status=%d body=%s", status, truncate(string(body), 200))
	}

	sources, dataBuild := parsePowResources(string(body))
	c.powScriptSources = sources
	c.powDataBuild = dataBuild
	c.bootstrapped = true
	return nil
}

var (
	scriptSrcRe = regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
	buildRe     = regexp.MustCompile(`c/[^/]*/_`)
	htmlBuildRe = regexp.MustCompile(`<html[^>]*data-build="([^"]*)"`)
)

// parsePowResources 复刻 utils/pow.py 的 parse_pow_resources。
func parsePowResources(html string) ([]string, string) {
	sources := make([]string, 0, 8)
	dataBuild := ""
	for _, match := range scriptSrcRe.FindAllStringSubmatch(html, -1) {
		src := match[1]
		if strings.TrimSpace(src) == "" {
			continue
		}
		sources = append(sources, src)
		if dataBuild == "" {
			if buildMatch := buildRe.FindString(src); buildMatch != "" {
				dataBuild = buildMatch
			}
		}
	}
	if len(sources) == 0 {
		sources = []string{DefaultPowScript}
	}
	if dataBuild == "" {
		if match := htmlBuildRe.FindStringSubmatch(html); len(match) > 1 {
			dataBuild = match[1]
		}
	}
	return sources, dataBuild
}

// ChatRequirements 是 protocol.Upstream 要求的签名（返回 protocol.Requirements）。
// 内部实现见 chatRequirements。
func (c *Client) ChatRequirements(ctx context.Context) (protocol.Requirements, error) {
	req, err := c.chatRequirements(ctx)
	if err != nil {
		return protocol.Requirements{}, err
	}
	return protocol.Requirements{
		Token:          req.Token,
		ProofToken:     req.ProofToken,
		TurnstileToken: req.TurnstileToken,
		SOToken:        req.SOToken,
		Raw:            req.Raw,
	}, nil
}

// chatRequirements 复刻 _get_chat_requirements 的 prepare + finalize 两步流程。
func (c *Client) chatRequirements(ctx context.Context) (Requirements, error) {
	base := "/backend-anon/sentinel/chat-requirements"
	if c.accessToken != "" {
		base = "/backend-api/sentinel/chat-requirements"
	}
	pToken := BuildLegacyRequirementsToken(c.userAgent, c.powScriptSources, c.powDataBuild)

	preparePath := base + "/prepare"
	prepareBody, err := json.Marshal(map[string]any{"p": pToken})
	if err != nil {
		return Requirements{}, err
	}
	prepareResp, status, err := c.doJSON(ctx, http.MethodPost, BaseURL+preparePath, preparePath, prepareBody, 30*time.Second)
	if err != nil {
		return Requirements{}, fmt.Errorf("chat_requirements_prepare: %w", err)
	}
	if status >= 400 {
		return Requirements{}, fmt.Errorf("chat_requirements_prepare failed: status=%d body=%s",
			status, truncate(string(prepareResp), 300))
	}

	var prepareData map[string]any
	if err := json.Unmarshal(prepareResp, &prepareData); err != nil {
		return Requirements{}, fmt.Errorf("chat_requirements_prepare: invalid json: %w", err)
	}
	if arkose, ok := prepareData["arkose"].(map[string]any); ok && truthy(arkose["required"]) {
		return Requirements{}, fmt.Errorf("chat requirements requires arkose token, which is not implemented")
	}

	proofToken := ""
	if proof, ok := prepareData["proofofwork"].(map[string]any); ok && truthy(proof["required"]) {
		solved, err := BuildProofToken(
			stringValue(proof["seed"]),
			stringValue(proof["difficulty"]),
			c.userAgent,
			c.powScriptSources,
			c.powDataBuild,
		)
		if err != nil {
			return Requirements{}, err
		}
		proofToken = solved
	}

	turnstileToken := ""
	if turnstile, ok := prepareData["turnstile"].(map[string]any); ok &&
		truthy(turnstile["required"]) && stringValue(turnstile["dx"]) != "" {
		turnstileToken = SolveTurnstileToken(stringValue(turnstile["dx"]), pToken)
	}

	finalizePath := base + "/finalize"
	finalizeBody, err := json.Marshal(map[string]any{
		"prepare_token":   stringValue(prepareData["prepare_token"]),
		"proof_token":     proofToken,
		"turnstile_token": turnstileToken,
	})
	if err != nil {
		return Requirements{}, err
	}
	finalizeResp, status, err := c.doJSON(ctx, http.MethodPost, BaseURL+finalizePath, finalizePath, finalizeBody, 30*time.Second)
	if err != nil {
		return Requirements{}, fmt.Errorf("chat_requirements_finalize: %w", err)
	}
	if status >= 400 {
		return Requirements{}, fmt.Errorf("chat_requirements_finalize failed: status=%d body=%s",
			status, truncate(string(finalizeResp), 300))
	}

	var finalizeData map[string]any
	if err := json.Unmarshal(finalizeResp, &finalizeData); err != nil {
		return Requirements{}, fmt.Errorf("chat_requirements_finalize: invalid json: %w", err)
	}
	token := stringValue(finalizeData["token"])
	if token == "" {
		message := "missing chat requirements token"
		if c.accessToken != "" {
			message = "missing auth chat requirements token"
		}
		return Requirements{}, fmt.Errorf("%s: %s", message, truncate(string(finalizeResp), 300))
	}

	return Requirements{
		Token:          token,
		ProofToken:     proofToken,
		TurnstileToken: turnstileToken,
		SOToken:        stringValue(finalizeData["so_token"]),
		Raw:            finalizeData,
	}, nil
}

// doJSON 发一个 JSON POST，返回 body 与状态码。
func (c *Client) doJSON(ctx context.Context, method, url, targetPath string, body []byte, timeout time.Duration) ([]byte, int, error) {
	headers := c.headers(targetPath, map[string]string{"Content-Type": "application/json"})
	return c.do(ctx, method, url, headers, body, timeout)
}

// do 是唯一的出站入口：所有请求都经过它，确保指纹头一致。
func (c *Client) do(ctx context.Context, method, url string, headers http.Header, body []byte, timeout time.Duration) ([]byte, int, error) {
	// timeout 必须真正生效：客户端级超时已设为 0（见 NewClient），
	// 若这里也不加 deadline，长流会无限挂起。
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header = headers

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return payload, resp.StatusCode, nil
}

func setHeader(headers http.Header, name, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	headers[strings.ToLower(name)] = []string{value}
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		lowered := strings.ToLower(strings.TrimSpace(typed))
		return lowered == "1" || lowered == "true" || lowered == "yes" || lowered == "on"
	case float64:
		return typed != 0
	case int:
		return typed != 0
	default:
		return false
	}
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// conversationHeaders 复刻 _conversation_headers。
func (c *Client) conversationHeaders(path string, requirements Requirements) http.Header {
	extra := map[string]string{
		"Accept":       "text/event-stream",
		"Content-Type": "application/json",
		"OpenAI-Sentinel-Chat-Requirements-Token": requirements.Token,
	}
	if requirements.ProofToken != "" {
		extra["OpenAI-Sentinel-Proof-Token"] = requirements.ProofToken
	}
	if requirements.TurnstileToken != "" {
		extra["OpenAI-Sentinel-Turnstile-Token"] = requirements.TurnstileToken
	}
	if requirements.SOToken != "" {
		extra["OpenAI-Sentinel-SO-Token"] = requirements.SOToken
	}
	return c.headers(path, extra)
}
