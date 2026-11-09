package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type streamEvent struct {
	Sequence     int            `json:"sequence"`
	Type         string         `json:"type"`
	UpstreamType string         `json:"upstream_type,omitempty"`
	Image        *savedImage    `json:"image,omitempty"`
	Preview      *savedPreview  `json:"preview,omitempty"`
	Warning      *resultWarning `json:"warning,omitempty"`
	Result       *imageResult   `json:"result,omitempty"`
}

func (r *imageResult) emit(event streamEvent) error {
	r.sequence++
	event.Sequence = r.sequence
	if err := json.NewEncoder(os.Stdout).Encode(event); err != nil {
		return imageError(exitGeneral, "output_failed", "output", fmt.Errorf("写入 stdout 失败：%w", err))
	}
	return nil
}
func (r *imageResult) publishImage(o *options, img savedImage, upstream string) error {
	if o.jsonlOutput {
		return r.emit(streamEvent{Type: "image.completed", UpstreamType: upstream, Image: &img})
	}
	if !o.jsonOutput {
		if _, err := fmt.Fprintln(os.Stdout, img.Path); err != nil {
			return imageError(exitGeneral, "output_failed", "output", fmt.Errorf("写入 stdout 失败：%w", err))
		}
	}
	return nil
}
func (r *imageResult) consumeStream(body io.Reader, o *options, dir string) error {
	reader := bufio.NewReader(io.LimitReader(body, maxResponseBytes+1))
	total := 0
	var name string
	var data bytes.Buffer
	unknown := map[string]bool{}
	for {
		line, err := reader.ReadString('\n')
		total += len(line)
		if total > maxResponseBytes {
			return imageError(exitGeneral, "response_limit_exceeded", "decode", fmt.Errorf("流式响应超过 %d 字节", maxResponseBytes))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return imageNetworkError(err)
			}
			return imageError(exitUpstream, "incomplete_stream", "decode", fmt.Errorf("流已结束，仅收到 %d/%d 张最终图片", len(r.Images), o.n))
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if data.Len() == 0 {
				name = ""
				continue
			}
			raw := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
			if bytes.Equal(raw, []byte("[DONE]")) {
				return imageError(exitUpstream, "incomplete_stream", "decode", fmt.Errorf("流已结束，仅收到 %d/%d 张最终图片", len(r.Images), o.n))
			}
			done, err := r.consumeEvent(name, raw, o, dir, unknown)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			name = ""
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
}
func (r *imageResult) consumeEvent(name string, raw []byte, o *options, dir string, unknown map[string]bool) (bool, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(raw, &event); err != nil || event == nil {
		return false, imageError(exitUpstream, "invalid_response", "decode", errors.New("SSE data 不是合法 JSON 对象"))
	}
	var typ string
	if field, ok := event["type"]; ok {
		if err := json.Unmarshal(field, &typ); err != nil {
			return false, imageError(exitUpstream, "invalid_response", "decode", errors.New("SSE type 必须是字符串"))
		}
	}
	if name != "" && typ != "" && name != typ {
		return false, imageError(exitUpstream, "invalid_response", "decode", errors.New("SSE event 与 data.type 不一致"))
	}
	typ = first(typ, name)
	if typ == "" {
		return false, imageError(exitUpstream, "invalid_response", "decode", errors.New("SSE 事件缺少类型"))
	}
	encoded := event["b64_json"]
	delete(event, "b64_json")
	if _, ok := event["type"]; !ok {
		event["type"], _ = json.Marshal(typ)
	}
	r.Events = append(r.Events, event)
	if typ == "error" || (len(event["error"]) > 0 && string(event["error"]) != "null") {
		upstream := event["error"]
		if len(upstream) == 0 {
			upstream, _ = json.Marshal(event)
		}
		return false, withExit(exitUpstream, &imageFailure{Code: "upstream_error", Message: "上游流式错误：" + truncate(string(upstream), 1500), Stage: "decode", Upstream: upstream, Retry: retryInfo{Scope: "none"}})
	}
	prefix := "image_generation."
	if r.Operation == "edit" {
		prefix = "image_edit."
	}
	if typ != prefix+"partial_image" && typ != prefix+"completed" {
		if !unknown[typ] {
			unknown[typ] = true
			r.warning("unknown_stream_event", "收到未知流事件："+typ)
		}
		return false, r.outputErr
	}
	var b64, declared string
	if err := json.Unmarshal(encoded, &b64); err != nil || b64 == "" {
		return false, imageError(exitUpstream, "invalid_image", "decode", errors.New("流事件缺少 base64 图像"))
	}
	rawImage, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false, imageError(exitUpstream, "invalid_image", "decode", fmt.Errorf("流事件 base64 无效：%w", err))
	}
	_ = json.Unmarshal(event["output_format"], &declared)
	if typ == prefix+"partial_image" {
		var partial int
		if err := json.Unmarshal(event["partial_image_index"], &partial); err != nil || partial < 0 {
			return false, imageError(exitUpstream, "invalid_response", "decode", errors.New("预览缺少有效 partial_image_index"))
		}
		if r.previewDir == "" {
			r.previewDir = filepath.Join(dir, "previews", randomHex(12))
			if err := os.MkdirAll(r.previewDir, 0755); err != nil {
				return false, imageError(exitGeneral, "save_failed", "save", err)
			}
		}
		finalStem := r.stem
		r.stem = fmt.Sprintf("preview-%06d", len(r.Events))
		img, err := r.save(rawImage, o, r.previewDir, declared, 0)
		r.stem = finalStem
		if err != nil {
			return false, err
		}
		preview := savedPreview{Sequence: len(r.Events), PartialIndex: partial, savedImage: img}
		r.Previews = append(r.Previews, preview)
		if o.jsonlOutput {
			return false, r.emit(streamEvent{Type: "image.partial", UpstreamType: typ, Preview: &preview})
		}
		statusf(o, "预览已保存：%s", img.Path)
		return false, r.outputErr
	}
	if len(r.Images)+1 == o.n {
		r.generationStatus = "completed"
	}
	img, err := r.save(rawImage, o, dir, declared, len(r.Images))
	if err != nil {
		return false, err
	}
	r.Images = append(r.Images, img)
	if err := r.publishImage(o, img, typ); err != nil {
		return false, err
	}
	return len(r.Images) == o.n, r.outputErr
}
