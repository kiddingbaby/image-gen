package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/spf13/pflag"
)

type imageParameters struct {
	complexity, svgModel, svgEffort, safetyIdentifier, user, responseFormat, style, inputFidelity string
	frameCount, frameDuration, partialImages                                                      int
	stream                                                                                        bool
}
type svgRequest struct {
	Model     *string       `json:"model,omitempty"`
	Reasoning *svgReasoning `json:"reasoning,omitempty"`
}
type svgReasoning struct {
	Effort string `json:"effort"`
}
type imageRequest struct {
	Model             *string     `json:"model,omitempty"`
	Prompt            string      `json:"prompt"`
	N                 *int        `json:"n,omitempty"`
	Size              *string     `json:"size,omitempty"`
	Quality           *string     `json:"quality,omitempty"`
	OutputFormat      *string     `json:"output_format,omitempty"`
	Background        *string     `json:"background,omitempty"`
	Moderation        *string     `json:"moderation,omitempty"`
	OutputCompression *int        `json:"output_compression,omitempty"`
	Complexity        *string     `json:"complexity,omitempty"`
	FrameCount        *int        `json:"frame_count,omitempty"`
	FrameDuration     *int        `json:"frame_duration_ms,omitempty"`
	Stream            *bool       `json:"stream,omitempty"`
	PartialImages     *int        `json:"partial_images,omitempty"`
	SafetyIdentifier  *string     `json:"safety_identifier,omitempty"`
	User              *string     `json:"user,omitempty"`
	ResponseFormat    *string     `json:"response_format,omitempty"`
	Style             *string     `json:"style,omitempty"`
	InputFidelity     *string     `json:"input_fidelity,omitempty"`
	SVG               *svgRequest `json:"svg,omitempty"`
}

func optional[T any](set bool, value T) *T {
	if set {
		return &value
	}
	return nil
}
func addImageParameterFlags(f *pflag.FlagSet, o *options) {
	f.StringVar(&o.complexity, "complexity", "", "复杂度：low/medium/high")
	f.IntVar(&o.frameCount, "frame-count", 0, "GIF 帧数：2–16")
	f.IntVar(&o.frameDuration, "frame-duration-ms", 0, "GIF 帧间隔毫秒，至少 20（示例 20–2000）")
	f.BoolVar(&o.stream, "stream", false, "接收流式预览和结果")
	f.IntVar(&o.partialImages, "partial-images", 0, "预览数量：0–3，需要 --stream")
	f.StringVar(&o.safetyIdentifier, "safety-identifier", "", "安全标识，最多 128 个字符")
	f.StringVar(&o.user, "user", "", "已弃用的用户标识，优先使用 --safety-identifier")
	f.StringVar(&o.svgModel, "svg-model", "", "SVG 编写模型（独立于 --model）")
	f.StringVar(&o.svgEffort, "svg-reasoning-effort", "", "SVG 推理档位：none/minimal/low/medium/high/xhigh/max")
}
func requestFields(o *options, model, prompt, size, quality, format, background, moderation string) map[string]any {
	request := imageRequest{
		Model: optional(model != "", model), Prompt: prompt, N: optional(o.supplied["count"], o.n),
		Size: optional(size != "", size), Quality: optional(quality != "", quality), OutputFormat: optional(o.supplied["format"], format),
		Background: optional(background != "", background), Moderation: optional(moderation != "", moderation), OutputCompression: optional(o.outputCompressionSet, o.outputCompression),
		Complexity: optional(o.supplied["complexity"], o.complexity), FrameCount: optional(o.supplied["frame-count"], o.frameCount), FrameDuration: optional(o.supplied["frame-duration-ms"], o.frameDuration),
		Stream: optional(o.supplied["stream"], o.stream), PartialImages: optional(o.supplied["partial-images"], o.partialImages),
		SafetyIdentifier: optional(o.supplied["safety-identifier"], o.safetyIdentifier), User: optional(o.supplied["user"], o.user),
		ResponseFormat: optional(o.supplied["response-format"], o.responseFormat), Style: optional(o.supplied["style"], o.style), InputFidelity: optional(o.supplied["input-fidelity"], o.inputFidelity),
	}
	if o.supplied["svg-model"] || o.supplied["svg-reasoning-effort"] {
		request.SVG = &svgRequest{Model: optional(o.supplied["svg-model"], o.svgModel)}
		if o.supplied["svg-reasoning-effort"] {
			request.SVG.Reasoning = &svgReasoning{Effort: o.svgEffort}
		}
	}
	// 此结构只含可序列化标量；转换用于 multipart 和 JSON 共用同一字段映射。
	raw, _ := json.Marshal(request)
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&fields)
	// 保持 multipart 标量和原有内部调用的整数类型。
	for key, value := range fields {
		if n, ok := value.(json.Number); ok {
			if integer, err := n.Int64(); err == nil {
				fields[key] = int(integer)
			}
		}
	}
	return fields
}
func isSVG(raw []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})))
	depth, roots := 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return roots == 1 && depth == 0
		}
		if err != nil {
			return false
		}
		switch t := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || t.Name.Local != "svg" || (t.Name.Space != "" && t.Name.Space != "http://www.w3.org/2000/svg") {
					return false
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return false
			}
		}
	}
}

