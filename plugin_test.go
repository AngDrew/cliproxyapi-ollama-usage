package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const creditBalance = `{"included":{"balance_usd":72.5,"allowance_usd":100,"period":{"from":"2026-09-15T09:30:00Z","until":"2026-10-15T09:30:00Z"}},"purchased":{"balance_usd":25}}`
const usageBody = `{"range":"24h","totals":{"usage_usd":0.01718,"request_count":15,"input_tokens":106000,"cached_input_tokens":46000,"output_tokens":13600}}`

func TestNormalize(t *testing.T) {
	q, err := normalize([]byte(creditBalance), []byte(usageBody))
	if err != nil {
		t.Fatal(err)
	}
	b := q.Groups[0].Buckets[0]
	if b.RemainingFraction != 0.725 || b.ResetTime != "2026-10-15T09:30:00Z" {
		t.Fatalf("bad credit window: %+v", b)
	}
	if len(q.Summary) != 8 || q.Summary[3].Value != 0.01718 || q.Summary[3].Currency != "USD" {
		t.Fatalf("bad metrics: %+v", q.Summary)
	}
	legacy := `{"included":{"session":{"remaining_percent":75,"resets_at":"2026-10-01T07:00:00Z"},"weekly":{"remaining_percent":40,"resets_at":"2026-10-05T00:00:00Z"}},"purchased":{"balance_usd":0}}`
	q, err = normalize([]byte(legacy), []byte(usageBody))
	if err != nil || q.Groups[0].Buckets[0].RemainingFraction != 0.75 || q.Groups[0].Buckets[1].RemainingFraction != 0.4 {
		t.Fatalf("bad legacy quota: %+v %v", q, err)
	}
	q, err = normalize([]byte(strings.Replace(creditBalance, `"allowance_usd":100`, `"allowance_usd":0`, 1)), []byte(usageBody))
	if err != nil || len(q.Groups) != 0 || q.Summary[0].Value != 72.5 {
		t.Fatal("zero allowance must retain money without inventing a percentage")
	}
	for _, bad := range []string{"{}", "null", `{"included":{},"purchased":{"balance_usd":0}}`, strings.Replace(creditBalance, "100", "-1", 1), strings.Replace(creditBalance, "2026-10-15", "bad", 1), strings.Replace(legacy, "75", "101", 1)} {
		if _, err := normalize([]byte(bad), []byte(usageBody)); err == nil {
			t.Fatalf("accepted invalid balance: %s", bad)
		}
	}
	for _, bad := range []string{"{}", "null", strings.Replace(usageBody, "24h", "7d", 1), strings.Replace(usageBody, "15,", "-1,", 1), strings.Replace(usageBody, "106000", "0.5", 1)} {
		if _, err := normalize([]byte(creditBalance), []byte(bad)); err == nil {
			t.Fatalf("accepted invalid usage: %s", bad)
		}
	}
}

func fakeHost(t *testing.T, calls *[]string, status int) hostCall {
	return func(method string, payload any, out any) error {
		raw, _ := json.Marshal(payload)
		var request map[string]any
		_ = json.Unmarshal(raw, &request)
		switch method {
		case "host.http.operation_open":
			return json.Unmarshal([]byte(`{"operation_id":"operation"}`), out)
		case "host.http.cancel":
			return nil
		case "host.http.do":
			url, _ := request["url"].(string)
			if !strings.HasPrefix(url, "https://ollama.com/api/") || request["host_callback_id"] != "callback" || request["operation_id"] != "operation" || request["method"] != "GET" {
				t.Fatalf("unsafe host request: %s", method)
			}
			headers := request["headers"].(map[string]any)
			if headers["Authorization"].([]any)[0] != "Bearer test-key" {
				t.Fatal("missing bearer credential")
			}
			*calls = append(*calls, url)
			body := creditBalance
			if strings.Contains(url, "/usage?range=24h") {
				body = usageBody
			}
			if status != 200 {
				body = `{"error":"SECRET-DO-NOT-ECHO"}`
			}
			raw, _ := json.Marshal(struct {
				StatusCode int
				Headers    http.Header
				Body       []byte
			}{status, http.Header{"Retry-After": {"120"}}, []byte(body)})
			return json.Unmarshal(raw, out)
		default:
			return errors.New("unexpected callback")
		}
	}
}

func TestPluginRoundTripAndCache(t *testing.T) {
	cache.entries = make(map[[32]byte]cacheEntry)
	var calls []string
	host := fakeHost(t, &calls, 200)
	for _, method := range []string{"plugin.register", "plugin.reconfigure", "quota.identifier", "quota.describe"} {
		if _, err := dispatch(method, nil, host); err != nil {
			t.Fatal(err)
		}
	}
	req := []byte(`{"provider":"ollama-usage","auth_index":"index","host_callback_id":"callback","metadata":{"api_key":"test-key"}}`)
	result, err := dispatch("quota.fetch", req, host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch("quota.fetch", req, host); err != nil || len(calls) != 2 {
		t.Fatal("refresh should use the 60-second cache")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "test-key") {
		t.Fatal("credential leaked in quota response")
	}
	if _, err := dispatch("quota.reset", req, host); err == nil || len(calls) != 2 {
		t.Fatal("reset must fail without HTTP")
	}
	if _, err := dispatch("quota.fetch", []byte("{}"), host); err == nil {
		t.Fatal("accepted invalid credential")
	}

	cache.entries = make(map[[32]byte]cacheEntry)
	calls = nil
	limitedHost := fakeHost(t, &calls, 429)
	_, err = dispatch("quota.fetch", req, limitedHost)
	if err == nil || !strings.Contains(err.Error(), "120 seconds") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("bad rate limit error: %v", err)
	}
	_, _ = dispatch("quota.fetch", req, limitedHost)
	if len(calls) != 1 {
		t.Fatal("Retry-After backoff not respected")
	}
	for _, entry := range cache.entries {
		if time.Until(entry.until) < 119*time.Second {
			t.Fatal("backoff too short")
		}
	}

	cache.entries = make(map[[32]byte]cacheEntry)
	for _, status := range []int{401, 403, 500} {
		_, err = getJSON("/api/balance", "test-key", "callback", fakeHost(t, &calls, status))
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("upstream errors must be sanitized")
		}
	}
}

func TestCredentialEnvironment(t *testing.T) {
	t.Setenv("OLLAMA_TEST_KEY", "env-key")
	key, err := credentialKey(quotaRequest{Metadata: map[string]any{"api_key_env": "OLLAMA_TEST_KEY"}})
	if err != nil || key != "env-key" {
		t.Fatal("environment credential not resolved")
	}
	for _, data := range []map[string]any{{}, {"api_key_env": "bad name"}, {"api_key": "x\r\ny"}} {
		if _, err := credentialKey(quotaRequest{Metadata: data}); err == nil {
			t.Fatal("accepted invalid credential")
		}
	}
}
