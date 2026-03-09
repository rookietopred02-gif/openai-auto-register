package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

const (
	tempMailHomeURL      = "https://temp-mail.org/en/"
	tempMailAPIBase      = "https://web2.temp-mail.org"
	mailTMAPIBase        = "https://api.mail.tm"
	tempMailPollInterval = 4 * time.Second
	tempMailDefaultGap   = 15 * time.Second
	tempMailCreateWait   = 24 * time.Second
	mailTMDomainCacheTTL = 15 * time.Minute
)

var (
	tempMailCodeRe        = regexp.MustCompile(`\b(\d{6})\b`)
	tempMailChatGPTCodeRe = regexp.MustCompile(`(?is)chatgpt[^A-Za-z0-9]{0,120}(\d{6})`)
	tempMailAfterCodeRe   = regexp.MustCompile(`(?is)[^A-Za-z0-9](\d{6})\b`)
	tempMailEmailRe       = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
)

type TempMailConfig struct {
	Count            int    `json:"count"`
	Password         string `json:"password"`
	AllowParallel    bool   `json:"allow_parallel,omitempty"`
	NextDelaySeconds *int   `json:"next_delay_seconds,omitempty"`
}

func (c *TempMailConfig) PostSuccessDelaySeconds() int {
	if c == nil || c.NextDelaySeconds == nil {
		return 15
	}
	delay := *c.NextDelaySeconds
	if delay < 0 {
		return 15
	}
	if delay > 300 {
		return 300
	}
	return delay
}

func (c *TempMailConfig) MailboxCreateGap() time.Duration {
	return time.Duration(c.PostSuccessDelaySeconds()) * time.Second
}

type tempMailRow struct {
	ID       string
	Received string
	Text     string
}

type tempMailboxResp struct {
	Token   string `json:"token"`
	Mailbox string `json:"mailbox"`
}

type tempMessagesResp struct {
	Mailbox  string                   `json:"mailbox"`
	Messages []map[string]interface{} `json:"messages"`
}

type mailTMHydraResp struct {
	Members []map[string]interface{} `json:"hydra:member"`
}

type TempMailService struct {
	mu              sync.Mutex
	httpClient      *HTTPClient
	proxy           string
	createGap       time.Duration
	provider        string
	token           string
	currentMailbox  string
	firstServed     bool
	lastCreatedAt   time.Time
	mailTMDomain    string
	domainFetchedAt time.Time
	detailCache     map[string]string
}

var tempMailService = &TempMailService{createGap: tempMailDefaultGap}

type tempMailSession struct {
	Provider  string `json:"provider,omitempty"`
	Token     string `json:"token"`
	Mailbox   string `json:"mailbox"`
	UpdatedAt string `json:"updated_at"`
}

func ensureTempMailReady() error {
	return tempMailService.EnsureReady()
}

func acquireTempMailbox() (string, error) {
	return tempMailService.AcquireMailbox()
}

func configureTempMailRuntime(proxy string, cfg *TempMailConfig) {
	tempMailService.Configure(proxy, cfg)
}

func (s *TempMailService) Configure(proxy string, cfg *TempMailConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	proxy = strings.TrimSpace(proxy)
	if cfg == nil {
		s.createGap = tempMailDefaultGap
	} else {
		s.createGap = cfg.MailboxCreateGap()
	}
	if proxy != s.proxy {
		s.proxy = proxy
		// 代理变更后重建 HTTP 客户端（保留已缓存 token/mailbox）。
		s.httpClient = nil
		s.detailCache = nil
	}
}

func (s *TempMailService) EnsureReady() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureReadyLocked()
}

func (s *TempMailService) ensureReadyLocked() error {
	if s.httpClient == nil {
		client, err := NewHTTPClient(s.proxy)
		if err != nil {
			return fmt.Errorf("创建 Temp Mail HTTP 客户端失败: %w", err)
		}
		s.httpClient = client
	}

	hasReadyMailbox := isValidMailbox(s.currentMailbox) && (s.token != "" || strings.EqualFold(s.provider, "mailtm"))
	if hasReadyMailbox {
		if s.provider == "" {
			s.provider = "temp-mail"
		}
		return nil
	}

	if s.loadSessionLocked() && s.validateCurrentMailboxLocked() {
		s.firstServed = false
		return nil
	}

	_, _, _ = s.httpClient.Get(tempMailHomeURL)
	if err := s.createOrRotateMailboxLocked(""); err != nil {
		return err
	}
	s.firstServed = false
	return nil
}