func validateImageParameters(o *options, editing bool) error {
	if o.jsonlOutput && (o.jsonOutput || !o.supplied["stream"] || !o.stream) {
		return errors.New("--jsonl 要求显式 --stream=true，且与 --json 互斥")
	}
	for flag, value := range map[string]string{"model": o.model, "size": o.size, "quality": o.quality, "format": o.outputFormat, "background": o.background, "moderation": o.moderation, "svg-model": o.svgModel} {
		if o.supplied[flag] && strings.TrimSpace(value) == "" {
			return fmt.Errorf("--%s 不能为空", flag)
		}
	}
	for _, field := range []struct {
		name, value string
		choices     []string
	}{
		{"complexity", o.complexity, []string{"low", "medium", "high"}},
		{"svg-reasoning-effort", o.svgEffort, []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}},
		{"input-fidelity", o.inputFidelity, []string{"high", "low"}},
		{"response-format", o.responseFormat, []string{"url", "b64_json"}},
		{"style", o.style, []string{"vivid", "natural"}},
	} {
		if o.supplied[field.name] && !slices.Contains(field.choices, field.value) {
			return fmt.Errorf("--%s 必须是 %s", field.name, strings.Join(field.choices, "/"))
		}
	}
	if editing && (o.quality == "standard" || o.quality == "hd") {
		return errors.New("edit 的 --quality 不支持 standard/hd")
	}
	if !utf8.ValidString(o.safetyIdentifier) || utf8.RuneCountInString(o.safetyIdentifier) > 128 {
		return errors.New("--safety-identifier 必须是合法 UTF-8 且最多 128 个字符")
	}
	format := first(o.outputFormat, "png")
	if o.supplied["frame-count"] {
		if format != "gif" || o.frameCount < 2 || o.frameCount > 16 {
			return errors.New("--frame-count 仅用于 gif，必须在 2–16 之间")
		}
	}
	if o.supplied["frame-duration-ms"] {
		if format != "gif" || o.frameDuration < 20 {
			return errors.New("--frame-duration-ms 仅用于 gif，至少为 20")
		}
	}
	if o.supplied["partial-images"] && (!o.stream || o.partialImages < 0 || o.partialImages > 3) {
		return errors.New("--partial-images 必须在 0–3 之间，且要求 --stream=true")
	}
	if format == "gif" || format == "svg" {
		if o.stream || o.supplied["partial-images"] || o.outputCompressionSet {
			return fmt.Errorf("%s 不支持 stream、partial_images 或 output_compression", format)
		}
	}
	if format == "svg" && o.n != 1 {
		return errors.New("SVG 输出要求 --count=1")
	}
	if format == "gif" && o.maskPath != "" {
		return errors.New("GIF 不支持 mask")
	}
	if o.supplied["svg-model"] || o.supplied["svg-reasoning-effort"] {
		if format != "svg" {
			return errors.New("SVG 子参数要求 --format=svg")
		}
		if !editing && !o.supplied["svg-model"] {
			return errors.New("生成时 SVG 子参数要求 --svg-model")
		}
	}
	return nil
}
