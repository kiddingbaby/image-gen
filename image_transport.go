package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func quotaError(body []byte) bool {
	var obj struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &obj)
	for _, value := range []string{obj.Error.Code, obj.Error.Type} {
		switch value {
		case "insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "organization_usage_limit_exceeded":
			return true
		}
	}
	return false
}
func retryAfter(value string) *float64 {
	if value == "" {
		return nil
	}
	if seconds, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32); err == nil {
		f := float64(seconds)
		return &f
	}
	if date, err := http.ParseTime(value); err == nil {
		f := time.Until(date).Seconds()
		if f < 0 {
			f = 0
		}
		return &f
	}
	return nil
}
func imageNetworkError(err error) error {
	if errors.Is(err, context.Canceled) {
		return imageError(130, "canceled", "request", errors.New("请求已取消"))
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return imageError(exitNetwork, "request_timeout", "request", fmt.Errorf("请求超时，上游是否已完成生成尚不确定：%w", err))
	}
	return imageError(exitNetwork, "request_failed", "request", fmt.Errorf("网络错误：%w", err))
}
func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return imageNetworkError(ctx.Err())
	}
}
func (t target) imageResponse(path string, payload *httpPayload, o *options) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(o.context(), http.MethodPost, endpoint(t.baseURL, path), io.NewSectionReader(payload.reader, 0, payload.size))
		if err != nil {
			return nil, imageNetworkError(err)
		}
		req.Header.Set("Authorization", "Bearer "+t.key)
		req.Header.Set("Content-Type", payload.contentType)
		if o.stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		req.ContentLength = payload.size
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(payload.reader, 0, payload.size)), nil
		}
		o.result.RequestID = nil
		o.result.httpStatus = nil
		o.result.generationStatus = "unknown"
		resp, err := (&http.Client{Timeout: o.timeout}).Do(req)
		if err != nil {
			return nil, imageNetworkError(err)
		}
		o.result.httpStatus = &resp.StatusCode
		if id := resp.Header.Get("X-Request-ID"); id != "" {
			o.result.RequestID = &id
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			o.result.generationStatus = "not_started"
		}
		if resp.StatusCode != 429 || attempt == maxRateLimitRetries {
			return resp, nil
		}
		data, readErr := readImageResponse(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
		delay := retryDelay(attempt)
		if seconds := retryAfter(resp.Header.Get("Retry-After")); seconds != nil {
			if *seconds > o.timeout.Seconds() {
				return resp, nil
			}
			delay = time.Duration(*seconds * float64(time.Second))
		}
		if quotaError(data) || delay > o.timeout {
			return resp, nil
		}
		if err := waitRetry(o.context(), delay); err != nil {
			return nil, err
		}
	}
}

func downloadImage(o *options, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, imageError(exitNetwork, "download_failed", "download", errors.New("上游返回了非法的图片 URL"))
	}
	for attempt := 0; attempt <= maxRateLimitRetries; attempt++ {
		req, err := http.NewRequestWithContext(o.context(), http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, imageError(exitNetwork, "download_failed", "download", err)
		}
		resp, err := (&http.Client{Timeout: o.timeout}).Do(req)
		retry := err != nil
		var data []byte
		var after *float64
		if err == nil {
			if resp.StatusCode >= 400 {
				err = fmt.Errorf("下载图片 URL 失败（HTTP %d）", resp.StatusCode)
				retry = resp.StatusCode == 429 || resp.StatusCode == 408 || resp.StatusCode >= 500
				after = retryAfter(resp.Header.Get("Retry-After"))
			} else {
				data, err = readImageResponse(resp.Body)
				retry = err != nil && exitCodeOf(err) == exitNetwork
			}
			_ = resp.Body.Close()
		}
		if o.context().Err() != nil {
			return nil, imageError(130, "canceled", "download", errors.New("下载已取消"))
		}
		if err == nil {
			if len(data) == 0 {
				return nil, imageError(exitUpstream, "invalid_image", "decode", errors.New("上游图片 URL 返回空内容"))
			}
			return data, nil
		}
		var failure *imageFailure
		if errors.As(err, &failure) && failure.Code == "response_limit_exceeded" {
			failure.Stage = "download"
			return nil, err
		}
		delay := retryDelay(attempt)
		if after != nil {
			if *after > o.timeout.Seconds() {
				retry = false
			} else {
				delay = time.Duration(*after * float64(time.Second))
			}
		}
		if !retry || attempt == maxRateLimitRetries || delay > o.timeout {
			return nil, withExit(exitNetwork, &imageFailure{Code: "download_failed", Message: err.Error(), Stage: "download", Retry: retryInfo{Scope: "asset", AfterSeconds: after}, cause: err})
		}
		if err := waitRetry(o.context(), delay); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("图片下载未完成")
}
