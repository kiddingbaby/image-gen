package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"time"
)

type resultWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type retryInfo struct {
	Scope        string   `json:"scope"`
	Automatic    bool     `json:"automatic"`
	AfterSeconds *float64 `json:"after_seconds"`
}
type imageFailure struct {
	Code             string          `json:"code"`
	Message          string          `json:"message"`
	Stage            string          `json:"stage"`
	HTTPStatus       *int            `json:"http_status"`
	RequestID        *string         `json:"request_id"`
	Upstream         json.RawMessage `json:"upstream"`
	GenerationStatus string          `json:"generation_status"`
	Retry            retryInfo       `json:"retry"`
	exit             int
	cause            error
}

func (e *imageFailure) Error() string { return e.Message }
func (e *imageFailure) Unwrap() error { return e.cause }
func imageError(exit int, code, stage string, err error) error {
	return withExit(exit, &imageFailure{Code: code, Message: err.Error(), Stage: stage, Retry: retryInfo{Scope: "none"}, exit: exit, cause: err})
}

type savedPreview struct {
	Sequence     int `json:"sequence"`
	PartialIndex int `json:"partial_image_index"`
	savedImage
}
type imageResult struct {
	sequence         int
	jsonl            bool
	outputErr        error
	previewDir       string
	SchemaVersion    int                          `json:"schema_version"`
	Operation        string                       `json:"operation"`
	Status           string                       `json:"status"`
	Model            *string                      `json:"model"`
	RequestID        *string                      `json:"request_id"`
	Images           []savedImage                 `json:"images"`
	Previews         []savedPreview               `json:"previews"`
	Response         map[string]json.RawMessage   `json:"response"`
	Events           []map[string]json.RawMessage `json:"events"`
	Warnings         []resultWarning              `json:"warnings"`
	Error            *imageFailure                `json:"error"`
	generationStatus string
	httpStatus       *int
	stem             string
}

