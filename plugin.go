package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const pluginID = "ollama-usage"

var version = "0.1.1"

type hostCall func(string, any, any) error

type quotaRequest struct {
	Provider       string            `json:"provider"`
	AuthIndex      string            `json:"auth_index"`
	Metadata       map[string]any    `json:"metadata"`
	Attributes     map[string]string `json:"attributes"`
	HostCallbackID string            `json:"host_callback_id"`
}
type metric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Format   string  `json:"format"`
	Currency string  `json:"currency,omitempty"`
}
type bucket struct {
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}
type group struct {
	DisplayName string   `json:"displayName"`
	Buckets     []bucket `json:"buckets"`
}
type quota struct {
	Subscription map[string]string `json:"subscription"`
	Summary      []metric          `json:"summary"`
	Groups       []group           `json:"groups"`
}
type cacheEntry struct {
	quota quota
	err   error
	until time.Time
}

var cache = struct {
	sync.Mutex
	entries map[[32]byte]cacheEntry
}{entries: make(map[[32]byte]cacheEntry)}
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func dispatch(method string, raw []byte, host hostCall) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return map[string]any{
			"schema_version": 1,
			"metadata": map[string]any{"Name": "Ollama Usage", "Version": version, "Author": "AngDrew",
				"GitHubRepository": "https://github.com/AngDrew/cliproxyapi-ollama-usage"},
			"capabilities": map[string]bool{"quota_provider": true},
		}, nil
	case "quota.identifier":
		return map[string]string{"identifier": pluginID}, nil
	case "quota.describe":
		return map[string]any{"supported_providers": []string{pluginID}, "display_name": "Ollama Cloud", "supports_reset": false}, nil
	case "quota.reset":
		return nil, errors.New("Ollama usage is read-only; resets are not supported")
	case "quota.fetch":
		var req quotaRequest
		if json.Unmarshal(raw, &req) != nil || req.Provider != pluginID || req.AuthIndex == "" || req.HostCallbackID == "" {
			return nil, errors.New("invalid Ollama monitor credential")
		}
		key, err := credentialKey(req)
		if err != nil {
			return nil, err
		}
		return fetchQuota(key, req.HostCallbackID, host)
	default:
		return nil, errors.New("unsupported plugin method")
	}
}

func credentialKey(req quotaRequest) (string, error) {
	key, _ := req.Metadata["api_key"].(string)
	variable, _ := req.Metadata["api_key_env"].(string)
	if variable != "" {
		if !envName.MatchString(variable) {
			return "", errors.New("invalid api_key_env name")
		}
		key = os.Getenv(variable)
	}
	if key == "" {
		key = req.Attributes["api_key"]
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("Ollama API key is missing or invalid; set api_key or api_key_env in the monitor auth file")
	}
	return key, nil
}

func fetchQuota(key, callbackID string, host hostCall) (quota, error) {
	// ponytail: refreshes serialize across accounts; use per-key locks if many accounts make this noticeable.
	cache.Lock()
	defer cache.Unlock()
	now := time.Now()
	digest := sha256.Sum256([]byte(key))
	if cached, ok := cache.entries[digest]; ok && now.Before(cached.until) {
		return cached.quota, cached.err
	}
	for k, v := range cache.entries {
		if !now.Before(v.until) {
			delete(cache.entries, k)
		}
	}
	balance, err := getJSON("/api/balance", key, callbackID, host)
	var usage []byte
	if err == nil {
		usage, err = getJSON("/api/usage?range=24h", key, callbackID, host)
	}
	var result quota
	if err == nil {
		result, err = normalize(balance, usage)
	}
	ttl := time.Minute
	if err != nil {
		ttl = 5 * time.Second
	}
	var limited *rateLimitError
	if errors.As(err, &limited) {
		ttl = limited.wait
	}
	// Bound retained credentials even when a caller cycles keys.
	if len(cache.entries) >= 256 {
		clear(cache.entries)
	}
	cache.entries[digest] = cacheEntry{result, err, time.Now().Add(ttl)}
	return result, err
}

type rateLimitError struct{ wait time.Duration }

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("Ollama rate limit reached; retry after %d seconds", int(e.wait.Seconds()))
}

func getJSON(path, key, callbackID string, host hostCall) ([]byte, error) {
	var operation struct {
		ID string `json:"operation_id"`
	}
	if err := host("host.http.operation_open", map[string]string{"host_callback_id": callbackID}, &operation); err != nil || operation.ID == "" {
		return nil, errors.New("could not start Ollama HTTP request; use a current CLIProxyAPI v8 build")
	}
	cancel := func() {
		_ = host("host.http.cancel", map[string]string{"operation_id": operation.ID, "host_callback_id": callbackID}, nil)
	}
	canceled := make(chan struct{})
	deadline := time.AfterFunc(10*time.Second, func() {
		defer close(canceled)
		cancel()
	})
	defer func() {
		if !deadline.Stop() {
			<-canceled // Never leave a host callback running after this plugin RPC returns.
		}
	}()
	var response struct {
		StatusCode int
		Headers    http.Header
		Body       []byte
	}
	err := host("host.http.do", map[string]any{
		"host_callback_id": callbackID, "operation_id": operation.ID,
		"method": "GET", "url": "https://ollama.com" + path,
		"headers": map[string][]string{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}},
	}, &response)
	if err != nil {
		cancel()
		return nil, errors.New("Ollama request failed or timed out")
	}
	if response.StatusCode == 429 {
		wait := 60
		if seconds, err := strconv.ParseUint(response.Headers.Get("Retry-After"), 10, 31); err == nil && int(seconds) > wait {
			wait = int(seconds)
		}
		return nil, &rateLimitError{time.Duration(wait) * time.Second}
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama %s returned HTTP %d", strings.Split(path, "?")[0], response.StatusCode)
	}
	if len(response.Body) > 1<<20 || !json.Valid(response.Body) {
		return nil, errors.New("invalid Ollama JSON response")
	}
	return response.Body, nil
}