func (s *TempMailService) AcquireMailbox() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureReadyLocked(); err != nil {
		return "", err
	}

	if !s.firstServed {
		s.firstServed = true
		if strings.EqualFold(s.provider, "mailtm") {
			// mail.tm 无需复用旧箱，首个账号也尽量使用新邮箱，避免复用导致账号已存在。
			if err := s.createMailTMMailboxLocked(); err == nil {
				return s.currentMailbox, nil
			}
		}
		return s.currentMailbox, nil
	}

	if err := s.createFreshMailboxLocked(s.token); err != nil {
		return "", err
	}
	return s.currentMailbox, nil
}

func (s *TempMailService) createFreshMailboxLocked(authToken string) error {
	previousMailbox := strings.TrimSpace(strings.ToLower(s.currentMailbox))
	if err := s.createOrRotateMailboxLocked(authToken); err != nil {
		return err
	}
	if previousMailbox != "" && strings.EqualFold(previousMailbox, strings.TrimSpace(strings.ToLower(s.currentMailbox))) {
		if strings.EqualFold(s.provider, "temp-mail") {
			broadcast("    ⚠️ Temp Mail 未拿到新邮箱，自动切换到 mail.tm 以避免复用旧地址", "warning")
			if err := s.createMailTMMailboxLocked(); err == nil {
				return nil
			}
		}
		return fmt.Errorf("未获取到新的临时邮箱，已阻止复用旧地址: %s", previousMailbox)
	}
	return nil
}

func (s *TempMailService) createOrRotateMailboxLocked(authToken string) error {
	if strings.EqualFold(s.provider, "mailtm") {
		return s.createMailTMMailboxLocked()
	}

	if wait := s.createGap - time.Since(s.lastCreatedAt); wait > 0 {
		broadcast(fmt.Sprintf("    ⏳ Temp Mail 冷却中，等待 %ds...", int(wait.Seconds())+1), "dim")
		time.Sleep(wait)
	}

	extraHeaders := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
	}
	if strings.TrimSpace(authToken) != "" {
		extraHeaders["Authorization"] = "Bearer " + strings.TrimSpace(authToken)
	}

	var lastErr error
	deadline := time.Now().Add(tempMailCreateWait)
	for attempt := 1; ; attempt++ {
		status, body, err := s.httpClient.PostJSON(tempMailAPIBase+"/mailbox", map[string]interface{}{}, extraHeaders)
		if err != nil {
			lastErr = fmt.Errorf("请求 temp-mail mailbox 失败: %w", err)
		} else if status == 429 {
			lastErr = fmt.Errorf("请求 temp-mail mailbox 触发限流: %d %s", status, truncate(body, 120))
			sleep := time.Duration(attempt*4) * time.Second
			if sleep > 45*time.Second {
				sleep = 45 * time.Second
			}
			broadcast(fmt.Sprintf("    ⚠️ Temp Mail 限流，%ds 后重试...", int(sleep.Seconds())), "warning")
			if time.Now().Add(sleep).After(deadline) {
				break
			}
			time.Sleep(sleep)
			continue
		} else if status < 200 || status >= 300 {
			lastErr = fmt.Errorf("请求 temp-mail mailbox 失败: %d %s", status, truncate(body, 200))
		} else {
			var resp tempMailboxResp
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				lastErr = fmt.Errorf("解析 temp-mail mailbox 失败: %w", err)
			} else {
				resp.Token = strings.TrimSpace(resp.Token)
				resp.Mailbox = strings.TrimSpace(resp.Mailbox)
				if resp.Token == "" || !isValidMailbox(resp.Mailbox) {
					lastErr = fmt.Errorf("temp-mail 未返回可用邮箱")
				} else {
					s.token = resp.Token
					s.provider = "temp-mail"
					s.currentMailbox = resp.Mailbox
					s.lastCreatedAt = time.Now()
					s.saveSessionLocked()
					return nil
				}
			}
		}

		sleep := time.Duration(attempt) * time.Second
		if sleep > 15*time.Second {
			sleep = 15 * time.Second
		}
		if time.Now().Add(sleep).After(deadline) {
			break
		}
		time.Sleep(sleep)
	}

	if s.validateCurrentMailboxLocked() {
		broadcast("    ⚠️ Temp Mail 创建新邮箱持续限流，复用当前邮箱继续", "warning")
		return nil
	}

	// temp-mail.org 被限流时自动切换到 mail.tm，避免任务硬失败
	errText := ""
	if lastErr != nil {
		errText = lastErr.Error()
	}
	if strings.Contains(strings.ToLower(errText), "限流") || strings.Contains(errText, "429") {
		broadcast("    ⚠️ Temp Mail 持续限流，自动切换到备用临时邮箱服务 mail.tm", "warning")
		if err := s.createMailTMMailboxLocked(); err == nil {
			return nil
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("请求 temp-mail mailbox 失败")
	}
	if strings.Contains(strings.ToLower(lastErr.Error()), "限流") || strings.Contains(lastErr.Error(), "429") {
		return fmt.Errorf("%w（建议先将 Temp Mail 数量设为 1，或为任务设置代理/VPN 后重试）", lastErr)
	}
	return lastErr
}

