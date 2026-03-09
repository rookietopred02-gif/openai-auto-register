package main

import (
	"encoding/json"
	"testing"
)

func TestStartRequestAcceptsDomainMailUsernamePassword(t *testing.T) {
	payload := []byte(`{
		"accounts":"alias@example.com----pw",
		"domain_mail":{
			"host":"imap.gmail.com",
			"port":993,
			"username":"catchall@gmail.com",
			"password":"app-password"
		}
	}`)

	var req StartRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal start request: %v", err)
	}
	if req.DomainMail == nil {
		t.Fatal("domain_mail should be present")
	}
	if got := req.DomainMail.IMAPUser(); got != "catchall@gmail.com" {
		t.Fatalf("unexpected IMAP user: %q", got)
	}
	if got := req.DomainMail.IMAPPass(); got != "app-password" {
		t.Fatalf("unexpected IMAP password: %q", got)
	}
}

func TestStartRequestAcceptsLegacyDomainMailUserPass(t *testing.T) {
	payload := []byte(`{
		"accounts":"alias@example.com----pw",
		"domain_mail":{
			"host":"imap.gmail.com",
			"port":993,
			"user":"catchall@gmail.com",
			"pass":"app-password"
		}
	}`)

	var req StartRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal start request: %v", err)
	}
	if req.DomainMail == nil {
		t.Fatal("domain_mail should be present")
	}
	if got := req.DomainMail.IMAPUser(); got != "catchall@gmail.com" {
		t.Fatalf("unexpected IMAP user: %q", got)
	}
	if got := req.DomainMail.IMAPPass(); got != "app-password" {
		t.Fatalf("unexpected IMAP password: %q", got)
	}
}

func TestPickIntegratedTargetEmailPrefersAliasOverMailbox(t *testing.T) {
	content := `
		This email was sent to auto123@imlegitarena.anonaddy.com from noreply@openai.com
		and has been forwarded by AnonAddy.
	`

	got := pickIntegratedTargetEmail(
		"gifulin.tw@gmail.com",
		content,
		"",
		"gifulin.tw@gmail.com",
	)
	if got != "auto123@imlegitarena.anonaddy.com" {
		t.Fatalf("unexpected forwarded alias: %q", got)
	}
}

func TestPickIntegratedTargetEmailFallsBackToMailboxForDirectInbox(t *testing.T) {
	got := pickIntegratedTargetEmail(
		"gifulin.tw@gmail.com",
		"",
		"",
		"gifulin.tw@gmail.com",
	)
	if got != "gifulin.tw@gmail.com" {
		t.Fatalf("unexpected direct inbox target: %q", got)
	}
}

func TestPickIntegratedTargetEmailExtractsAnonAddyAliasFromFromHeader(t *testing.T) {
	got := pickIntegratedTargetEmail(
		"gifulin.tw@gmail.com",
		"",
		"openaixbieit945+noreply=tm.openai.com@gifulin.anonaddy.com",
		"gifulin.tw@gmail.com",
	)
	if got != "openaixbieit945@gifulin.anonaddy.com" {
		t.Fatalf("unexpected alias from AnonAddy sender: %q", got)
	}
}

func TestLooksLikeIntegratedOpenAIRejectsGenericVerificationMails(t *testing.T) {
	if looksLikeIntegratedOpenAI(
		"Brave Search API login attempt",
		"search-api@brave.com",
		"Your verification code is 486151",
	) {
		t.Fatal("generic verification mail should not be treated as OpenAI mail")
	}
}

func TestLoadIntegratedIMAPConfigFromText(t *testing.T) {
	data := []byte("IMAP 服务器 = imap.gmail.com\nIMAP 端口 = 993\nIMAP 账号 = gifulin.tw@gmail.com\nIMAP 密码 = app-password\n")

	cfg, ok := loadIntegratedIMAPConfigFromText(data)
	if !ok {
		t.Fatal("expected text config to be recognized")
	}
	if cfg.Host != "imap.gmail.com" || cfg.Port != 993 || cfg.Username != "gifulin.tw@gmail.com" || cfg.Password != "app-password" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
}
