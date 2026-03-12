package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	imap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	gomail "github.com/emersion/go-message/mail"

	tls_client "github.com/bogdanfinn/tls-client"
	tls_profiles "github.com/bogdanfinn/tls-client/profiles"
)

// ═══════════════════════════════════════════════════════
// 常量
// ═══════════════════════════════════════════════════════
const (
	defaultRegisterPassword = "Qwer1234!Aa#"

	OAIClientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	OAIAuthURL           = "https://auth.openai.com/oauth/authorize"
	OAITokenURL          = "https://auth.openai.com/oauth/token"
	OAISentinelURL       = "https://sentinel.openai.com/backend-api/sentinel/req"
	OAISignupURL         = "https://auth.openai.com/api/accounts/authorize/continue"
	OAIUserRegisterURL   = "https://auth.openai.com/api/accounts/user/register"
	OAISendOTPURL        = "https://auth.openai.com/api/accounts/passwordless/send-otp"
	OAIEmailOTPResendURL = "https://auth.openai.com/api/accounts/email-otp/resend"
	OAIVerifyURL         = "https://auth.openai.com/api/accounts/email-otp/validate"
	OAICreateURL         = "https://auth.openai.com/api/accounts/create_account"
	OAIWorkURL           = "https://auth.openai.com/api/accounts/workspace/select"

	LocalPort        = 1455
	LocalRedirectURI = "http://localhost:1455/auth/callback"

	PollTimeout    = 180 * time.Second
	ResendInterval = 25 * time.Second
	MaxRetry       = 2
)

// ═══════════════════════════════════════════════════════
// 数据结构
// ═══════════════════════════════════════════════════════
type Account struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	ClientID     string `json:"client_id,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type DomainMailConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user,omitempty"`
	Pass     string `json:"pass,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	UseTLS   bool   `json:"use_tls,omitempty"`
}

func (c *DomainMailConfig) IMAPUser() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmpty(c.User, c.Username))
}

func (c *DomainMailConfig) IMAPPass() string {
	if c == nil {
		return ""
	}
	return firstNonEmpty(c.Pass, c.Password)
}

type StartRequest struct {
	Accounts     string            `json:"accounts"`
	Proxy        string            `json:"proxy"`
	Workers      int               `json:"workers"`
	LoginMode    bool              `json:"login_mode"`
	SkipFinished bool              `json:"skip_finished"`
	DomainMail   *DomainMailConfig `json:"domain_mail,omitempty"`
	TempMail     *TempMailConfig   `json:"temp_mail,omitempty"`
}

type RegResult struct {
	Email        string `json:"email"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	ExpiresAt    string `json:"expires_at"`
	RegisteredAt string `json:"registered_at"`
	Mode         string `json:"mode"`
}

// SSE event
type SSEEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// ═══════════════════════════════════════════════════════
// 全局状态
// ═══════════════════════════════════════════════════════
var (
	gRunning   atomic.Bool
	gStopFlag  atomic.Bool
	gTotal     atomic.Int32
	gSuccess   atomic.Int32
	gFail      atomic.Int32
	gStartTime time.Time

	sseClients     = make(map[chan []byte]struct{})
	sseClientsLock sync.Mutex

	resultsDir string
)

// ═══════════════════════════════════════════════════════
// 姓名/生日
// ═══════════════════════════════════════════════════════
var givenNames = []string{
	"Liam", "Noah", "Oliver", "James", "Elijah", "William", "Henry", "Lucas",
	"Benjamin", "Theodore", "Jack", "Levi", "Alexander", "Mason", "Ethan",
	"Olivia", "Emma", "Charlotte", "Amelia", "Sophia", "Isabella", "Mia",
	"Evelyn", "Harper", "Luna", "Camila", "Sofia", "Scarlett", "Elizabeth",
}

var familyNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Miller", "Davis",
	"Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin",
	"Lee", "Thompson", "White", "Harris", "Clark", "Lewis", "Robinson",
}

func randomName() string {
	return givenNames[rand.Intn(len(givenNames))] + " " + familyNames[rand.Intn(len(familyNames))]
}

func randomBirthday() string {
	y := 1986 + rand.Intn(21)
	m := 1 + rand.Intn(12)
	d := 1 + rand.Intn(28)
	return fmt.Sprintf("%d-%02d-%02d", y, m, d)
}

