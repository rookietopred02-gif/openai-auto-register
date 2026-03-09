package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	manualCodeInputOnce sync.Once
	manualCodeOnlyRe    = regexp.MustCompile(`^\d{4,8}$`)
	manualMailboxOnlyRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

	manualMailboxMu      sync.Mutex
	manualMailboxWaiters []chan string
)

func startManualCodeInput() {
	manualCodeInputOnce.Do(func() {
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}

				// Temp Mail 兜底：当 API 限流时，允许在终端输入邮箱地址继续流程
				if isManualMailboxLine(line) {
					if injectManualMailbox(line) {
						broadcast(fmt.Sprintf("    ✅ 手动邮箱已注入: %s", strings.ToLower(strings.TrimSpace(line))), "success")
						continue
					}
				}

				email, code, err := parseManualCodeInput(line, integratedIMAP.PendingWaiterEmails())
				if err != nil {
					log.Printf("[manual] %v", err)
					continue
				}

				entry, err := integratedIMAP.InjectManualCode(email, code, "manual")
				if err != nil {
					log.Printf("[manual] 注入验证码失败: %v", err)
					continue
				}

				broadcast(fmt.Sprintf("    ✅ 验证码: %s (终端手动输入:%s)", entry.Code, entry.Email), "success")
			}
			if err := scanner.Err(); err != nil {
				log.Printf("[manual] stdin 读取停止: %v", err)
			}
		}()
	})
}

func parseManualCodeInput(line string, pending []string) (string, string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", fmt.Errorf("验证码输入为空")
	}

	if strings.Contains(line, "----") {
		parts := strings.SplitN(line, "----", 2)
		email := strings.ToLower(strings.TrimSpace(parts[0]))
		code := strings.TrimSpace(parts[1])
		if email == "" || !manualCodeOnlyRe.MatchString(code) {
			return "", "", fmt.Errorf("手动验证码格式错误，请输入 email----123456")
		}
		return email, code, nil
	}

	fields := strings.Fields(line)
	if len(fields) == 2 && strings.Contains(fields[0], "@") && manualCodeOnlyRe.MatchString(fields[1]) {
		return strings.ToLower(strings.TrimSpace(fields[0])), strings.TrimSpace(fields[1]), nil
	}

	if manualCodeOnlyRe.MatchString(line) {
		switch len(pending) {
		case 0:
			return "", "", fmt.Errorf("当前没有等待中的邮箱，请输入 email----123456")
		case 1:
			return pending[0], line, nil
		default:
			return "", "", fmt.Errorf("当前有多个邮箱等待验证码，请输入 email----123456，待输入邮箱: %s", strings.Join(pending, ", "))
		}
	}

	return "", "", fmt.Errorf("无法识别手动验证码输入，请输入 123456 或 email----123456")
}

func manualCodeHint(email string) {
	pending := integratedIMAP.PendingWaiterEmails()
	hint := fmt.Sprintf("    ⌨️ 终端可手动输入验证码：直接输入 6 位数字，或输入 %s----123456", email)
	if len(pending) > 1 {
		hint = "    ⌨️ 当前有多个邮箱等待验证码，请在终端输入 email----123456"
	}
	broadcast(hint, "dim")
}

func isManualMailboxLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.Contains(line, "----") {
		return false
	}
	if strings.Contains(line, " ") || strings.Contains(line, "\t") {
		return false
	}
	return manualMailboxOnlyRe.MatchString(line)
}

func injectManualMailbox(mailbox string) bool {
	mailbox = strings.ToLower(strings.TrimSpace(mailbox))
	if !manualMailboxOnlyRe.MatchString(mailbox) {
		return false
	}
	manualMailboxMu.Lock()
	defer manualMailboxMu.Unlock()
	if len(manualMailboxWaiters) == 0 {
		return false
	}
	ch := manualMailboxWaiters[0]
	manualMailboxWaiters = manualMailboxWaiters[1:]
	select {
	case ch <- mailbox:
	default:
	}
	return true
}

func waitManualMailboxInput(timeout time.Duration) (string, error) {
	ch := make(chan string, 1)
	manualMailboxMu.Lock()
	manualMailboxWaiters = append(manualMailboxWaiters, ch)
	pending := len(manualMailboxWaiters)
	manualMailboxMu.Unlock()

	broadcast(fmt.Sprintf("    ⌨️ Temp Mail 限流，请在终端输入临时邮箱（例如 xxx@xxx.com），等待中... (队列:%d)", pending), "warning")

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case mailbox := <-ch:
		return mailbox, nil
	case <-timer.C:
		manualMailboxMu.Lock()
		for i := range manualMailboxWaiters {
			if manualMailboxWaiters[i] == ch {
				manualMailboxWaiters = append(manualMailboxWaiters[:i], manualMailboxWaiters[i+1:]...)
				break
			}
		}
		manualMailboxMu.Unlock()
		return "", fmt.Errorf("等待手动输入临时邮箱超时")
	}
}

func (s *IntegratedIMAPService) PendingWaiterEmails() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]string, 0, len(s.waiters))
	for email, waiters := range s.waiters {
		if len(waiters) == 0 {
			continue
		}
		out = append(out, email)
	}
	sort.Strings(out)
	return out
}

func (s *IntegratedIMAPService) InjectManualCode(email, code, source string) (IntegratedIMAPCode, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	code = strings.TrimSpace(code)
	if email == "" {
		return IntegratedIMAPCode{}, fmt.Errorf("邮箱不能为空")
	}
	if !manualCodeOnlyRe.MatchString(code) {
		return IntegratedIMAPCode{}, fmt.Errorf("验证码格式错误")
	}

	now := time.Now()
	entry := IntegratedIMAPCode{
		Email:      email,
		Code:       code,
		Source:     firstNonEmpty(strings.TrimSpace(source), "manual"),
		Subject:    "manual-terminal-input",
		From:       "stdin",
		ReceivedAt: now,
		UpdatedAt:  now,
	}
	changed := s.upsertCode(entry)
	if changed {
		s.saveCodes()
	}
	return entry, nil
}