func (s *TempMailService) tempMailGetLocked(path string) (int, string, error) {
	if s.httpClient == nil {
		return 0, "", fmt.Errorf("temp-mail client is nil")
	}
	req, err := fhttp.NewRequest("GET", tempMailAPIBase+path, nil)
	if err != nil {
		return 0, "", err
	}
	s.httpClient.setGetHeaders(req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.httpClient.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	s.httpClient.saveCookies(resp)

	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func (s *TempMailService) fetchRowsLocked() (string, []tempMailRow, error) {
	if strings.EqualFold(s.provider, "mailtm") {
		return s.fetchRowsMailTMLocked()
	}

	var (
		status int
		body   string
		err    error
	)

	for attempt := 1; attempt <= 5; attempt++ {
		status, body, err = s.tempMailGetLocked("/messages")
		if err != nil {
			if attempt < 5 {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return "", nil, err
		}
		if status == 429 {
			if attempt < 5 {
				wait := time.Duration(attempt*3) * time.Second
				broadcast(fmt.Sprintf("    ⚠️ Temp Mail 消息拉取限流，%ds 后重试...", int(wait.Seconds())), "warning")
				time.Sleep(wait)
				continue
			}
			break
		}
		if status == 401 || status == 403 {
			if rotateErr := s.createOrRotateMailboxLocked(""); rotateErr != nil {
				return "", nil, rotateErr
			}
			status, body, err = s.tempMailGetLocked("/messages")
			if err != nil {
				return "", nil, err
			}
		}
		break
	}
	if status < 200 || status >= 300 {
		return "", nil, fmt.Errorf("读取 temp-mail 消息失败: %d %s", status, truncate(body, 200))
	}

	var resp tempMessagesResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", nil, fmt.Errorf("解析 temp-mail 消息失败: %w", err)
	}

	mailbox := strings.TrimSpace(resp.Mailbox)
	if isValidMailbox(mailbox) {
		s.currentMailbox = mailbox
		s.saveSessionLocked()
	}

	rows := make([]tempMailRow, 0, len(resp.Messages))
	for idx, msg := range resp.Messages {
		row := tempMailRow{
			ID:       strings.TrimSpace(pickFirstNonEmpty(strFromAny(msg["id"]), strFromAny(msg["_id"]), strFromAny(msg["message_id"]))),
			Received: strings.TrimSpace(pickFirstNonEmpty(strFromAny(msg["created_at"]), strFromAny(msg["createdAt"]), strFromAny(msg["date"]), strFromAny(msg["timestamp"]), strFromAny(msg["time"]), strFromAny(msg["receivedAt"]), strFromAny(msg["sent_at"]))),
		}
		if row.ID == "" {
			row.ID = fmt.Sprintf("row-%d", idx)
		}
		if b, err := json.Marshal(msg); err == nil {
			row.Text = string(b)
		}
		rows = append(rows, row)
	}

	return mailbox, rows, nil
}

func (s *TempMailService) createMailTMMailboxLocked() error {
	if s.httpClient == nil {
		return fmt.Errorf("temp-mail client is nil")
	}
	domain, err := s.fetchMailTMDomainLocked()
	if err != nil {
		return err
	}

	// 避免地址冲突，最多尝试 6 次。
	var (
		address string
		token   string
		lastErr error
	)
	for i := 0; i < 6; i++ {
		local := fmt.Sprintf("tm%d%x", time.Now().Unix()%1_000_000_000, rand.Intn(0xffff))
		address = strings.ToLower(strings.TrimSpace(local + "@" + domain))
		password := fmt.Sprintf("Qw%d!mT", 100000+rand.Intn(899999))

		accPayload := map[string]string{
			"address":  address,
			"password": password,
		}
		status, body, reqErr := s.mailTMPostJSONLocked("/accounts", accPayload, "")
		if reqErr != nil {
			lastErr = fmt.Errorf("创建 mail.tm 账号失败: %w", reqErr)
			continue
		}
		if status < 200 || status >= 300 {
			// 地址冲突会返回 422，继续换地址重试。
			if status == 422 {
				lastErr = fmt.Errorf("mail.tm 地址冲突")
				continue
			}
			lastErr = fmt.Errorf("创建 mail.tm 账号失败: %d %s", status, truncate(body, 180))
			continue
		}

		tokPayload := map[string]string{
			"address":  address,
			"password": password,
		}
		ts, tb, tokErr := s.mailTMPostJSONLocked("/token", tokPayload, "")
		if tokErr != nil {
			lastErr = fmt.Errorf("获取 mail.tm token 失败: %w", tokErr)
			continue
		}
		if ts < 200 || ts >= 300 {
			lastErr = fmt.Errorf("获取 mail.tm token 失败: %d %s", ts, truncate(tb, 180))
			continue
		}

		var tok map[string]interface{}
		if err := json.Unmarshal([]byte(tb), &tok); err != nil {
			lastErr = fmt.Errorf("解析 mail.tm token 失败: %w", err)
			continue
		}
		token = strings.TrimSpace(strFromAny(tok["token"]))
		if token == "" {
			lastErr = fmt.Errorf("mail.tm token 为空")
			continue
		}
		break
	}
	if token == "" {
		if lastErr == nil {
			lastErr = fmt.Errorf("mail.tm 创建邮箱失败")
		}
		return lastErr
	}

	s.provider = "mailtm"
	s.token = token
	s.currentMailbox = address
	s.lastCreatedAt = time.Now()
	s.detailCache = make(map[string]string)
	s.saveSessionLocked()
	return nil
}

func (s *TempMailService) fetchMailTMDomainLocked() (string, error) {
	if domain := strings.TrimSpace(s.mailTMDomain); domain != "" && time.Since(s.domainFetchedAt) < mailTMDomainCacheTTL {
		return domain, nil
	}
	status, body, err := s.mailTMGetLocked("/domains?page=1", "")
	if err != nil {
		return "", fmt.Errorf("读取 mail.tm 域名失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("读取 mail.tm 域名失败: %d %s", status, truncate(body, 180))
	}
	var resp mailTMHydraResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", fmt.Errorf("解析 mail.tm 域名失败: %w", err)
	}
	for _, d := range resp.Members {
		domain := strings.TrimSpace(strFromAny(d["domain"]))
		isActive := strings.EqualFold(strFromAny(d["isActive"]), "true") || strFromAny(d["isActive"]) == "1"
		if !isActive {
			// bool 类型兼容
			if b, ok := d["isActive"].(bool); !ok || !b {
				continue
			}
		}
		if isValidMailbox("x@" + domain) {
			s.mailTMDomain = domain
			s.domainFetchedAt = time.Now()
			return domain, nil
		}
	}
	return "", fmt.Errorf("mail.tm 无可用域名")
}

func (s *TempMailService) fetchRowsMailTMLocked() (string, []tempMailRow, error) {
	if strings.TrimSpace(s.token) == "" || !isValidMailbox(s.currentMailbox) {
		return "", nil, fmt.Errorf("mail.tm 邮箱状态无效")
	}
	status, body, err := s.mailTMGetLocked("/messages?page=1", s.token)
	if err != nil {
		return "", nil, fmt.Errorf("读取 mail.tm 消息失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", nil, fmt.Errorf("读取 mail.tm 消息失败: %d %s", status, truncate(body, 200))
	}

	var resp mailTMHydraResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", nil, fmt.Errorf("解析 mail.tm 消息列表失败: %w", err)
	}

	rows := make([]tempMailRow, 0, len(resp.Members))
	for idx, msg := range resp.Members {
		id := strings.TrimSpace(strFromAny(msg["id"]))
		if id == "" {
			id = fmt.Sprintf("row-%d", idx)
		}

		received := strings.TrimSpace(pickFirstNonEmpty(
			strFromAny(msg["createdAt"]),
			strFromAny(msg["created_at"]),
			strFromAny(msg["date"]),
		))

		text := ""
		if b, err := json.Marshal(msg); err == nil {
			text = string(b)
		}

		if extractTempMailCode(text, len(resp.Members)) == "" && isTempMailCodeCandidate(text) {
			if s.detailCache == nil {
				s.detailCache = make(map[string]string)
			}
			if cached := strings.TrimSpace(s.detailCache[id]); cached != "" {
				text = cached
			} else {
				detailPath := "/messages/" + url.PathEscape(id)
				ds, db, derr := s.mailTMGetLocked(detailPath, s.token)
				if derr == nil && ds >= 200 && ds < 300 {
					text = db
					s.detailCache[id] = db
				}
			}
		}

		rows = append(rows, tempMailRow{
			ID:       id,
			Received: received,
			Text:     text,
		})
	}
	return s.currentMailbox, rows, nil
}

func (s *TempMailService) mailTMGetLocked(path, bearer string) (int, string, error) {
	if s.httpClient == nil {
		return 0, "", fmt.Errorf("temp-mail client is nil")
	}
	req, err := fhttp.NewRequest("GET", mailTMAPIBase+path, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header = fhttp.Header{
		"user-agent":      {s.httpClient.userAgent},
		"accept":          {"application/ld+json, application/json;q=0.9, */*;q=0.8"},
		"accept-language": {"en-US,en;q=0.9"},
		"accept-encoding": {"gzip, deflate, br"},
	}
	if strings.TrimSpace(bearer) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
	}
	resp, err := s.httpClient.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	s.httpClient.saveCookies(resp)
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func (s *TempMailService) mailTMPostJSONLocked(path string, payload interface{}, bearer string) (int, string, error) {
	if s.httpClient == nil {
		return 0, "", fmt.Errorf("temp-mail client is nil")
	}
	b, _ := json.Marshal(payload)
	req, err := fhttp.NewRequest("POST", mailTMAPIBase+path, strings.NewReader(string(b)))
	if err != nil {
		return 0, "", err
	}
	req.Header = fhttp.Header{
		"user-agent":      {s.httpClient.userAgent},
		"accept":          {"application/ld+json, application/json;q=0.9, */*;q=0.8"},
		"content-type":    {"application/json"},
		"accept-language": {"en-US,en;q=0.9"},
		"accept-encoding": {"gzip, deflate, br"},
	}
	if strings.TrimSpace(bearer) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
	}
	resp, err := s.httpClient.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	s.httpClient.saveCookies(resp)
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

func (s *TempMailService) validateCurrentMailboxLocked() bool {
	if !isValidMailbox(s.currentMailbox) || s.httpClient == nil {
		return false
	}
	if strings.EqualFold(s.provider, "mailtm") {
		if strings.TrimSpace(s.token) == "" {
			return false
		}
		status, _, err := s.mailTMGetLocked("/messages?page=1", s.token)
		if err != nil {
			return false
		}
		return status >= 200 && status < 300
	}
	if strings.TrimSpace(s.token) == "" {
		return false
	}
	status, _, err := s.tempMailGetLocked("/messages")
	if err != nil {
		return false
	}
	if status >= 200 && status < 300 {
		return true
	}
	// 消息端限流时，不丢弃已缓存的邮箱，继续后续轮询重试
	if status == 429 {
		return true
	}
	return false
}

func (s *TempMailService) sessionFilePathLocked() string {
	base := "."
	if strings.TrimSpace(resultsDir) != "" {
		base = strings.TrimSpace(resultsDir)
	}
	return filepath.Join(base, ".temp_mail_session.json")
}

func (s *TempMailService) loadSessionLocked() bool {
	path := s.sessionFilePathLocked()
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return false
	}
	var sess tempMailSession
	if err := json.Unmarshal(b, &sess); err != nil {
		return false
	}
	if strings.TrimSpace(sess.Provider) == "" {
		sess.Provider = "temp-mail"
	}
	s.provider = strings.TrimSpace(sess.Provider)
	sess.Token = strings.TrimSpace(sess.Token)
	sess.Mailbox = strings.TrimSpace(sess.Mailbox)
	if !isValidMailbox(sess.Mailbox) {
		return false
	}
	if !strings.EqualFold(s.provider, "mailtm") && sess.Token == "" {
		return false
	}
	s.token = sess.Token
	s.currentMailbox = sess.Mailbox
	return true
}

func (s *TempMailService) saveSessionLocked() {
	if !isValidMailbox(s.currentMailbox) {
		return
	}
	if !strings.EqualFold(s.provider, "mailtm") && strings.TrimSpace(s.token) == "" {
		return
	}
	path := s.sessionFilePathLocked()
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	payload := tempMailSession{
		Provider:  firstNonEmpty(strings.TrimSpace(s.provider), "temp-mail"),
		Token:     strings.TrimSpace(s.token),
		Mailbox:   strings.TrimSpace(s.currentMailbox),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0644)
}

func (s *TempMailService) FindCode(expectedEmail string, minTime time.Time, seen map[string]struct{}) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureReadyLocked(); err != nil {
		return "", err
	}

	mailbox, rows, err := s.fetchRowsLocked()
	if err != nil {
		return "", err
	}
	if expectedEmail != "" && isValidMailbox(mailbox) && !strings.EqualFold(expectedEmail, mailbox) {
		return "", fmt.Errorf("temp-mail 当前邮箱变更: expected=%s current=%s", expectedEmail, mailbox)
	}

	return findBestTempMailCode(rows, minTime, seen), nil
}

func waitForTempMailCode(email string, otpSentAt time.Time, resendFn func() bool) (string, error) {
	integratedIMAP.ConsumeCode(strings.ToLower(email), "")
	minTime := otpSentAt.Add(-60 * time.Second)

	done := make(chan struct{})
	defer close(done)

	// 定时重发 OTP
	go func() {
		select {
		case <-time.After(20 * time.Second):
		case <-done:
			return
		}
		if resendFn != nil {
			resendFn()
			broadcast("    🔄 已重发 OTP", "info")
		}
		ticker := time.NewTicker(ResendInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if resendFn != nil {
					resendFn()
					broadcast("    🔄 已重发 OTP", "info")
				}
			case <-done:
				return
			}
		}
	}()

	// Temp Mail 轮询，发现验证码后注入到统一等待通道（支持终端手动输入兜底）
	go func() {
		seen := map[string]struct{}{}
		var lastWarnAt time.Time

		poll := func() {
			code, err := tempMailService.FindCode(email, minTime, seen)
			if err != nil {
				if time.Since(lastWarnAt) > 10*time.Second {
					broadcast(fmt.Sprintf("    ⚠️ Temp Mail 轮询异常: %s", truncate(err.Error(), 120)), "warning")
					lastWarnAt = time.Now()
				}
				return
			}
			if code == "" {
				return
			}
			if _, injectErr := integratedIMAP.InjectManualCode(email, code, "temp-mail"); injectErr != nil {
				if time.Since(lastWarnAt) > 10*time.Second {
					broadcast(fmt.Sprintf("    ⚠️ Temp Mail 注入验证码失败: %s", truncate(injectErr.Error(), 120)), "warning")
					lastWarnAt = time.Now()
				}
			}
		}

		poll()
		ticker := time.NewTicker(tempMailPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				poll()
			}
		}
	}()

	manualCodeHint(email)
	code, err := WaitVerificationCode(email, PollTimeout, minTime)
	if err != nil {
		return "", err
	}
	if code == "" {
		return "", fmt.Errorf("empty verification code for %s", email)
	}
	broadcast(fmt.Sprintf("    ✅ 验证码: %s (Temp Mail)", code), "success")
	return code, nil
}

