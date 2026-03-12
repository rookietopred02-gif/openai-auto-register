package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDebugStep3Body(t *testing.T) {
	if os.Getenv("DEBUG_STEP3") != "1" {
		t.Skip("set DEBUG_STEP3=1 to enable")
	}

	zero := 0
	configureTempMailRuntime("", &TempMailConfig{Count: 1, Password: defaultRegisterPassword, NextDelaySeconds: &zero})
	if err := ensureTempMailReady(); err != nil {
		t.Fatalf("ensure temp mail ready: %v", err)
	}
	tempMailService.mu.Lock()
	tempMailService.firstServed = true
	tempMailService.mu.Unlock()
	email, err := acquireTempMailbox()
	if err != nil {
		t.Fatalf("acquire temp mailbox: %v", err)
	}
	t.Logf("mailbox=%s", email)

	cwd, _ := os.Getwd()
	t.Logf("cwd=%s", cwd)

	httpClient, err := NewHTTPClient("")
	if err != nil {
		t.Fatalf("new http client: %v", err)
	}

	authURL, _, _ := createOAuthParams()
	if status, _, err := httpClient.Get(authURL); err != nil || status < 200 || status >= 400 {
		t.Fatalf("oauth get failed: status=%d err=%v", status, err)
	}

	deviceID := httpClient.GetCookie("oai-did")
	sentinelBody := map[string]interface{}{"p": "", "id": deviceID, "flow": "authorize_continue"}
	sStatus, sBody, err := httpClient.PostJSON(OAISentinelURL, sentinelBody, map[string]string{
		"Origin":  "https://sentinel.openai.com",
		"Referer": "https://sentinel.openai.com/backend-api/sentinel/frame.html",
	})
	if err != nil || sStatus < 200 || sStatus >= 300 {
		t.Fatalf("sentinel failed: status=%d err=%v body=%s", sStatus, err, sBody)
	}

	var sentinelResp map[string]interface{}
	_ = json.Unmarshal([]byte(sBody), &sentinelResp)
	sentinelToken, _ := sentinelResp["token"].(string)
	sentinelHeader, _ := json.Marshal(map[string]interface{}{
		"p": "", "t": "", "c": sentinelToken, "id": deviceID, "flow": "authorize_continue",
	})

	signupBody := map[string]interface{}{
		"username":    map[string]interface{}{"value": email, "kind": "email"},
		"screen_hint": "signup",
	}
	s3Status, s3Body, err := httpClient.PostJSON(OAISignupURL, signupBody, map[string]string{
		"Referer":               "https://auth.openai.com/create-account",
		"openai-sentinel-token": string(sentinelHeader),
	})
	if err != nil || s3Status < 200 || s3Status >= 300 {
		t.Fatalf("step3 failed: status=%d err=%v body=%s", s3Status, err, s3Body)
	}

	t.Logf("step3=%s", s3Body)

	var step3 map[string]interface{}
	_ = json.Unmarshal([]byte(s3Body), &step3)
	continueURL, _ := step3["continue_url"].(string)
	if continueURL == "" {
		t.Fatalf("missing continue_url")
	}

	if status, body, err := httpClient.Get(continueURL); err != nil {
		t.Fatalf("get continue_url failed: %v", err)
	} else {
		_ = os.WriteFile(filepath.Join(cwd, "_debug_continue.html"), []byte(body), 0644)
		t.Logf("continue_url_status=%d body=%s", status, body[:min(200, len(body))])
		for _, needle := range []string{"one-time code", "passwordless", "email-verification", "__NEXT_DATA__"} {
			if idx := strings.Index(strings.ToLower(body), strings.ToLower(needle)); idx >= 0 {
				start := idx - 180
				if start < 0 {
					start = 0
				}
				end := min(len(body), idx+420)
				t.Logf("continue_url_%s=%s", needle, body[start:end])
			}
		}
	}

	if status, body, err := httpClient.PostJSON(OAIUserRegisterURL, map[string]interface{}{
		"username": email,
		"password": defaultRegisterPassword,
	}, map[string]string{
		"Referer": "https://auth.openai.com/create-account/password",
	}); err != nil {
		t.Fatalf("user register failed: %v", err)
	} else {
		t.Logf("user_register_status=%d body=%s", status, body)

		var step4 map[string]interface{}
		_ = json.Unmarshal([]byte(body), &step4)
		nextURL, _ := step4["continue_url"].(string)
		t.Logf("step4_page_type=%s continue_url=%s", extractPageType(step4), nextURL)

		if nextURL != "" {
			if status, body, err := httpClient.Get(nextURL); err != nil {
				t.Fatalf("get step4 continue_url failed: %v", err)
			} else {
				_ = os.WriteFile(filepath.Join(cwd, "_debug_email_verification.html"), []byte(body), 0644)
				t.Logf("step4_continue_status=%d body=%s", status, body[:min(200, len(body))])
			}
		}
	}

	seen := map[string]struct{}{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		code, err := tempMailService.FindCode(email, time.Now().Add(-2*time.Minute), seen)
		if err == nil && code != "" {
			t.Logf("code=%s", code)
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Log("code not received within debug window")
}