// ═══════════════════════════════════════════════════════
// PKCE
// ═══════════════════════════════════════════════════════
func urlsafeB64(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func createPKCE() (verifier, challenge string) {
	b := make([]byte, 48)
	rand.Read(b)
	verifier = urlsafeB64(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = urlsafeB64(h[:])
	return
}

func createOAuthParams() (authURL, state, verifier string) {
	verifier, challenge := createPKCE()
	b := make([]byte, 16)
	rand.Read(b)
	state = urlsafeB64(b)
	q := url.Values{
		"client_id":                  {OAIClientID},
		"response_type":              {"code"},
		"redirect_uri":               {LocalRedirectURI},
		"scope":                      {"openid email profile offline_access"},
		"state":                      {state},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	authURL = OAIAuthURL + "?" + q.Encode()
	return
}

func decodeJWTPayload(token string) map[string]interface{} {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	for len(payload)%4 != 0 {
		payload += "="
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	json.Unmarshal(raw, &m)
	return m
}

// ═══════════════════════════════════════════════════════
// TLS 指纹 HTTP 客户端
// ═══════════════════════════════════════════════════════
var tlsProfiles = []tls_profiles.ClientProfile{
	tls_profiles.Chrome_131,
	tls_profiles.Chrome_131_PSK,
	tls_profiles.Chrome_124,
	tls_profiles.Chrome_120,
}

var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
}

var acceptLanguages = []string{
	"en-US,en;q=0.9",
	"en-US,en;q=0.9,zh-CN;q=0.8",
	"en-GB,en;q=0.9,en-US;q=0.8",
}

type HTTPClient struct {
	client    tls_client.HttpClient
	cookies   map[string]string
	profile   string
	userAgent string
}

func NewHTTPClient(proxy string) (*HTTPClient, error) {
	profile := tlsProfiles[rand.Intn(len(tlsProfiles))]
	ua := userAgents[rand.Intn(len(userAgents))]

	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(profile),
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithCookieJar(jar),
		tls_client.WithRandomTLSExtensionOrder(),
	}
	if proxy != "" {
		options = append(options, tls_client.WithProxyUrl(proxy))
	}

	c, err := tls_client.NewHttpClient(nil, options...)
	if err != nil {
		return nil, err
	}

	return &HTTPClient{
		client:    c,
		cookies:   make(map[string]string),
		profile:   fmt.Sprintf("%v", profile),
		userAgent: ua,
	}, nil
}

// Chrome 请求头顺序（Cloudflare 检查这个）
var chromeGetHeaderOrder = []string{
	"host", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
	"upgrade-insecure-requests", "user-agent", "accept",
	"sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest",
	"accept-encoding", "accept-language",
}

var chromePostHeaderOrder = []string{
	"host", "content-length", "sec-ch-ua", "sec-ch-ua-mobile",
	"sec-ch-ua-platform", "content-type", "user-agent", "accept",
	"origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
	"referer", "accept-encoding", "accept-language",
}

func (h *HTTPClient) setGetHeaders(req *fhttp.Request) {
	req.Header = fhttp.Header{
		"sec-ch-ua":                 {`"Chromium";v="131", "Not_A Brand";v="24"`},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Windows"`},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {h.userAgent},
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"},
		"sec-fetch-site":            {"none"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-user":            {"?1"},
		"sec-fetch-dest":            {"document"},
		"accept-encoding":           {"gzip, deflate, br, zstd"},
		"accept-language":           {acceptLanguages[rand.Intn(len(acceptLanguages))]},
		"dnt":                       {"1"},
		fhttp.HeaderOrderKey:        chromeGetHeaderOrder,
	}
}

func (h *HTTPClient) setPostHeaders(req *fhttp.Request, contentType string, extraHeaders map[string]string) {
	req.Header = fhttp.Header{
		"sec-ch-ua":          {`"Chromium";v="131", "Not_A Brand";v="24"`},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"Windows"`},
		"content-type":       {contentType},
		"user-agent":         {h.userAgent},
		"accept":             {"application/json"},
		"origin":             {"https://auth.openai.com"},
		"sec-fetch-site":     {"same-origin"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-dest":     {"empty"},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
		"accept-language":    {acceptLanguages[rand.Intn(len(acceptLanguages))]},
		"dnt":                {"1"},
		fhttp.HeaderOrderKey: chromePostHeaderOrder,
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
}

func (h *HTTPClient) Get(rawURL string) (int, string, error) {
	req, _ := fhttp.NewRequest("GET", rawURL, nil)
	h.setGetHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	h.saveCookies(resp)
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

func (h *HTTPClient) PostJSON(rawURL string, data interface{}, extraHeaders map[string]string) (int, string, error) {
	b, _ := json.Marshal(data)
	req, _ := fhttp.NewRequest("POST", rawURL, strings.NewReader(string(b)))
	h.setPostHeaders(req, "application/json", extraHeaders)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	h.saveCookies(resp)
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

func (h *HTTPClient) PostForm(rawURL string, data url.Values) (int, string, error) {
	req, _ := fhttp.NewRequest("POST", rawURL, strings.NewReader(data.Encode()))
	h.setPostHeaders(req, "application/x-www-form-urlencoded", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	h.saveCookies(resp)
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

func (h *HTTPClient) FollowRedirects(startURL string, maxHops int) (string, error) {
	h.client.SetFollowRedirect(false)
	defer h.client.SetFollowRedirect(true)

	u := startURL
	for i := 0; i < maxHops; i++ {
		req, _ := fhttp.NewRequest("GET", u, nil)
		h.setGetHeaders(req)
		resp, err := h.client.Do(req)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		h.saveCookies(resp)
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", fmt.Errorf("no Location header at hop %d (status %d)", i, resp.StatusCode)
		}
		if strings.Contains(loc, "localhost") && strings.Contains(loc, "/auth/callback") {
			return loc, nil
		}
		u = loc
	}
	return "", fmt.Errorf("too many redirects")
}

func (h *HTTPClient) GetCookie(name string) string {
	return h.cookies[name]
}

func (h *HTTPClient) saveCookies(resp *fhttp.Response) {
	for _, c := range resp.Cookies() {
		h.cookies[c.Name] = c.Value
	}
}

// ═══════════════════════════════════════════════════════
// 注册流程 (核心)
// ═══════════════════════════════════════════════════════
func registerAccount(acc Account, proxy string, mode string, domainMail *DomainMailConfig, tempMail *TempMailConfig) (*RegResult, error) {
	email := strings.TrimSpace(acc.Email)
	tempMode := tempMail != nil
	if strings.HasSuffix(strings.ToLower(email), "@placeholder.local") {
		if err := ensureTempMailReady(); err != nil {
			return nil, fmt.Errorf("Temp Mail 初始化失败: %w", err)
		}
		mailbox, err := acquireTempMailbox()
		if err != nil {
			return nil, fmt.Errorf("Temp Mail 获取邮箱失败: %w", err)
		}
		email = mailbox
		acc.Email = mailbox
		broadcast(fmt.Sprintf("  🧪 Temp Mail 分配邮箱: %s", mailbox), "info")
	}
	isLogin := mode == "login"
	modeLabel := "注册"
	if isLogin {
		modeLabel = "登录"
	}

	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}

	httpClient, err := NewHTTPClient(proxy)
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 客户端失败: %w", err)
	}
	broadcast(fmt.Sprintf("  🎭 浏览器指纹: %s", httpClient.profile), "dim")

	// --- Step 1: OAuth ---
	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}
	authURL, state, verifier := createOAuthParams()
	broadcast(fmt.Sprintf("  [1] 发起 OAuth (%s)...", modeLabel), "info")
	status, _, err := httpClient.Get(authURL)
	if err != nil {
		return nil, fmt.Errorf("OAuth 失败: %w", err)
	}
	broadcast(fmt.Sprintf("      状态: %d", status), "dim")

	deviceID := httpClient.GetCookie("oai-did")
	if deviceID != "" {
		broadcast(fmt.Sprintf("      设备ID: %s...", deviceID[:min(16, len(deviceID))]), "dim")
	}
	sleepFlow(tempMode, 800, 2000)

	// --- Step 2: Sentinel ---
	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}
	broadcast("  [2] 获取 Sentinel token...", "info")
	sentinelBody := map[string]interface{}{"p": "", "id": deviceID, "flow": "authorize_continue"}
	sStatus, sBody, err := httpClient.PostJSON(OAISentinelURL, sentinelBody, map[string]string{
		"Origin":  "https://sentinel.openai.com",
		"Referer": "https://sentinel.openai.com/backend-api/sentinel/frame.html",
	})
	if err != nil || sStatus < 200 || sStatus >= 300 {
		return nil, fmt.Errorf("Sentinel 失败: %d %s", sStatus, truncate(sBody, 200))
	}
	var sentinelResp map[string]interface{}
	json.Unmarshal([]byte(sBody), &sentinelResp)
	sentinelToken, _ := sentinelResp["token"].(string)
	sentinelHeader, _ := json.Marshal(map[string]interface{}{
		"p": "", "t": "", "c": sentinelToken, "id": deviceID, "flow": "authorize_continue",
	})
	broadcast("      OK", "dim")
	sleepFlow(tempMode, 500, 1500)

	// 对应 Python: otp_sent_at = time.time() 在提交邮箱之前
	// 已注册账号在步骤3提交邮箱时自动发送OTP，所以需要在步骤3之前记录时间
	otpSentAt := time.Now()

	// --- Step 3: Submit email ---
	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}
	broadcast(fmt.Sprintf("  [3] 提交邮箱: %s (%s)", email, modeLabel), "info")
	signupBody := map[string]interface{}{
		"username":    map[string]interface{}{"value": email, "kind": "email"},
		"screen_hint": "signup",
	}
	s3Status, s3Body, err := httpClient.PostJSON(OAISignupURL, signupBody, map[string]string{
		"Referer":               "https://auth.openai.com/create-account",
		"openai-sentinel-token": string(sentinelHeader),
	})
	if err != nil || s3Status < 200 || s3Status >= 300 {
		return nil, fmt.Errorf("提交邮箱失败: %d %s", s3Status, truncate(s3Body, 300))
	}
	broadcast("      OK", "dim")

	var step3Data map[string]interface{}
	json.Unmarshal([]byte(s3Body), &step3Data)
	pageType := extractPageType(step3Data)
	step3ContinueURL := strFromMap(step3Data, "continue_url")
	broadcast(fmt.Sprintf("      页面类型: %s", pageType), "dim")
	sleepFlow(tempMode, 500, 1500)

	isExisting := pageType == "email_otp_verification"
	otpResendMode := ""

	// --- Step 4: Send OTP ---
	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}
	switch pageType {
	case "create_account_password":
		if step3ContinueURL != "" {
			status, _, err := httpClient.Get(step3ContinueURL)
			if err != nil {
				return nil, fmt.Errorf("访问密码注册页失败: %w", err)
			}
			if status < 200 || status >= 400 {
				return nil, fmt.Errorf("访问密码注册页失败: %d", status)
			}
			sleepFlow(tempMode, 300, 900)
		}

		password, err := normalizeRegisterPassword(acc.Password)
		if err != nil {
			return nil, err
		}

		broadcast("  [4] 提交注册密码...", "info")
		r4Status, r4Body, err := httpClient.PostJSON(OAIUserRegisterURL, map[string]interface{}{
			"username": email,
			"password": password,
		}, map[string]string{
			"Referer": "https://auth.openai.com/create-account/password",
		})
		if err != nil {
			return nil, fmt.Errorf("提交注册密码失败: %w", err)
		}
		if r4Status < 200 || r4Status >= 300 {
			return nil, fmt.Errorf("提交注册密码失败: %d %s", r4Status, truncate(r4Body, 300))
		}

		var step4Data map[string]interface{}
		json.Unmarshal([]byte(r4Body), &step4Data)
		pageType = extractPageType(step4Data)
		broadcast("      OK", "dim")
		broadcast(fmt.Sprintf("      下一页面: %s", pageType), "dim")

		if nextURL := strFromMap(step4Data, "continue_url"); nextURL != "" {
			status, _, err := httpClient.Get(nextURL)
			if err != nil {
				return nil, fmt.Errorf("访问下一注册页失败: %w", err)
			}
			if status < 200 || status >= 400 {
				return nil, fmt.Errorf("访问下一注册页失败: %d", status)
			}
		}

		if pageType == "email_otp_send" || pageType == "email_otp_verification" {
			otpResendMode = "email_otp"
			otpSentAt = time.Now()
		}
		sleepFlow(tempMode, 500, 1200)
	case "email_otp_verification":
		broadcast("  [4] 跳过发送 OTP（服务器已自动发送）", "info")
		otpResendMode = "email_otp"
	default:
		broadcast("  [4] 发送 OTP...", "info")
		o4Status, o4Body, err := httpClient.PostJSON(OAISendOTPURL, map[string]interface{}{}, map[string]string{
			"Referer": "https://auth.openai.com/create-account/password",
		})
		if err != nil {
			return nil, fmt.Errorf("发送 OTP 失败: %w", err)
		}
		if o4Status < 200 || o4Status >= 300 {
			return nil, fmt.Errorf("发送 OTP 失败: %d %s", o4Status, truncate(o4Body, 300))
		} else {
			broadcast(fmt.Sprintf("      OK，验证码已发送到 %s", email), "dim")
			otpSentAt = time.Now()
			otpResendMode = "passwordless"
		}
	}

	// --- Step 5: Get code ---
	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}
	codeSource := "Outlook IMAP"
	if tempMail != nil {
		codeSource = "Temp Mail"
	} else if domainMail != nil {
		codeSource = "集成 IMAP"
	}
	broadcast(fmt.Sprintf("    📧 等待验证码 (%s, %s)...", email, codeSource), "info")

	resendFn := func() bool {
		switch otpResendMode {
		case "email_otp":
			s, _, _ := httpClient.PostJSON(OAIEmailOTPResendURL, map[string]interface{}{}, map[string]string{
				"Referer": "https://auth.openai.com/email-verification",
			})
			return s >= 200 && s < 300
		case "passwordless":
			s, _, _ := httpClient.PostJSON(OAISendOTPURL, map[string]interface{}{}, map[string]string{
				"Referer": "https://auth.openai.com/email-verification",
			})
			return s >= 200 && s < 300
		default:
			return false
		}
	}

	if !isExisting && otpResendMode == "" {
		return nil, fmt.Errorf("当前注册流未进入邮箱验证码阶段: %s", pageType)
	}

	var code string
	if tempMail != nil {
		code, err = waitForTempMailCode(email, otpSentAt, resendFn)
	} else if domainMail != nil {
		// 域名邮箱模式：使用集成 IMAP 服务
		code, err = waitForCode(email, otpSentAt, resendFn)
	} else {
		// Outlook 模式：用账号自己的邮箱 IMAP
		code, err = fetchOutlookCode(acc, otpSentAt, resendFn)
	}
	if err != nil {
		return nil, err
	}

	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}
	sleepFlow(tempMode, 300, 1000)

	// --- Step 6: Verify OTP ---
	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}
	broadcast(fmt.Sprintf("  [6] 验证 OTP: %s", code), "info")
	v6Status, v6Body, err := httpClient.PostJSON(OAIVerifyURL, map[string]interface{}{"code": code}, map[string]string{
		"Referer": "https://auth.openai.com/email-verification",
	})
	if err != nil || v6Status < 200 || v6Status >= 300 {
		return nil, fmt.Errorf("OTP 验证失败: %d %s", v6Status, truncate(v6Body, 300))
	}
	broadcast("      OK", "dim")
	sleepFlow(tempMode, 500, 1500)

	// --- Step 7: Create account ---
	name := ""
	if gStopFlag.Load() {
		return nil, fmt.Errorf("已取消")
	}
	if isExisting || isLogin {
		broadcast("  [7] 跳过（账号已存在）", "info")
	} else {
		name = randomName()
		birthday := randomBirthday()
		broadcast(fmt.Sprintf("  [7] 创建账号: %s, %s", name, birthday), "info")
		c7Status, c7Body, err := httpClient.PostJSON(OAICreateURL, map[string]interface{}{
			"name": name, "birthdate": birthday,
		}, map[string]string{"Referer": "https://auth.openai.com/about-you"})
		if err != nil || c7Status < 200 || c7Status >= 300 {
			return nil, fmt.Errorf("创建账号失败: %d %s", c7Status, truncate(c7Body, 300))
		}
		broadcast("      OK", "dim")
		sleepFlow(tempMode, 500, 1500)
	}

	// --- Step 8: Select workspace ---
	authCookie := httpClient.GetCookie("oai-client-auth-session")
	if authCookie == "" {
		return nil, fmt.Errorf("未获取到 oai-client-auth-session cookie")
	}

	parts := strings.Split(authCookie, ".")
	cookieB64 := parts[0]
	for len(cookieB64)%4 != 0 {
		cookieB64 += "="
	}
	cookieRaw, err := base64.StdEncoding.DecodeString(cookieB64)
	if err != nil {
		return nil, fmt.Errorf("解析 cookie 失败: %w", err)
	}

	var cookieData map[string]interface{}
	json.Unmarshal(cookieRaw, &cookieData)
	workspaces, _ := cookieData["workspaces"].([]interface{})
	if len(workspaces) == 0 {
		return nil, fmt.Errorf("未找到 workspace")
	}
	ws0, _ := workspaces[0].(map[string]interface{})
	workspaceID, _ := ws0["id"].(string)
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace_id 为空")
	}

	broadcast(fmt.Sprintf("  [8] 选择 Workspace: %s...", workspaceID[:min(20, len(workspaceID))]), "info")
	w8Status, w8Body, err := httpClient.PostJSON(OAIWorkURL, map[string]interface{}{
		"workspace_id": workspaceID,
	}, map[string]string{"Referer": "https://auth.openai.com/sign-in-with-chatgpt/codex/consent"})
	if err != nil || w8Status < 200 || w8Status >= 300 {
		return nil, fmt.Errorf("选择 workspace 失败: %d %s", w8Status, truncate(w8Body, 300))
	}
	var w8Data map[string]interface{}
	json.Unmarshal([]byte(w8Body), &w8Data)
	continueURL, _ := w8Data["continue_url"].(string)
	if continueURL == "" {
		return nil, fmt.Errorf("未获取到 continue_url")
	}

	// --- Step 9: Follow redirects ---
	broadcast("  [9] 跟随重定向获取 Token...", "info")
	callbackURL, err := httpClient.FollowRedirects(continueURL, 12)
	if err != nil {
		return nil, fmt.Errorf("重定向失败: %w", err)
	}

	parsed, _ := url.Parse(callbackURL)
	authCode := parsed.Query().Get("code")
	returnedState := parsed.Query().Get("state")
	if authCode == "" {
		return nil, fmt.Errorf("回调缺少 code")
	}
	if returnedState != state {
		return nil, fmt.Errorf("state 不匹配")
	}

	// Exchange token
	tStatus, tBody, err := httpClient.PostForm(OAITokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {OAIClientID},
		"code":          {authCode},
		"redirect_uri":  {LocalRedirectURI},
		"code_verifier": {verifier},
	})
	if err != nil || tStatus < 200 || tStatus >= 300 {
		return nil, fmt.Errorf("Token 兑换失败: %d %s", tStatus, truncate(tBody, 300))
	}

	var tokenData map[string]interface{}
	json.Unmarshal([]byte(tBody), &tokenData)

	claims := decodeJWTPayload(strVal(tokenData, "id_token"))
	authClaims, _ := claims["https://api.openai.com/auth"].(map[string]interface{})

	now := time.Now()
	expiresIn := intVal(tokenData, "expires_in")

	result := &RegResult{
		Email:        email,
		Type:         "codex",
		Name:         firstNonEmpty(name, strFromMap(claims, "name")),
		AccessToken:  strVal(tokenData, "access_token"),
		RefreshToken: strVal(tokenData, "refresh_token"),
		IDToken:      strVal(tokenData, "id_token"),
		AccountID:    strFromMap(authClaims, "chatgpt_account_id"),
		ExpiresAt:    now.Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339),
		RegisteredAt: now.UTC().Format(time.RFC3339),
		Mode:         mode,
	}

	broadcast(fmt.Sprintf("  🎉 %s成功！", modeLabel), "success")
	return result, nil
}

func waitForCode(email string, otpSentAt time.Time, resendFn func() bool) (string, error) {
	// 清除旧验证码，避免二次注册时复用上一次的旧码
	integratedIMAP.ConsumeCode(strings.ToLower(email), "")

	// 将 otpSentAt 前 60 秒作为最早接受时间（留出时钟偏差容差，对应 Python min_ts = otp_sent_at - 60）
	minTime := otpSentAt.Add(-60 * time.Second)

	// 启动后台重发 goroutine
	done := make(chan struct{})
	defer close(done)

	go func() {
		// 首次重发在 20 秒后
		select {
		case <-time.After(20 * time.Second):
		case <-done:
			return
		}
		if resendFn != nil {
			if resendFn() {
				broadcast("    🔄 已重发 OTP", "info")
			}
		}
		// 此后每 25 秒重发
		ticker := time.NewTicker(ResendInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if resendFn != nil {
					if resendFn() {
						broadcast("    🔄 已重发 OTP", "info")
					}
				}
			case <-done:
				return
			}
		}
	}()

	// 使用集成 IMAP 服务等待新验证码，仅接受 minTime 之后的邮件
	manualCodeHint(email)
	code, err := WaitVerificationCode(email, PollTimeout, minTime)
	if err != nil {
		return "", err
	}
	if code == "" {
		return "", fmt.Errorf("empty verification code for %s", email)
	}
	broadcast(fmt.Sprintf("    ✅ 验证码: %s (集成 IMAP)", code), "success")
	return code, nil
}

// ═══════════════════════════════════════════════════════
// Outlook 账号 IMAP 验证码获取
// ═══════════════════════════════════════════════════════
func fetchOutlookCode(acc Account, otpSentAt time.Time, resendFn func() bool) (string, error) {
	const (
		outlookHost    = "outlook.office365.com"
		outlookPort    = 993
		pollInterval   = 4 * time.Second
		firstResend    = 20 * time.Second
		resendInterval = 25 * time.Second
	)

	start := time.Now()
	lastResend := time.Duration(0)
	minTime := otpSentAt.Add(-60 * time.Second)

	var c *imapclient.Client
	connect := func() error {
		if c != nil {
			c.Logout()
			c = nil
		}
		addr := fmt.Sprintf("%s:%d", outlookHost, outlookPort)
		var err error
		c, err = imapclient.DialTLS(addr, nil)
		if err != nil {
			return fmt.Errorf("IMAP 连接失败: %w", err)
		}
		// 优先 XOAUTH2（对应 Python OutlookIMAP.connect()）
		if acc.ClientID != "" && acc.RefreshToken != "" {
			token, tokenErr := refreshMSToken(acc.Email, acc.ClientID, acc.RefreshToken)
			if tokenErr == nil {
				xoauth2 := buildXOAuth2(acc.Email, token)
				if authErr := c.Authenticate(xoauth2Sasl(xoauth2)); authErr == nil {
					return nil
				} else {
					broadcast(fmt.Sprintf("    ⚠️ XOAUTH2 失败: %v，尝试密码", authErr), "warning")
				}
			} else {
				broadcast(fmt.Sprintf("    ⚠️ 刷新 MS Token 失败: %v，尝试密码", tokenErr), "warning")
			}
		}
		// 回退密码认证
		if loginErr := c.Login(acc.Email, acc.Password); loginErr != nil {
			c.Logout()
			c = nil
			return fmt.Errorf("IMAP 登录失败: %w", loginErr)
		}
		return nil
	}

	if err := connect(); err != nil {
		return "", err
	}
	defer func() {
		if c != nil {
			c.Logout()
		}
	}()

	seenCodes := map[string]bool{}
	subjectRe := regexp.MustCompile(`\b(\d{6})\b`)

	for time.Since(start) < PollTimeout {
		if gStopFlag.Load() {
			return "", fmt.Errorf("收到停止信号")
		}

		// 定时重发 OTP（参照 Python poll_verification_code）
		elapsed := time.Since(start)
		if resendFn != nil && elapsed > firstResend {
			if lastResend == 0 || elapsed-lastResend > resendInterval {
				if resendFn() {
					broadcast("    🔄 已重发 OTP", "info")
				}
				lastResend = elapsed
			}
		}

		// 拉最近20封邮件
		mbox, err := c.Select("INBOX", false)
		if err != nil {
			if err2 := connect(); err2 != nil {
				return "", err2
			}
			mbox, err = c.Select("INBOX", false)
			if err != nil {
				time.Sleep(pollInterval)
				continue
			}
		}

		if mbox.Messages == 0 {
			time.Sleep(pollInterval)
			continue
		}

		from := uint32(1)
		if mbox.Messages > 20 {
			from = mbox.Messages - 19
		}

		seqset := new(imap.SeqSet)
		seqset.AddRange(from, mbox.Messages)
		section := &imap.BodySectionName{}
		items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchInternalDate, section.FetchItem()}
		messages := make(chan *imap.Message, 20)
		errCh := make(chan error, 1)
		go func() { errCh <- c.Fetch(seqset, items, messages) }()

		for msg := range messages {
			// InternalDate 为零表示服务器未返回，不过滤（安全兜底）
			if !msg.InternalDate.IsZero() && msg.InternalDate.Before(minTime) {
				continue
			}
			body := msg.GetBody(section)
			if body == nil {
				continue
			}
			mr, err := gomail.CreateReader(body)
			if err != nil {
				continue
			}

			// 读取 subject
			subject, _ := mr.Header.Subject()

			// 读取 from（envelope 更可靠，对应 Python msg.get("From","")）
			fromHdr := ""
			if msg.Envelope != nil && len(msg.Envelope.From) > 0 {
				addr := msg.Envelope.From[0]
				fromHdr = addr.PersonalName + " " + addr.MailboxName + "@" + addr.HostName
			}
			if fromHdr == "" {
				fromHdr, _ = mr.Header.Text("From")
			}

			// 读取全部正文（对应 Python _extract_body）
			var sb strings.Builder
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				if _, ok := part.Header.(*gomail.InlineHeader); ok {
					b, _ := io.ReadAll(part.Body)
					sb.Write(b)
				}
			}
			bodyText := integratedHTMLTagRe.ReplaceAllString(sb.String(), " ")

			// _is_oai_mail：对应 Python：from + subject + body 三合一检查
			combined := strings.ToLower(fromHdr + " " + subject + " " + bodyText)
			isOAI := strings.Contains(combined, "openai") ||
				strings.Contains(combined, "chatgpt")
			if !isOAI {
				continue
			}

			// 优先从 Subject 提取（对应 Python subj_match）
			code := ""
			if m := subjectRe.FindStringSubmatch(subject); len(m) >= 2 {
				code = m[1]
			}
			// Subject 无结果时，从正文精确匹配（对应 Python precise）
			if code == "" {
				if m := integratedBodyCodeRegex.FindStringSubmatch(bodyText); len(m) >= 2 {
					code = m[1]
				} else if m := subjectRe.FindStringSubmatch(bodyText); len(m) >= 2 {
					code = m[1]
				}
			}

			if code != "" && !seenCodes[code] {
				seenCodes[code] = true
				broadcast(fmt.Sprintf("    ✅ 验证码: %s (Outlook IMAP)", code), "success")
				<-errCh
				return code, nil
			}
		}
		<-errCh
		time.Sleep(pollInterval)
	}

	return "", fmt.Errorf("等待 Outlook 验证码超时")

}

// refreshMSToken 刷新 Microsoft access token（对应 Python refresh_ms_token）
// 端点: https://login.live.com/oauth20_token.srf
// redirect_uri: https://login.live.com/oauth20_desktop.srf
func refreshMSToken(email, clientID, refreshToken string) (string, error) {
	vals := url.Values{
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
		"redirect_uri":  {"https://login.live.com/oauth20_desktop.srf"},
	}
	resp, err := http.PostForm("https://login.live.com/oauth20_token.srf", vals)
	if err != nil {
		return "", fmt.Errorf("MS OAuth 请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var j map[string]interface{}
	json.Unmarshal(data, &j)
	token, _ := j["access_token"].(string)
	if token == "" {
		return "", fmt.Errorf("MS OAuth 响应无 access_token: %s", string(data))
	}
	return token, nil
}

// buildXOAuth2 构造 XOAUTH2 认证串（对应 Python _build_xoauth2）
// 格式: "user=<email>\x01auth=Bearer <token>\x01\x01"
func buildXOAuth2(emailAddr, token string) []byte {
	return []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", emailAddr, token))
}

// xoauth2Sasl 实现 sasl.Client 接口，用于 IMAP AUTHENTICATE XOAUTH2
type xoauth2Client struct {
	payload []byte
}

func xoauth2Sasl(payload []byte) *xoauth2Client {
	return &xoauth2Client{payload: payload}
}

func (x *xoauth2Client) Start() (string, []byte, error) {
	return "XOAUTH2", x.payload, nil
}

func (x *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	// 服务端返回错误 JSON 时发空字符串继续
	return []byte{}, nil
}

// ═══════════════════════════════════════════════════════
// Web 服务器
// ═══════════════════════════════════════════════════════
func main() {
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	resultsDir = filepath.Join(exeDir, "tokens")

	// 如果在开发模式，用当前目录
	if _, err := os.Stat(filepath.Join(".", "web_ui.html")); err == nil {
		exeDir = "."
		resultsDir = filepath.Join(".", "tokens")
	}

	mux := http.NewServeMux()

	// 静态文件
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		htmlPath := filepath.Join(exeDir, "..", "web_ui.html")
		if _, err := os.Stat(htmlPath); os.IsNotExist(err) {
			htmlPath = filepath.Join(exeDir, "web_ui.html")
		}
		http.ServeFile(w, r, htmlPath)
	})

	// 注册集成 IMAP 服务路由
	RegisterIntegratedIMAPRoutes(mux)
	startManualCodeInput()

	// SSE 流（兼容前端 /api/stream）
	mux.HandleFunc("/api/stream", integratedIMAP.handleEvents)

	// API
	mux.HandleFunc("/api/start", handleStart)
	mux.HandleFunc("/api/stop", handleStop)
	mux.HandleFunc("/api/status", handleStatusAPI)
	mux.HandleFunc("/api/logs", handleSSE)

	addr := ":8899"
	fmt.Printf(`
╔══════════════════════════════════════════════╗
║  OpenAI 协议注册 (Go 高速版)                 ║
║                                              ║
║  🌐 http://localhost%s                    ║
║  📁 结果目录: %s
║                                              ║
║  按 Ctrl+C 退出                              ║
╚══════════════════════════════════════════════╝
`, addr, resultsDir)

	log.Fatal(http.ListenAndServe(addr, mux))
}

func normalizeTempWorkers(requested int, allowParallel bool) int {
	if !allowParallel {
		return 1
	}
	if requested < 1 {
		return 1
	}
	if requested > 50 {
		return 50
	}
	return requested
}

func isPasswordlessUnavailable(status int, body string) bool {
	lower := strings.ToLower(body)
	return status == 401 && strings.Contains(lower, "passwordless signup is unavailable")
}

func extractPageType(data map[string]interface{}) string {
	page, ok := data["page"].(map[string]interface{})
	if !ok {
		return ""
	}
	pageType, _ := page["type"].(string)
	return pageType
}

func normalizeRegisterPassword(raw string) (string, error) {
	password := strings.TrimSpace(raw)
	switch password {
	case "", "Qwer1234!":
		return defaultRegisterPassword, nil
	}
	if len([]rune(password)) < 12 {
		return "", fmt.Errorf("OpenAI 注册密码至少需要 12 位，请在 Dashboard 调整密码后重试")
	}
	return password, nil
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if gRunning.Load() {
		jsonResp(w, map[string]interface{}{"error": "已有任务运行中"})
		return
	}

	var req StartRequest
	json.NewDecoder(r.Body).Decode(&req)

	// 自动配置集成 IMAP 服务
	if req.DomainMail != nil && req.DomainMail.Host != "" {
		port := req.DomainMail.Port
		if port <= 0 {
			port = 993
		}
		user := req.DomainMail.IMAPUser()
		pass := req.DomainMail.IMAPPass()
		// 根据端口判断是否启用 TLS（993/995 通常为 TLS，143/110 不加密）
		useTLS := port == 993 || port == 995
		if err := ConfigureIntegratedIMAP(req.DomainMail.Host, port, user, pass, useTLS); err != nil {
			broadcast(fmt.Sprintf("⚠️ 集成 IMAP 配置失败: %v", err), "warning")
		} else {
			tlsLabel := map[bool]string{true: "TLS", false: "明文"}[useTLS]
			broadcast(fmt.Sprintf("📡 集成 IMAP 已配置: %s@%s:%d (%s)", user, req.DomainMail.Host, port, tlsLabel), "info")
		}
	}

	var accounts []Account
	isTempMode := req.TempMail != nil && req.TempMail.Count > 0

	if isTempMode {
		configureTempMailRuntime(req.Proxy, req.TempMail)
		count := req.TempMail.Count
		if count < 1 {
			count = 1
		}
		if count > 200 {
			count = 200
		}
		password, err := normalizeRegisterPassword(req.TempMail.Password)
		if err != nil {
			jsonResp(w, map[string]interface{}{"error": err.Error()})
			return
		}
		if strings.TrimSpace(req.TempMail.Password) != password {
			broadcast(fmt.Sprintf("⚠️ Temp Mail 密码已自动升级为兼容默认值: %s", password), "warning")
		}
		req.Workers = normalizeTempWorkers(req.Workers, req.TempMail.AllowParallel)
		for i := 0; i < count; i++ {
			accounts = append(accounts, Account{
				Email:    fmt.Sprintf("temp-mail-%d@placeholder.local", i+1),
				Password: password,
			})
		}
		if req.TempMail.AllowParallel {
			broadcast(fmt.Sprintf("🧪 Temp Mail 模式已启用: %d 个账号，并发 %d，切换延迟 %d 秒（平行开关: ON）", count, req.Workers, req.TempMail.PostSuccessDelaySeconds()), "info")
			if req.Workers > 5 {
				broadcast("⚠️ Temp Mail 并发过高更容易触发限流，建议控制在 2-5", "warning")
			}
		} else {
			broadcast(fmt.Sprintf("🧪 Temp Mail 模式已启用: %d 个账号，固定并发 1，切换延迟 %d 秒（平行开关: OFF）", count, req.TempMail.PostSuccessDelaySeconds()), "info")
		}
		if count > 1 && !req.TempMail.AllowParallel {
			broadcast("⚠️ Temp Mail 会限制短时间创建新邮箱，建议先用 1 个账号验证链路", "warning")
		}
	} else {
		// 解析账号
		for _, line := range strings.Split(req.Accounts, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Split(line, "----")
			if len(parts) < 2 {
				continue
			}
			acc := Account{Email: parts[0], Password: parts[1]}
			if len(parts) > 2 {
				acc.ClientID = parts[2]
			}
			if len(parts) > 3 {
				acc.RefreshToken = parts[3]
			}
			accounts = append(accounts, acc)
		}
	}

	if !isTempMode && len(accounts) > 0 {
		allPlaceholder := true
		for _, a := range accounts {
			if !strings.HasSuffix(strings.ToLower(strings.TrimSpace(a.Email)), "@placeholder.local") {
				allPlaceholder = false
				break
			}
		}
		if allPlaceholder {
			password, err := normalizeRegisterPassword(accounts[0].Password)
			if err != nil {
				jsonResp(w, map[string]interface{}{"error": err.Error()})
				return
			}
			if strings.TrimSpace(accounts[0].Password) != password {
				broadcast(fmt.Sprintf("⚠️ 占位账号密码已自动升级为兼容默认值: %s", password), "warning")
			}
			req.TempMail = &TempMailConfig{
				Count:         len(accounts),
				Password:      password,
				AllowParallel: false,
			}
			isTempMode = true
			req.Workers = normalizeTempWorkers(req.Workers, req.TempMail.AllowParallel)
			configureTempMailRuntime(req.Proxy, req.TempMail)
			if req.TempMail.AllowParallel {
				broadcast(fmt.Sprintf("🧪 自动识别 Temp Mail 占位账号: %d 个，并发 %d，切换延迟 %d 秒（平行开关: ON）", len(accounts), req.Workers, req.TempMail.PostSuccessDelaySeconds()), "warning")
			} else {
				broadcast(fmt.Sprintf("🧪 自动识别 Temp Mail 占位账号: %d 个，固定并发 1，切换延迟 %d 秒（平行开关: OFF）", len(accounts), req.TempMail.PostSuccessDelaySeconds()), "warning")
			}
		}
	}

	if len(accounts) == 0 {
		jsonResp(w, map[string]interface{}{"error": "没有有效的账号"})
		return
	}

	// 过滤已完成
	if req.SkipFinished {
		done := getFinishedEmails()
		var pending []Account
		for _, a := range accounts {
			if !done[strings.ToLower(a.Email)] {
				pending = append(pending, a)
			}
		}
		accounts = pending
	}

	if len(accounts) == 0 {
		jsonResp(w, map[string]interface{}{"error": "所有账号已注册完毕"})
		return
	}

	mode := "register"
	if req.LoginMode {
		mode = "login"
	}

	// 重置状态
	gRunning.Store(true)
	gStopFlag.Store(false)
	gTotal.Store(int32(len(accounts)))
	gSuccess.Store(0)
	gFail.Store(0)
	gStartTime = time.Now()

	go runWorkers(accounts, req.Proxy, req.Workers, mode, req.DomainMail, req.TempMail)

	jsonResp(w, map[string]interface{}{"ok": true, "total": len(accounts)})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	gStopFlag.Store(true)
	jsonResp(w, map[string]interface{}{"ok": true})
}

func handleStatusAPI(w http.ResponseWriter, r *http.Request) {
	elapsed := float64(0)
	if gRunning.Load() {
		elapsed = time.Since(gStartTime).Seconds()
	}
	jsonResp(w, map[string]interface{}{
		"running": gRunning.Load(),
		"success": gSuccess.Load(),
		"fail":    gFail.Load(),
		"total":   gTotal.Load(),
		"elapsed": elapsed,
	})
}

func handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan []byte, 100)
	sseClientsLock.Lock()
	sseClients[ch] = struct{}{}
	sseClientsLock.Unlock()

	defer func() {
		sseClientsLock.Lock()
		delete(sseClients, ch)
		sseClientsLock.Unlock()
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// ═══════════════════════════════════════════════════════
// Worker 池
// ═══════════════════════════════════════════════════════
func runWorkers(accounts []Account, proxy string, workers int, mode string, domainMail *DomainMailConfig, tempMail *TempMailConfig) {
	defer func() {
		gRunning.Store(false)
		elapsed := time.Since(gStartTime)
		broadcastJSON(map[string]interface{}{
			"type":    "done",
			"success": gSuccess.Load(),
			"fail":    gFail.Load(),
			"elapsed": fmt.Sprintf("%.1fs", elapsed.Seconds()),
		})
	}()

	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, acc := range accounts {
		if gStopFlag.Load() {
			break
		}
		sem <- struct{}{} // 限流
		wg.Add(1)
		go func(a Account, idx int) {
			defer func() {
				<-sem
				wg.Done()
			}()
			doOne(a, idx+1, len(accounts), proxy, mode, domainMail, tempMail)
		}(acc, i)
	}
	wg.Wait()
}

func doOne(acc Account, idx, total int, proxy, mode string, domainMail *DomainMailConfig, tempMail *TempMailConfig) {
	if gStopFlag.Load() {
		return
	}
	displayEmail := acc.Email
	if tempMail != nil {
		displayEmail = fmt.Sprintf("temp-mail#%d", idx)
	}
	broadcast(fmt.Sprintf("\n%s", strings.Repeat("─", 45)), "dim")
	broadcast(fmt.Sprintf("[%d/%d] %s", idx, total, displayEmail), "info")

	var success bool
	var lastErr string
	finalEmail := acc.Email
	assignedMailbox := ""

	if tempMail != nil {
		const maxMailboxAllocAttempts = 2
		for allocAttempt := 1; allocAttempt <= maxMailboxAllocAttempts; allocAttempt++ {
			if gStopFlag.Load() {
				return
			}
			mailbox, mailboxErr := acquireTempMailbox()
			if mailboxErr == nil {
				assignedMailbox = mailbox
				finalEmail = mailbox
				broadcast(fmt.Sprintf("  🧪 Temp Mail 分配邮箱: %s", mailbox), "info")
				break
			}
			lastErr = fmt.Sprintf("Temp Mail 获取邮箱失败: %v", mailboxErr)
			broadcast(fmt.Sprintf("  ❌ 分配邮箱失败 #%d: %s", allocAttempt, truncate(lastErr, 120)), "error")
			if allocAttempt < maxMailboxAllocAttempts {
				time.Sleep(time.Duration(allocAttempt*3) * time.Second)
			}
		}
		if assignedMailbox == "" {
			manualMailbox, manualErr := waitManualMailboxInput(3 * time.Minute)
			if manualErr != nil {
				lastErr = fmt.Sprintf("%s; %v", truncate(lastErr, 80), manualErr)
				gFail.Add(1)
				broadcastJSON(map[string]interface{}{
					"type": "result", "email": finalEmail, "success": false,
					"elapsed": "—", "error": truncate(lastErr, 100),
				})
				return
			}
			assignedMailbox = manualMailbox
			finalEmail = manualMailbox
			broadcast(fmt.Sprintf("  🧪 使用手动输入临时邮箱: %s", manualMailbox), "info")
		}
	}

	for attempt := 1; attempt <= MaxRetry; attempt++ {
		if gStopFlag.Load() {
			return
		}
		if attempt > 1 {
			broadcast(fmt.Sprintf("  重试 #%d...", attempt), "warning")
			sleepFlow(tempMail != nil, 2000, 4000)
		}

		runAcc := acc
		if assignedMailbox != "" {
			runAcc.Email = assignedMailbox
		}

		result, err := registerAccount(runAcc, proxy, mode, domainMail, tempMail)
		if err != nil {
			if gStopFlag.Load() {
				return
			}
			lastErr = err.Error()
			broadcast(fmt.Sprintf("  ❌ 尝试 %d 失败: %s", attempt, truncate(lastErr, 120)), "error")
			continue
		}

		// 保存结果
		os.MkdirAll(resultsDir, 0755)
		fpath := filepath.Join(resultsDir, result.Email+".json")
		data, _ := json.MarshalIndent(result, "", "  ")
		os.WriteFile(fpath, data, 0644)

		gSuccess.Add(1)
		broadcast(fmt.Sprintf("  🎉 注册成功: %s", result.Email), "success")
		broadcastJSON(map[string]interface{}{
			"type": "result", "email": result.Email, "success": true,
			"elapsed":    fmt.Sprintf("%.1fs", time.Since(gStartTime).Seconds()),
			"account_id": result.AccountID,
		})
		if tempMail != nil {
			delaySeconds := tempMail.PostSuccessDelaySeconds()
			if delaySeconds > 0 && idx < total {
				broadcast(fmt.Sprintf("  ⏳ 已获取 Token，%d 秒后切换到下一个账号...", delaySeconds), "dim")
				if !sleepWithStop(time.Duration(delaySeconds) * time.Second) {
					return
				}
			}
		}
		success = true
		break
	}

	if !success {
		gFail.Add(1)
		broadcastJSON(map[string]interface{}{
			"type": "result", "email": finalEmail, "success": false,
			"elapsed": "—", "error": truncate(lastErr, 100),
		})
	}
}

// ═══════════════════════════════════════════════════════
// 工具函数
// ═══════════════════════════════════════════════════════
func broadcast(msg, level string) {
	data, _ := json.Marshal(map[string]interface{}{
		"type": "log", "text": fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg), "level": level,
	})
	sseClientsLock.Lock()
	for ch := range sseClients {
		select {
		case ch <- data:
		default:
		}
	}
	sseClientsLock.Unlock()
	log.Printf("[%s] %s", level, msg)
}

func broadcastJSON(v interface{}) {
	data, _ := json.Marshal(v)
	sseClientsLock.Lock()
	for ch := range sseClients {
		select {
		case ch <- data:
		default:
		}
	}
	sseClientsLock.Unlock()
}

func jsonResp(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func getFinishedEmails() map[string]bool {
	done := make(map[string]bool)
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return done
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(resultsDir, e.Name()))
		if err != nil {
			continue
		}
		var m map[string]interface{}
		json.Unmarshal(data, &m)
		if email, ok := m["email"].(string); ok {
			done[strings.ToLower(email)] = true
		}
	}
	return done
}

func sleepRand(minMs, maxMs int) {
	if maxMs <= minMs {
		time.Sleep(time.Duration(minMs) * time.Millisecond)
		return
	}
	d := time.Duration(minMs+rand.Intn(maxMs-minMs)) * time.Millisecond
	time.Sleep(d)
}

func adjustedFlowDelayRange(tempMode bool, minMs, maxMs int) (int, int) {
	if !tempMode {
		return minMs, maxMs
	}
	fastMin := minMs / 4
	if fastMin < 80 {
		fastMin = 80
	}
	fastMax := maxMs / 4
	if fastMax < fastMin+40 {
		fastMax = fastMin + 40
	}
	if fastMax > maxMs {
		fastMax = maxMs
	}
	if fastMin > fastMax {
		fastMin = fastMax
	}
	return fastMin, fastMax
}

func sleepFlow(tempMode bool, minMs, maxMs int) {
	minMs, maxMs = adjustedFlowDelayRange(tempMode, minMs, maxMs)
	sleepRand(minMs, maxMs)
}

func sleepWithStop(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	deadline := time.Now().Add(d)
	for {
		if gStopFlag.Load() {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		if remaining > time.Second {
			remaining = time.Second
		}
		time.Sleep(remaining)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func strVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func strFromMap(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func intVal(m map[string]interface{}, key string) int {
	v, _ := m[key].(float64)
	return int(v)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
