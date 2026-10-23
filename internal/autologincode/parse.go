package autologincode

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var sixDigits = regexp.MustCompile(`^[0-9]{6}$`)
var digitRuns = regexp.MustCompile(`[0-9]+`)

type candidates struct {
	codes         map[string]struct{}
	stamp         *time.Time
	stampPriority int
	invalid       bool
}

func parseResponse(body []byte) (Result, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return Result{}, ErrNoCode
	}
	if body[0] != '{' && body[0] != '[' {
		if sixDigits.Match(body) {
			return Result{Code: string(body)}, nil
		}
		if bytes.ContainsAny(body, "<>") {
			return Result{}, ErrInvalidResponse
		}
		code, ambiguous := textCode(string(body))
		if ambiguous {
			return Result{}, ErrInvalidResponse
		}
		if code == "" {
			return Result{}, ErrNoCode
		}
		return Result{Code: code}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return Result{}, ErrInvalidResponse
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Result{}, ErrInvalidResponse
	}
	root, ok := value.(map[string]any)
	if !ok {
		return Result{}, ErrInvalidResponse
	}
	c := candidates{codes: make(map[string]struct{})}
	c.collect(root, 0)
	if c.invalid || len(c.codes) > 1 {
		return Result{}, ErrInvalidResponse
	}
	for code := range c.codes {
		return Result{Code: code, ReceivedAt: c.stamp}, nil
	}
	return Result{}, ErrNoCode
}

func (c *candidates) collect(obj map[string]any, depth int) {
	if depth > 4 {
		c.invalid = true
		return
	}
	if failedResponse(obj) {
		return
	}
	for key, value := range obj {
		normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		switch normalized {
		case "code", "otp", "verificationcode", "verifycode", "smscode", "authcode":
			text, ok := value.(string)
			if !ok && normalized != "code" {
				if number, numeric := value.(json.Number); numeric {
					text, ok = number.String(), true
				}
			}
			if ok && sixDigits.MatchString(strings.TrimSpace(text)) {
				c.codes[strings.TrimSpace(text)] = struct{}{}
			}
		case "message", "msg", "sms", "smstext", "content", "text":
			if text, ok := value.(string); ok {
				code, ambiguous := textCode(text)
				if ambiguous {
					c.invalid = true
				}
				if code != "" {
					c.codes[code] = struct{}{}
				}
			}
		case "data", "result":
			if nested, ok := value.(map[string]any); ok {
				c.collect(nested, depth+1)
			}
			if text, ok := value.(string); ok {
				if sixDigits.MatchString(strings.TrimSpace(text)) {
					c.codes[strings.TrimSpace(text)] = struct{}{}
				} else {
					code, ambiguous := textCode(text)
					if ambiguous {
						c.invalid = true
					}
					if code != "" {
						c.codes[code] = struct{}{}
					}
				}
			}
		case "receivedat", "receivedtime", "timestamp", "time", "date", "createtime":
			if stamp := parseTimestamp(value); stamp != nil {
				priority := depth * 2
				if normalized == "receivedat" || normalized == "receivedtime" || normalized == "createtime" {
					priority++
				}
				// A top-level server timestamp is not evidence of when the SMS arrived.
				if priority == 0 {
					continue
				}
				if c.stamp == nil || priority > c.stampPriority {
					c.stamp, c.stampPriority = stamp, priority
				} else if priority == c.stampPriority && !c.stamp.Equal(*stamp) {
					c.invalid = true
				}
			}
		}
	}
}

func failedResponse(obj map[string]any) bool {
	for key, value := range obj {
		switch strings.ToLower(key) {
		case "success", "status", "ok":
			if boolean, ok := value.(bool); ok && !boolean {
				return true
			}
			if text, ok := value.(string); ok {
				switch strings.ToLower(strings.TrimSpace(text)) {
				case "false", "error", "failed", "failure":
					return true
				}
			}
		case "error", "errors":
			if value != nil && value != false && value != "" {
				return true
			}
		}
	}
	return false
}

func textCode(text string) (string, bool) {
	if strings.ContainsAny(text, "<>") {
		return "", false
	}
	lower := strings.ToLower(text)
	for _, marker := range []string{"失败", "错误", "已失效", "已过期", "failed", "invalid", "expired", "error"} {
		if strings.Contains(lower, marker) {
			return "", false
		}
	}
	context := strings.Contains(text, "验证码") || strings.Contains(text, "驗證碼") || strings.Contains(lower, "verification code") || strings.Contains(lower, "security code") || strings.Contains(lower, "apple id code") || strings.Contains(lower, "apple account code")
	if !context {
		return "", false
	}
	codes := make(map[string]struct{})
	for _, digits := range digitRuns.FindAllString(text, -1) {
		if sixDigits.MatchString(digits) {
			codes[digits] = struct{}{}
		}
	}
	if len(codes) > 1 {
		return "", true
	}
	for code := range codes {
		return code, false
	}
	return "", false
}

func parseTimestamp(value any) *time.Time {
	var raw string
	switch v := value.(type) {
	case string:
		raw = strings.TrimSpace(v)
	case json.Number:
		raw = v.String()
	default:
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05Z07:00"} {
		if stamp, err := time.Parse(layout, raw); err == nil {
			return &stamp
		}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var stamp time.Time
		if n >= 1e12 && n < 1e14 {
			stamp = time.UnixMilli(n)
		} else if n >= 1e9 && n < 1e11 {
			stamp = time.Unix(n, 0)
		} else {
			return nil
		}
		return &stamp
	}
	return nil
}