func normalize(balanceJSON, usageJSON []byte) (quota, error) {
	var balance struct {
		Included *struct {
			Balance   *float64 `json:"balance_usd"`
			Allowance *float64 `json:"allowance_usd"`
			Period    struct {
				From  string `json:"from"`
				Until string `json:"until"`
			} `json:"period"`
			Session *struct {
				Remaining *float64 `json:"remaining_percent"`
				Reset     string   `json:"resets_at"`
			} `json:"session"`
			Weekly *struct {
				Remaining *float64 `json:"remaining_percent"`
				Reset     string   `json:"resets_at"`
			} `json:"weekly"`
		} `json:"included"`
		Purchased *struct {
			Balance *float64 `json:"balance_usd"`
		} `json:"purchased"`
	}
	var usage struct {
		Range  string `json:"range"`
		Totals *struct {
			Spend    *float64 `json:"usage_usd"`
			Requests *float64 `json:"request_count"`
			Input    *float64 `json:"input_tokens"`
			Cached   *float64 `json:"cached_input_tokens"`
			Output   *float64 `json:"output_tokens"`
		} `json:"totals"`
	}
	result := quota{Subscription: map[string]string{"plan": "Ollama Cloud"}, Groups: []group{}, Summary: []metric{}}
	invalid := errors.New("unrecognized Ollama balance or usage response")
	if json.Unmarshal(balanceJSON, &balance) != nil || balance.Included == nil ||
		balance.Purchased == nil || !finite(balance.Purchased.Balance) ||
		json.Unmarshal(usageJSON, &usage) != nil || usage.Range != "24h" || usage.Totals == nil {
		return result, invalid
	}
	add := func(key, label string, value float64, currency bool) {
		m := metric{Key: key, Label: label, Value: value, Format: "number"}
		if currency {
			m.Format, m.Currency = "currency", "USD"
		}
		result.Summary = append(result.Summary, m)
	}
	included := balance.Included
	if included.Balance != nil || included.Allowance != nil {
		from, e1 := time.Parse(time.RFC3339, included.Period.From)
		until, e2 := time.Parse(time.RFC3339, included.Period.Until)
		if !finite(included.Balance) || !finite(included.Allowance) || *included.Allowance < 0 ||
			e1 != nil || e2 != nil || !until.After(from) {
			return result, invalid
		}
		add("included_balance_usd", "Included balance", *included.Balance, true)
		add("included_allowance_usd", "Period allowance", *included.Allowance, true)
		if *included.Allowance > 0 {
			result.Groups = append(result.Groups, group{"Included credits", []bucket{{
				Window: "Billing period", RemainingFraction: math.Max(0, math.Min(1, *included.Balance / *included.Allowance)),
				ResetTime: included.Period.Until, Description: "Included credits; period starts " + included.Period.From,
			}}})
		}
	} else {
		if included.Session == nil || included.Weekly == nil {
			return result, invalid
		}
		limits := group{DisplayName: "Included limits"}
		for _, window := range []struct {
			name      string
			remaining *float64
			reset     string
		}{
			{"5h", included.Session.Remaining, included.Session.Reset}, {"7d", included.Weekly.Remaining, included.Weekly.Reset},
		} {
			if !finite(window.remaining) || *window.remaining < 0 || *window.remaining > 100 {
				return result, invalid
			}
			if _, err := time.Parse(time.RFC3339, window.reset); err != nil {
				return result, invalid
			}
			limits.Buckets = append(limits.Buckets, bucket{
				Window: window.name, RemainingFraction: *window.remaining / 100, ResetTime: window.reset,
			})
		}
		result.Groups = append(result.Groups, limits)
	}
	add("purchased_balance_usd", "Purchased balance", *balance.Purchased.Balance, true)
	total := usage.Totals
	if !count(total.Requests) {
		return result, invalid
	}
	if total.Spend != nil {
		if !finite(total.Spend) || *total.Spend < 0 {
			return result, invalid
		}
		add("usage_usd_24h", "Spend · 24h", *total.Spend, true)
	}
	add("request_count_24h", "Requests · 24h", *total.Requests, false)
	for _, field := range []struct {
		key, label string
		value      *float64
	}{
		{"input_tokens_24h", "Input tokens · 24h", total.Input},
		{"cached_input_tokens_24h", "Cached input tokens · 24h", total.Cached},
		{"output_tokens_24h", "Output tokens · 24h", total.Output},
	} {
		if field.value != nil {
			if !count(field.value) {
				return result, invalid
			}
			add(field.key, field.label, *field.value, false)
		}
	}
	return result, nil
}
func finite(v *float64) bool { return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) }
func count(v *float64) bool  { return finite(v) && *v >= 0 && math.Trunc(*v) == *v }