func extractTempMailCode(text string, _ int) string {
	if text == "" {
		return ""
	}

	// 去掉邮箱地址，避免误把地址中的 6 位数字当作验证码
	clean := tempMailEmailRe.ReplaceAllString(text, " ")
	lower := strings.ToLower(clean)
	if !strings.Contains(lower, "chatgpt") {
		return ""
	}

	if m := tempMailChatGPTCodeRe.FindStringSubmatch(clean); len(m) > 1 {
		return m[1]
	}

	pos := strings.Index(lower, "chatgpt")
	if pos < 0 || pos >= len(clean) {
		return ""
	}
	tail := clean[pos:]
	if m := tempMailAfterCodeRe.FindStringSubmatch(tail); len(m) > 1 {
		return m[1]
	}
	if m := tempMailCodeRe.FindStringSubmatch(tail); len(m) > 1 {
		return m[1]
	}

	return ""
}

func isTempMailCodeCandidate(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "chatgpt") || strings.Contains(lower, "openai")
}

func findBestTempMailCode(rows []tempMailRow, minTime time.Time, seen map[string]struct{}) string {
	rowCount := len(rows)
	var bestCode string
	var bestTs time.Time

	for _, row := range rows {
		key := strings.TrimSpace(row.ID)
		if key == "" {
			key = row.Received + "|" + truncate(row.Text, 120)
		}
		if _, ok := seen[key]; ok {
			continue
		}

		ts := parseTempMailTime(row.Received)
		if !ts.IsZero() && ts.Before(minTime) {
			seen[key] = struct{}{}
			continue
		}

		code := extractTempMailCode(row.Text, rowCount)
		if code == "" {
			if !isTempMailCodeCandidate(row.Text) {
				seen[key] = struct{}{}
			}
			continue
		}
		seen[key] = struct{}{}

		if ts.IsZero() {
			if bestCode == "" {
				bestCode = code
			}
			continue
		}
		if bestTs.IsZero() || ts.After(bestTs) {
			bestTs = ts
			bestCode = code
		}
	}

	return bestCode
}

func parseTempMailTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1_000_000_000_000 {
			return time.UnixMilli(n)
		}
		if n > 1_000_000_000 {
			return time.Unix(n, 0)
		}
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		n := int64(f)
		if n > 1_000_000_000_000 {
			return time.UnixMilli(n)
		}
		if n > 1_000_000_000 {
			return time.Unix(n, 0)
		}
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05 -0700",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func isValidMailbox(mailbox string) bool {
	mailbox = strings.TrimSpace(strings.ToLower(mailbox))
	if mailbox == "" {
		return false
	}
	if !strings.Contains(mailbox, "@") {
		return false
	}
	if strings.Contains(mailbox, "loading") {
		return false
	}
	return true
}

func strFromAny(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

func pickFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func splitMailbox(mailbox string) (string, string, bool) {
	mailbox = strings.TrimSpace(strings.ToLower(mailbox))
	parts := strings.Split(mailbox, "@")
	if len(parts) != 2 {
		return "", "", false
	}
	login := strings.TrimSpace(parts[0])
	domain := strings.TrimSpace(parts[1])
	if login == "" || domain == "" {
		return "", "", false
	}
	return login, domain, true
}
