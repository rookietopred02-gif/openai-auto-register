package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	imap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	gomail "github.com/emersion/go-message/mail"
)

var (
	integratedCodeRegex      = regexp.MustCompile(`\b(\d{6})\b`)
	integratedBodyCodeRegex  = regexp.MustCompile(`(?i)(?:code(?:\s+is)?|verification)[:\s]+(\d{6})`)
	integratedEmailRegex     = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	integratedHTMLTagRe      = regexp.MustCompile(`<[^>]+>`)
	integratedForwardedToRe  = regexp.MustCompile(`(?is)this email was sent to\s+([A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,})\s+from\s+[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\s+and has been forwarded by\s+(?:anonaddy|addy(?:\.io)?|simplelogin)`)
	integratedAnonAddyFromRe = regexp.MustCompile(`^([^+@]+)\+[^@]+@([A-Za-z0-9.\-]+\.[A-Za-z]{2,})$`)
)

type IntegratedIMAPConfig struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	UseTLS      bool   `json:"use_tls"`
	PollSeconds int    `json:"poll_seconds"`
}

type IntegratedIMAPCode struct {
	Email      string    `json:"email"`
	Code       string    `json:"code"`
	Source     string    `json:"source"`
	Subject    string    `json:"subject"`
	From       string    `json:"from"`
	ReceivedAt time.Time `json:"received_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type integratedEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type IntegratedIMAPService struct {
	mu        sync.RWMutex
	cfg       IntegratedIMAPConfig
	codes     map[string]IntegratedIMAPCode
	waiters   map[string][]chan string
	clients   map[chan string]struct{}
	storePath string
	running   bool
	stopCh    chan struct{}
}

func NewIntegratedIMAPService(storePath string) *IntegratedIMAPService {
	s := &IntegratedIMAPService{
		codes:     map[string]IntegratedIMAPCode{},
		waiters:   map[string][]chan string{},
		clients:   map[chan string]struct{}{},
		storePath: storePath,
		stopCh:    make(chan struct{}),
	}
	s.loadCodes()
	return s
}

var integratedIMAP = NewIntegratedIMAPService("codes.json")

var integratedDefaultRouteOnce sync.Once

func init() {
	RegisterIntegratedIMAPRoutes(nil)
	if err := integratedIMAP.AutoLoadConfig(); err != nil {
		log.Printf("integrated IMAP auto-load skipped: %v", err)
	}
}

func ConfigureIntegratedIMAP(host string, port int, username, password string, useTLS bool) error {
	cfg := IntegratedIMAPConfig{
		Host:        strings.TrimSpace(host),
		Port:        port,
		Username:    strings.TrimSpace(username),
		Password:    password,
		UseTLS:      useTLS,
		PollSeconds: 5,
	}
	return integratedIMAP.SetConfig(cfg)
}

func WaitVerificationCode(email string, timeout time.Duration, minTime time.Time) (string, error) {
	return integratedIMAP.WaitForCode(email, timeout, minTime)
}

func GetIntegratedVerificationCodes() []IntegratedIMAPCode {
	return integratedIMAP.GetCodes()
}

func CleanupVerificationEmails() (int, error) {
	return integratedIMAP.CleanupOpenAIMails()
}

func RegisterIntegratedIMAPRoutes(mux *http.ServeMux) {
	if mux == nil {
		integratedDefaultRouteOnce.Do(func() {
			registerIntegratedIMAPRoutes(http.DefaultServeMux)
		})
		return
	}
	registerIntegratedIMAPRoutes(mux)
}

func registerIntegratedIMAPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/config", integratedIMAP.handleConfig)
	mux.HandleFunc("/api/codes", integratedIMAP.handleCodes)
	mux.HandleFunc("/api/events", integratedIMAP.handleEvents)
	mux.HandleFunc("/api/cleanup", integratedIMAP.handleCleanup)
	mux.HandleFunc("/api/imap/status", integratedIMAP.handleStatus)
	mux.HandleFunc("/api/get-code", integratedIMAP.handleGetCode)
	mux.HandleFunc("/api/consume", integratedIMAP.handleConsume)

	mux.HandleFunc("/api/imap/config", integratedIMAP.handleConfig)
	mux.HandleFunc("/api/imap/codes", integratedIMAP.handleCodes)
	mux.HandleFunc("/api/imap/events", integratedIMAP.handleEvents)
	mux.HandleFunc("/api/imap/cleanup", integratedIMAP.handleCleanup)
	mux.HandleFunc("/api/imap/get-code", integratedIMAP.handleGetCode)
	mux.HandleFunc("/api/imap/consume", integratedIMAP.handleConsume)
}

func (s *IntegratedIMAPService) SetConfig(cfg IntegratedIMAPConfig) error {
	if cfg.Host == "" || cfg.Username == "" || cfg.Password == "" || cfg.Port <= 0 {
		return errors.New("IMAP 配置不完整")
	}
	if cfg.PollSeconds <= 0 {
		cfg.PollSeconds = 5
	}

	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()

	s.broadcast("config", map[string]interface{}{
		"ok":       true,
		"host":     cfg.Host,
		"port":     cfg.Port,
		"username": cfg.Username,
	})
	s.startPolling()
	return nil
}

func (s *IntegratedIMAPService) AutoLoadConfig() error {
	candidates := integratedConfigCandidates()
	var errs []string
	for _, candidate := range candidates {
		cfg, err := loadIntegratedIMAPConfigFromFile(candidate)
		if err != nil {
			errs = append(errs, candidate+": "+err.Error())
			continue
		}
		if err := s.SetConfig(cfg); err != nil {
			errs = append(errs, candidate+": "+err.Error())
			continue
		}
		log.Printf("integrated IMAP config loaded from %s", candidate)
		return nil
	}
	return errors.New(strings.Join(errs, " | "))
}

func integratedConfigCandidates() []string {
	seen := map[string]struct{}{}
	add := func(items *[]string, path string) {
		if path == "" {
			return
		}
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		*items = append(*items, clean)
	}

	items := []string{}
	add(&items, "config.json")
	add(&items, "current_config.txt")
	add(&items, filepath.Join("..", "config.json"))
	add(&items, filepath.Join("..", "current_config.txt"))
	if wd, err := os.Getwd(); err == nil {
		add(&items, filepath.Join(wd, "config.json"))
		add(&items, filepath.Join(wd, "current_config.txt"))
		add(&items, filepath.Join(wd, "..", "config.json"))
		add(&items, filepath.Join(wd, "..", "current_config.txt"))
	}
	return items
}

func loadIntegratedIMAPConfigFromFile(path string) (IntegratedIMAPConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return IntegratedIMAPConfig{}, err
	}
	if cfg, ok := loadIntegratedIMAPConfigFromText(data); ok {
		return finalizeIntegratedIMAPConfig(cfg)
	}
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return IntegratedIMAPConfig{}, err
	}
	cfg := extractIntegratedIMAPConfig(raw)
	return finalizeIntegratedIMAPConfig(cfg)
}

func finalizeIntegratedIMAPConfig(cfg IntegratedIMAPConfig) (IntegratedIMAPConfig, error) {
	if cfg.Host == "" || cfg.Port <= 0 || cfg.Username == "" || cfg.Password == "" {
		return IntegratedIMAPConfig{}, errors.New("未找到完整的 IMAP 字段")
	}
	if cfg.Port == 993 || cfg.Port == 995 {
		cfg.UseTLS = true
	}
	if cfg.PollSeconds <= 0 {
		cfg.PollSeconds = 5
	}
	return cfg, nil
}

func loadIntegratedIMAPConfigFromText(data []byte) (IntegratedIMAPConfig, bool) {
	cfg := IntegratedIMAPConfig{}
	found := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if value == "" {
			continue
		}
		switch key {
		case "IMAP 服务器", "IMAP 主机", "IMAP Host", "IMAP HOST":
			cfg.Host = value
			found = true
		case "IMAP 端口", "IMAP Port", "IMAP PORT":
			if port, err := strconv.Atoi(value); err == nil {
				cfg.Port = port
				found = true
			}
		case "IMAP 账号", "IMAP 用户名", "IMAP Username", "IMAP USERNAME":
			cfg.Username = value
			found = true
		case "IMAP 密码", "IMAP Password", "IMAP PASSWORD":
			cfg.Password = value
			found = true
		}
	}
	return cfg, found
}

func extractIntegratedIMAPConfig(raw interface{}) IntegratedIMAPConfig {
	cfg := IntegratedIMAPConfig{}
	walkIntegratedJSON(raw, func(m map[string]interface{}) {
		if cfg.Host == "" {
			cfg.Host = pickIntegratedString(m, "imap_host", "host", "server", "imapServer")
		}
		if cfg.Port <= 0 {
			cfg.Port = pickIntegratedInt(m, "imap_port", "port")
		}
		if cfg.Username == "" {
			cfg.Username = pickIntegratedString(m, "imap_username", "username", "user", "email", "address")
		}
		if cfg.Password == "" {
			cfg.Password = pickIntegratedString(m, "imap_password", "password", "pass")
		}
		if !cfg.UseTLS {
			cfg.UseTLS = pickIntegratedBool(m, "use_tls", "tls", "ssl")
		}
		if cfg.PollSeconds <= 0 {
			cfg.PollSeconds = pickIntegratedInt(m, "poll_seconds", "interval", "poll_interval")
		}
	})
	return cfg
}

func walkIntegratedJSON(raw interface{}, visit func(map[string]interface{})) {
	switch v := raw.(type) {
	case map[string]interface{}:
		visit(v)
		for _, child := range v {
			walkIntegratedJSON(child, visit)
		}
	case []interface{}:
		for _, child := range v {
			walkIntegratedJSON(child, visit)
		}
	}
}

func pickIntegratedString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if val, ok := m[key]; ok {
			switch x := val.(type) {
			case string:
				if strings.TrimSpace(x) != "" {
					return strings.TrimSpace(x)
				}
			}
		}
	}
	return ""
}

func pickIntegratedInt(m map[string]interface{}, keys ...string) int {
	for _, key := range keys {
		if val, ok := m[key]; ok {
			switch x := val.(type) {
			case float64:
				if int(x) > 0 {
					return int(x)
				}
			case int:
				if x > 0 {
					return x
				}
			}
		}
	}
	return 0
}

func pickIntegratedBool(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if val, ok := m[key]; ok {
			switch x := val.(type) {
			case bool:
				return x
			case string:
				lower := strings.ToLower(strings.TrimSpace(x))
				if lower == "true" || lower == "1" || lower == "yes" || lower == "on" {
					return true
				}
			}
		}
	}
	return false
}

func (s *IntegratedIMAPService) GetCodes() []IntegratedIMAPCode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]IntegratedIMAPCode, 0, len(s.codes))
	for _, code := range s.codes {
		out = append(out, code)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func (s *IntegratedIMAPService) WaitForCode(email string, timeout time.Duration, minTime time.Time) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", errors.New("邮箱不能为空")
	}

	// 先检查已有的码（必须在 minTime 之后）
	if code := s.getExistingCodeAfter(email, minTime); code != "" {
		return code, nil
	}

	ch := make(chan string, 1)
	s.mu.Lock()
	s.waiters[email] = append(s.waiters[email], ch)
	s.mu.Unlock()
	defer s.removeWaiter(email, ch)

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case code := <-ch:
			if code != "" {
				// 再次检查时间（邮件内部时间已在 upsertCode 里存入 entry）
				s.mu.RLock()
				entry, ok := s.codes[email]
				s.mu.RUnlock()
				if ok && !minTime.IsZero() && !entry.ReceivedAt.IsZero() && entry.ReceivedAt.Before(minTime) {
					// 旧邮件（ReceivedAt 早于 minTime），继续等待
					continue
				}
				return code, nil
			}
		case <-timer.C:
			return "", fmt.Errorf("等待验证码超时: %s", email)
		case <-ticker.C:
			if gStopFlag.Load() {
				return "", fmt.Errorf("收到停止信号，停止等待")
			}
		}
	}
}

func (s *IntegratedIMAPService) getExistingCode(email string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.codes[email]; ok && v.Code != "" {
		return v.Code
	}
	return ""
}

// getExistingCodeAfter 返回在 minTime 后收到的已存验证码
func (s *IntegratedIMAPService) getExistingCodeAfter(email string, minTime time.Time) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.codes[email]; ok && v.Code != "" {
		// minTime 为零 = 不过滤；ReceivedAt 为零 = 时间未知，接受；否则只接受 minTime 之后的
		if minTime.IsZero() || v.ReceivedAt.IsZero() || !v.ReceivedAt.Before(minTime) {
			return v.Code
		}
	}
	return ""
}

func (s *IntegratedIMAPService) PeekCode(email string) (IntegratedIMAPCode, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if entry, ok := s.codes[email]; ok && entry.Code != "" {
		return entry, true
	}
	return IntegratedIMAPCode{}, false
}

func (s *IntegratedIMAPService) ConsumeCode(email, code string) (IntegratedIMAPCode, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	code = strings.TrimSpace(code)

	s.mu.Lock()
	defer s.mu.Unlock()

	if email != "" {
		entry, ok := s.codes[email]
		if !ok || entry.Code == "" {
			return IntegratedIMAPCode{}, errors.New("验证码不存在")
		}
		if code != "" && entry.Code != code {
			return IntegratedIMAPCode{}, errors.New("验证码不匹配")
		}
		delete(s.codes, email)
		go s.saveCodes()
		go s.broadcast("consume", map[string]interface{}{"email": entry.Email, "code": entry.Code, "ok": true})
		return entry, nil
	}

	if code != "" {
		for key, entry := range s.codes {
			if entry.Code == code {
				delete(s.codes, key)
				go s.saveCodes()
				go s.broadcast("consume", map[string]interface{}{"email": entry.Email, "code": entry.Code, "ok": true})
				return entry, nil
			}
		}
	}

	return IntegratedIMAPCode{}, errors.New("验证码不存在")
}

func (s *IntegratedIMAPService) removeWaiter(email string, target chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.waiters[email]
	if len(items) == 0 {
		return
	}
	filtered := items[:0]
	for _, ch := range items {
		if ch != target {
			filtered = append(filtered, ch)
		}
	}
	if len(filtered) == 0 {
		delete(s.waiters, email)
		return
	}
	s.waiters[email] = filtered
}

func (s *IntegratedIMAPService) startPolling() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stopCh = make(chan struct{})
	s.mu.Unlock()

	go func() {
		s.broadcast("log", map[string]interface{}{"message": "📡 IMAP 监听已启动"})
		for {
			cfg := s.getConfig()
			if cfg.Host != "" {
				if err := s.fetchLatestCodesOnce(); err != nil {
					s.broadcast("log", map[string]interface{}{"message": "❌ IMAP 拉取失败: " + err.Error()})
				}
			}

			wait := time.Duration(cfg.PollSeconds)
			if wait <= 0 {
				wait = 5
			}
			select {
			case <-time.After(wait * time.Second):
			case <-s.stopCh:
				s.broadcast("log", map[string]interface{}{"message": "🛑 IMAP 监听已停止"})
				return
			}
		}
	}()
}

func (s *IntegratedIMAPService) getConfig() IntegratedIMAPConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *IntegratedIMAPService) fetchLatestCodesOnce() error {
	c, err := s.connect()
	if err != nil {
		return err
	}
	defer c.Logout()

	mbox, err := c.Select("INBOX", false)
	if err != nil {
		return err
	}
	if mbox.Messages == 0 {
		return nil
	}

	from := uint32(1)
	if mbox.Messages > 80 {
		from = mbox.Messages - 79
	}

	seqset := new(imap.SeqSet)
	seqset.AddRange(from, mbox.Messages)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchInternalDate, section.FetchItem()}
	messages := make(chan *imap.Message, 20)
	errCh := make(chan error, 1)

	go func() {
		errCh <- c.Fetch(seqset, items, messages)
	}()

	changed := false
	mailboxEmail := strings.ToLower(strings.TrimSpace(s.getConfig().Username))
	for msg := range messages {
		code, entry, ok := parseIntegratedMessage(msg, section, mailboxEmail)
		if !ok || code == "" || entry.Email == "" {
			continue
		}
		if s.upsertCode(entry) {
			changed = true
		}
	}

	if err := <-errCh; err != nil {
		return err
	}
	if changed {
		s.saveCodes()
	}
	return nil
}

func parseIntegratedMessage(msg *imap.Message, section *imap.BodySectionName, mailboxEmail string) (string, IntegratedIMAPCode, bool) {
	body := msg.GetBody(section)
	if body == nil {
		return "", IntegratedIMAPCode{}, false
	}

	mr, err := gomail.CreateReader(body)
	if err != nil {
		return "", IntegratedIMAPCode{}, false
	}

	header := mr.Header
	subject, _ := header.Subject()
	from := integratedFirstAddress(header, "From")
	to := integratedFirstAddress(header, "To")
	cc := integratedFirstAddress(header, "Cc")
	resentTo := integratedFirstAddress(header, "Resent-To")
	deliveredTo := integratedFirstAddress(header, "Delivered-To")

	// 获取所有 X-Original-To 头（域名邮箱转发时常用到）
	xOriginalTo, _ := header.Text("X-Original-To")
	envelopeTo, _ := header.Text("Envelope-To")
	xEnvelopeTo, _ := header.Text("X-Envelope-To")
	xOriginalEnvelopeTo, _ := header.Text("X-Original-Envelope-To")
	xForwardedTo, _ := header.Text("X-Forwarded-To")
	originalRecipient, _ := header.Text("Original-Recipient")
	apparentlyTo, _ := header.Text("Apparently-To")

	plainBody, htmlBody := integratedReadBodies(mr)
	content := strings.TrimSpace(plainBody + "\n" + stripIntegratedHTML(htmlBody))
	if !looksLikeIntegratedOpenAI(subject, from, content) {
		return "", IntegratedIMAPCode{}, false
	}

	// 首先从 subject 提取验证码（最可靠）
	var code, source string
	if m := integratedCodeRegex.FindStringSubmatch(subject); len(m) >= 2 {
		code = m[1]
		source = "subject"
	} else {
		// 再从正文用精确模式提取（避免误匹配邮政编码等）
		if m := integratedBodyCodeRegex.FindStringSubmatch(content); len(m) >= 2 {
			code = m[1]
			source = "body"
		} else if m := integratedCodeRegex.FindStringSubmatch(content); len(m) >= 2 {
			// 宽泛兜底
			code = m[1]
			source = "body-fallback"
		}
	}
	if code == "" {
		return "", IntegratedIMAPCode{}, false
	}

	// 只从邮件头匹配收件人，不读正文（正文里可能有 Gmail 恢复邮箱等干扰项）
	targetEmail := pickIntegratedTargetEmail(
		mailboxEmail,
		content,
		from,
		deliveredTo,
		xOriginalTo,
		envelopeTo,
		xEnvelopeTo,
		xOriginalEnvelopeTo,
		xForwardedTo,
		originalRecipient,
		apparentlyTo,
		resentTo,
		cc,
		to,
	)
	if targetEmail == "" {
		return "", IntegratedIMAPCode{}, false
	}

	entry := IntegratedIMAPCode{
		Email:      strings.ToLower(targetEmail),
		Code:       code,
		Source:     source,
		Subject:    subject,
		From:       from,
		ReceivedAt: msg.InternalDate,
		UpdatedAt:  time.Now(),
	}
	return code, entry, true
}

func integratedFirstAddress(header gomail.Header, key string) string {
	list, err := header.AddressList(key)
	if err == nil && len(list) > 0 {
		return list[0].Address
	}
	v, _ := header.Text(key)
	return strings.TrimSpace(v)
}

func integratedReadBodies(mr *gomail.Reader) (string, string) {
	var plain strings.Builder
	var htmlBody strings.Builder
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		switch h := part.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := h.ContentType()
			b, _ := io.ReadAll(part.Body)
			if strings.HasPrefix(strings.ToLower(ct), "text/plain") {
				plain.Write(b)
				plain.WriteString("\n")
			}
			if strings.HasPrefix(strings.ToLower(ct), "text/html") {
				htmlBody.Write(b)
				htmlBody.WriteString("\n")
			}
		}
	}
	return plain.String(), htmlBody.String()
}

func stripIntegratedHTML(input string) string {
	input = html.UnescapeString(input)
	return integratedHTMLTagRe.ReplaceAllString(input, " ")
}

func looksLikeIntegratedOpenAI(subject, from, body string) bool {
	joined := strings.ToLower(subject + "\n" + from + "\n" + body)
	return strings.Contains(joined, "openai") ||
		strings.Contains(joined, "chatgpt")
}

func pickIntegratedTargetEmail(mailboxEmail, content, from string, headers ...string) string {
	candidates := collectIntegratedTargetEmails(headers...)
	for _, item := range candidates {
		if mailboxEmail != "" && item == mailboxEmail {
			continue
		}
		return item
	}
	if alias := extractIntegratedForwardedAlias(content, mailboxEmail); alias != "" {
		return alias
	}
	if alias := extractIntegratedAnonAddyAlias(from, mailboxEmail); alias != "" {
		return alias
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func collectIntegratedTargetEmails(headers ...string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(headers))
	for _, candidate := range headers {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		items := integratedEmailRegex.FindAllString(candidate, -1)
		for _, item := range items {
			item = strings.ToLower(strings.TrimSpace(item))
			if item == "" {
				continue
			}
			if shouldSkipIntegratedTargetEmail(item) {
				continue
			}
			if _, ok := seen[item]; ok {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}

func extractIntegratedForwardedAlias(content, mailboxEmail string) string {
	m := integratedForwardedToRe.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	alias := strings.ToLower(strings.TrimSpace(m[1]))
	if alias == "" || alias == mailboxEmail {
		return ""
	}
	if shouldSkipIntegratedTargetEmail(alias) {
		return ""
	}
	return alias
}

func extractIntegratedAnonAddyAlias(from, mailboxEmail string) string {
	from = strings.ToLower(strings.TrimSpace(from))
	if from == "" || from == mailboxEmail {
		return ""
	}
	m := integratedAnonAddyFromRe.FindStringSubmatch(from)
	if len(m) < 3 {
		return ""
	}
	alias := strings.TrimSpace(m[1]) + "@" + strings.TrimSpace(m[2])
	alias = strings.ToLower(alias)
	if alias == "" || alias == mailboxEmail {
		return ""
	}
	if shouldSkipIntegratedTargetEmail(alias) {
		return ""
	}
	return alias
}

func shouldSkipIntegratedTargetEmail(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return true
	}
	local := email[:at]
	domain := email[at+1:]
	if domain == "openai.com" || strings.HasSuffix(domain, ".openai.com") {
		return true
	}
	if local == "noreply" || local == "no-reply" {
		return true
	}
	return false
}

func (s *IntegratedIMAPService) upsertCode(entry IntegratedIMAPCode) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.codes[entry.Email]
	if ok && old.Code == entry.Code && old.Subject == entry.Subject {
		return false
	}
	s.codes[entry.Email] = entry

	// 前端需要 source 和 timestamp 字段
	timestampMs := entry.ReceivedAt.UnixMilli()
	if timestampMs <= 0 {
		timestampMs = entry.UpdatedAt.UnixMilli()
	}
	payload := map[string]interface{}{
		"email":       entry.Email,
		"code":        entry.Code,
		"source":      entry.Source,
		"subject":     entry.Subject,
		"from":        entry.From,
		"received_at": entry.ReceivedAt,
		"timestamp":   timestampMs,
	}
	go s.broadcast("code", payload)

	for _, ch := range s.waiters[entry.Email] {
		select {
		case ch <- entry.Code:
		default:
		}
	}
	delete(s.waiters, entry.Email)
	return true
}

func (s *IntegratedIMAPService) connect() (*imapclient.Client, error) {
	cfg := s.getConfig()
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	var c *imapclient.Client
	var err error
	if cfg.UseTLS {
		c, err = imapclient.DialTLS(addr, nil)
	} else {
		c, err = imapclient.Dial(addr)
	}
	if err != nil {
		return nil, err
	}
	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		c.Logout()
		return nil, err
	}
	return c, nil
}

func (s *IntegratedIMAPService) saveCodes() {
	codes := s.GetCodes()
	data, err := json.MarshalIndent(codes, "", "  ")
	if err != nil {
		log.Printf("save codes marshal error: %v", err)
		return
	}
	if err := os.WriteFile(s.storePath, data, 0644); err != nil {
		log.Printf("save codes write error: %v", err)
	}
}

func (s *IntegratedIMAPService) loadCodes() {
	data, err := os.ReadFile(s.storePath)
	if err != nil {
		return
	}
	var codes []IntegratedIMAPCode
	if err := json.Unmarshal(data, &codes); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, code := range codes {
		if code.Email != "" {
			s.codes[strings.ToLower(code.Email)] = code
		}
	}
}

func (s *IntegratedIMAPService) clearCodes() {
	s.mu.Lock()
	s.codes = map[string]IntegratedIMAPCode{}
	s.waiters = map[string][]chan string{}
	s.mu.Unlock()
	s.saveCodes()
}

func (s *IntegratedIMAPService) CleanupOpenAIMails() (int, error) {
	c, err := s.connect()
	if err != nil {
		return 0, err
	}
	defer c.Logout()

	_, err = c.Select("INBOX", false)
	if err != nil {
		return 0, err
	}

	criteria := imap.NewSearchCriteria()
	criteria.Since = time.Now().AddDate(0, 0, -7)
	ids, err := c.Search(criteria)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		s.clearCodes()
		s.broadcast("cleanup", map[string]interface{}{"deleted": 0, "ok": true})
		return 0, nil
	}

	seqset := new(imap.SeqSet)
	for _, id := range ids {
		seqset.AddNum(id)
	}
	messages := make(chan *imap.Message, 50)
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope}, messages)
	}()

	deleteSet := new(imap.SeqSet)
	deleted := 0
	for msg := range messages {
		if msg.Envelope == nil {
			continue
		}
		from := ""
		if len(msg.Envelope.From) > 0 {
			from = strings.ToLower(msg.Envelope.From[0].MailboxName + "@" + msg.Envelope.From[0].HostName)
		}
		subject := msg.Envelope.Subject
		if looksLikeIntegratedOpenAI(subject, from, subject) {
			deleteSet.AddNum(msg.SeqNum)
			deleted++
		}
	}
	if err := <-errCh; err != nil {
		return 0, err
	}
	if deleted > 0 {
		item := imap.FormatFlagsOp(imap.AddFlags, true)
		flags := []interface{}{imap.DeletedFlag}
		if err := c.Store(deleteSet, item, flags, nil); err != nil {
			return 0, err
		}
		if err := c.Expunge(nil); err != nil {
			return 0, err
		}
	}

	s.clearCodes()
	s.broadcast("cleanup", map[string]interface{}{"deleted": deleted, "ok": true})
	return deleted, nil
}

func (s *IntegratedIMAPService) broadcast(eventType string, payload interface{}) {
	msg, _ := json.Marshal(integratedEvent{Type: eventType, Data: payload})
	line := "data: " + string(msg) + "\n\n"

	s.mu.RLock()
	clients := make([]chan string, 0, len(s.clients))
	for ch := range s.clients {
		clients = append(clients, ch)
	}
	s.mu.RUnlock()

	for _, ch := range clients {
		select {
		case ch <- line:
		default:
		}
	}
}

func integratedWriteCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func (s *IntegratedIMAPService) handleConfig(w http.ResponseWriter, r *http.Request) {
	integratedWriteCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodGet {
		cfg := s.getConfig()
		_ = json.NewEncoder(w).Encode(cfg)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg IntegratedIMAPConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.SetConfig(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (s *IntegratedIMAPService) handleCodes(w http.ResponseWriter, r *http.Request) {
	integratedWriteCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"codes": s.GetCodes(),
	})
}

func integratedReadRequestValue(r *http.Request, key string) string {
	if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
		return v
	}
	if err := r.ParseForm(); err == nil {
		if v := strings.TrimSpace(r.FormValue(key)); v != "" {
			return v
		}
	}
	if r.Body == nil {
		return ""
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil || len(data) == 0 {
		return ""
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	if raw, ok := payload[key]; ok {
		if text, ok := raw.(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func integratedReadTimeoutSeconds(r *http.Request) int {
	for _, key := range []string{"timeout", "timeout_seconds", "wait_seconds"} {
		text := integratedReadRequestValue(r, key)
		if text == "" {
			continue
		}
		var seconds int
		_, _ = fmt.Sscanf(text, "%d", &seconds)
		if seconds > 0 {
			return seconds
		}
	}
	return 0
}

func (s *IntegratedIMAPService) handleGetCode(w http.ResponseWriter, r *http.Request) {
	integratedWriteCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	email := strings.ToLower(strings.TrimSpace(integratedReadRequestValue(r, "email")))
	if email == "" {
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}

	if entry, ok := s.PeekCode(email); ok {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    true,
			"email": entry.Email,
			"code":  entry.Code,
			"item":  entry,
		})
		return
	}

	timeoutSeconds := integratedReadTimeoutSeconds(r)
	if timeoutSeconds <= 0 {
		timeoutSeconds = 120
	}
	code, err := s.WaitForCode(email, time.Duration(timeoutSeconds)*time.Second, time.Time{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	entry, _ := s.PeekCode(email)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"email": email,
		"code":  code,
		"item":  entry,
	})
}

func (s *IntegratedIMAPService) handleConsume(w http.ResponseWriter, r *http.Request) {
	integratedWriteCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	email := integratedReadRequestValue(r, "email")
	code := integratedReadRequestValue(r, "code")
	entry, err := s.ConsumeCode(email, code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"email": entry.Email,
		"code":  entry.Code,
		"item":  entry,
	})
}

func (s *IntegratedIMAPService) handleEvents(w http.ResponseWriter, r *http.Request) {
	integratedWriteCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 32)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.mu.Unlock()
	}()

	ch <- "data: {\"type\":\"connected\",\"data\":{\"ok\":true}}\n\n"
	for _, code := range s.GetCodes() {
		payload, _ := json.Marshal(integratedEvent{Type: "code", Data: code})
		ch <- "data: " + string(payload) + "\n\n"
	}
	flusher.Flush()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case msg := <-ch:
			_, _ = w.Write([]byte(msg))
			flusher.Flush()
		}
	}
}

func (s *IntegratedIMAPService) handleCleanup(w http.ResponseWriter, r *http.Request) {
	integratedWriteCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	deleted, err := s.CleanupOpenAIMails()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "deleted": deleted})
}

func (s *IntegratedIMAPService) handleStatus(w http.ResponseWriter, r *http.Request) {
	integratedWriteCORS(w)
	cfg := s.getConfig()
	codes := s.GetCodes()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":         true,
		"configured": cfg.Host != "" && cfg.Username != "",
		"running":    true,
		"count":      len(codes),
		"username":   cfg.Username,
		"host":       cfg.Host,
		"port":       cfg.Port,
	})
}