func newImageResult(operation string) *imageResult {
	return &imageResult{SchemaVersion: 1, Operation: operation, Status: "succeeded", Images: []savedImage{}, Previews: []savedPreview{}, Events: []map[string]json.RawMessage{}, Warnings: []resultWarning{}, generationStatus: "not_started"}
}
func finishImage(o *options, operation string, err error) error {
	if o.result == nil {
		o.result = newImageResult(operation)
	}
	r := o.result
	if r.outputErr != nil {
		err = r.outputErr
	}
	if err != nil {
		var failure *imageFailure
		if !errors.As(err, &failure) {
			code, stage := "save_failed", "save"
			switch exitCodeOf(err) {
			case exitUsage:
				code, stage = "invalid_argument", "validate"
			case exitConfig:
				code, stage = "invalid_config", "config"
			case exitNetwork:
				code, stage = "request_failed", "request"
			case exitUpstream:
				code, stage = "invalid_response", "decode"
			}
			failure = &imageFailure{Code: code, Message: err.Error(), Stage: stage, Retry: retryInfo{Scope: "none"}}
		}
		failure.GenerationStatus = r.generationStatus
		failure.RequestID = r.RequestID
		if failure.HTTPStatus == nil {
			failure.HTTPStatus = r.httpStatus
		}
		r.Error = failure
		r.Status = "failed"
	}
	if o.jsonlOutput {
		if outputErr := r.emit(streamEvent{Type: "result", Result: r}); outputErr != nil {
			return outputErr
		}
	} else if o.jsonOutput {
		if outputErr := json.NewEncoder(os.Stdout).Encode(r); outputErr != nil {
			return imageError(exitGeneral, "output_failed", "output", fmt.Errorf("写入 stdout 失败：%w", outputErr))
		}
	}
	return err
}
func (r *imageResult) warning(code, message string) {
	r.Warnings = append(r.Warnings, resultWarning{Code: code, Message: message})
	warnf("%s", message)
	if r.jsonl && r.outputErr == nil {
		warning := r.Warnings[len(r.Warnings)-1]
		r.outputErr = r.emit(streamEvent{Type: "warning", Warning: &warning})
	}
}
func (r *imageResult) upstreamError(status int, body []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err == nil && obj != nil {
		r.Response = obj
		stripResponseImages(r.Response)
	}
	raw := obj["error"]
	if len(raw) == 0 || string(raw) == "null" {
		raw = nil
	}
	if status >= 400 && status < 500 {
		r.generationStatus = "not_started"
	}
	summary := body
	if r.Response != nil {
		summary, _ = json.Marshal(r.Response)
	}
	failure := &imageFailure{Code: "upstream_error", Message: fmt.Sprintf("上游返回错误（HTTP %d）：%s", status, truncate(string(summary), 1500)), Stage: "request", HTTPStatus: &status, Upstream: raw, Retry: retryInfo{Scope: "none"}}
	return withExit(exitUpstream, failure)
}
func prepareOutputDir(path string) (string, error) {
	abs, err := filepath.Abs(first(path, "."))
	if err != nil {
		return "", fmt.Errorf("无法解析输出目录：%w", err)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return "", fmt.Errorf("无法创建输出目录 %s：%w", abs, err)
	}
	f, err := os.CreateTemp(abs, ".image-gen-check-*")
	if err != nil {
		return "", fmt.Errorf("输出目录不可写：%w", err)
	}
	removeUpload(f)
	return abs, nil
}
func (r *imageResult) save(raw []byte, o *options, dir, declared string, index int) (savedImage, error) {
	actual := sniffImageExt(raw)
	if actual == "" {
		return savedImage{}, imageError(exitUpstream, "invalid_image", "decode", errors.New("无法识别上游图片格式"))
	}
	requested := first(o.outputFormat, "png")
	if actual != requested || (declared != "" && actual != declared) {
		r.warning("output_format_mismatch", fmt.Sprintf("实际格式 %s 与请求 %s 或上游声明 %s 不符；已按实际格式命名", actual, requested, declared))
	}
	if r.stem == "" {
		model := "image"
		if r.Model != nil {
			model = *r.Model
		}
		r.stem = fmt.Sprintf("%s-%s-%s", time.Now().Format("20060102-150405"), safeFileStem(model), randomHex(6))
	}
	suffix := r.stem
	if o.n > 1 || index > 0 {
		suffix += fmt.Sprintf("-%d", index)
	}
	path := filepath.Join(dir, suffix+"."+actual)
	if err := atomicWrite(path, raw, fileModeImage); err != nil {
		return savedImage{}, imageError(exitGeneral, "save_failed", "save", fmt.Errorf("图片落盘失败：%w", err))
	}
	img := savedImage{Index: index, Path: path, Format: actual, Bytes: len(raw)}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		img.Width = cfg.Width
		img.Height = cfg.Height
	}
	return img, nil
}
func (r *imageResult) consumeJSON(body []byte, o *options, dir string) error {
	if err := json.Unmarshal(body, &r.Response); err != nil || r.Response == nil {
		return imageError(exitUpstream, "invalid_response", "decode", errors.New("上游响应不是合法 JSON 对象"))
	}
	encoded := stripResponseImages(r.Response)
	if raw := r.Response["error"]; len(raw) > 0 && string(raw) != "null" {
		return r.upstreamError(200, body)
	}
	var data []map[string]json.RawMessage
	if err := json.Unmarshal(r.Response["data"], &data); err != nil {
		return imageError(exitUpstream, "invalid_response", "decode", errors.New("上游响应里没有有效图像 data"))
	}

	if o.stream {
		r.warning("stream_ignored", "请求流式响应，但网关返回完整 JSON；已接收结果")
		if r.outputErr != nil {
			return r.outputErr
		}
	}

	if len(data) == o.n {
		r.generationStatus = "completed"
	}
	var declared string
	_ = json.Unmarshal(r.Response["output_format"], &declared)
	for i, item := range data {
		var b64, uri string
		_ = json.Unmarshal(encoded[i], &b64)
		_ = json.Unmarshal(item["url"], &uri)
		var raw []byte
		var err error
		switch {
		case b64 != "":
			raw, err = base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return imageError(exitUpstream, "invalid_image", "decode", fmt.Errorf("上游返回了无效的 Base64 图像数据：%w", err))
			}
		case uri != "":
			raw, err = downloadImage(o, uri)
			if err != nil {
				return err
			}
		default:
			return imageError(exitUpstream, "invalid_response", "decode", errors.New("上游返回项里既没有 b64_json 也没有 url"))
		}
		img, err := r.save(raw, o, dir, declared, i)
		if err != nil {
			return err
		}
		r.Images = append(r.Images, img)
		if err := r.publishImage(o, img, ""); err != nil {
			return err
		}
		if r.outputErr != nil {
			return r.outputErr
		}
	}
	if len(data) != o.n {
		return imageError(exitUpstream, "incomplete_result", "decode", fmt.Errorf("上游返回 %d 张结果，期望 %d 张", len(data), o.n))
	}
	return nil
}
func readImageResponse(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil {
		return nil, imageNetworkError(err)
	}
	if len(data) > maxResponseBytes {
		return nil, imageError(exitGeneral, "response_limit_exceeded", "decode", fmt.Errorf("响应体超过 %d 字节", maxResponseBytes))
	}
	return data, nil
}

// 逐项去除图像数据；一个错误项不能让其它项的 base64 留在结果中。
func stripResponseImages(response map[string]json.RawMessage) []json.RawMessage {
	raw, ok := response["data"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	encoded := make([]json.RawMessage, len(items))
	for i, item := range items {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(item, &object); err != nil || object == nil {
			continue
		}
		encoded[i] = object["b64_json"]
		delete(object, "b64_json")
		items[i], _ = json.Marshal(object)
	}
	response["data"], _ = json.Marshal(items)
	return encoded
}
