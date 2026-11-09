package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

const maxImageURLLength = 20971520

type imageSource struct{ kind, value string }
type imageReference struct {
	FileID   string `json:"file_id,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}
type sourceValue struct {
	o    *options
	kind string
}

func (v *sourceValue) String() string { return "" }
func (v *sourceValue) Type() string   { return "图片来源" }
func (v *sourceValue) Set(value string) error {
	v.o.sources = append(v.o.sources, imageSource{v.kind, value})
	if v.kind == "local" {
		v.o.imagePaths = append(v.o.imagePaths, value)
	}
	return nil
}
func addSourceFlags(f *pflag.FlagSet, o *options) {
	f.StringVar(&o.markupPath, "markup", "", "本地透明 PNG 标注层，与方向处理后的第一张图等尺寸")
	f.StringVar(&o.markupURL, "markup-url", "", "标注层 HTTPS URL 或 data URL")
	f.StringVar(&o.markupFileID, "markup-file-id", "", "已上传标注层 File ID")
	f.Var(&sourceValue{o, "local"}, "image", "本地 PNG/JPEG/WebP，可重复，共 1–16 张，每张严格小于 50,000,000 字节")
	f.Var(&sourceValue{o, "url"}, "image-url", "图片 HTTP(S) URL 或 data URL，按选项顺序添加")
	f.Var(&sourceValue{o, "file_id"}, "image-file-id", "已上传图片 File ID，按选项顺序添加")
	f.StringVar(&o.maskURL, "mask-url", "", "mask HTTP(S) URL 或 data URL")
	f.StringVar(&o.maskFileID, "mask-file-id", "", "已上传 mask 的 File ID")
}
func sourceReference(source imageSource) (imageReference, error) {
	switch source.kind {
	case "file_id":
		if strings.TrimSpace(source.value) == "" {
			return imageReference{}, errors.New("图片 File ID 不能为空")
		}
		return imageReference{FileID: source.value}, nil
	case "url":
		if len(source.value) > maxImageURLLength {
			return imageReference{}, errors.New("图片 image_url 超过 20,971,520 字符；请使用本地 multipart 或已有 File ID")
		}
		if strings.HasPrefix(source.value, "data:") {
			return imageReference{ImageURL: source.value}, nil
		}
		parsed, err := url.Parse(source.value)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return imageReference{}, errors.New("图片地址必须是 HTTP(S) URL 或 data URL")
		}
		return imageReference{ImageURL: source.value}, nil
	case "local":
		info, err := os.Stat(source.value)
		if err != nil {
			return imageReference{}, fmt.Errorf("读取图片失败：%w", err)
		}
		if !info.Mode().IsRegular() {
			return imageReference{}, errors.New("图片必须是普通文件")
		}
		if info.Size() <= 0 || info.Size() >= maxInputImageBytes {
			return imageReference{}, errors.New("图片不能为空，且必须小于 50,000,000 字节")
		}
		if base64.StdEncoding.EncodedLen(int(info.Size()))+22 > maxImageURLLength {
			return imageReference{}, errors.New("图片编码后的 image_url 过长；请使用本地 multipart 或已有 File ID")
		}
		f, err := os.Open(source.value)
		if err != nil {
			return imageReference{}, fmt.Errorf("读取图片失败：%w", err)
		}
		defer func() { _ = f.Close() }()
		raw, err := io.ReadAll(io.LimitReader(f, maxInputImageBytes))
		if err != nil {
			return imageReference{}, err
		}
		if len(raw) != int(info.Size()) {
			return imageReference{}, errors.New("图片在读取时发生变化")
		}
		_, format, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return imageReference{}, fmt.Errorf("图片必须是 PNG/JPEG/WebP：%w", err)
		}
		if format != "png" && format != "jpeg" && format != "webp" {
			return imageReference{}, errors.New("图片必须是 PNG/JPEG/WebP")
		}
		uri := "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(raw)
		if len(uri) > maxImageURLLength {
			return imageReference{}, errors.New("图片编码后的 image_url 过长；请使用本地 multipart 或已有 File ID")
		}
		return imageReference{ImageURL: uri}, nil
	}
	return imageReference{}, errors.New("未知图片来源")
}
func editPayload(o *options, fields map[string]any) (*httpPayload, func(), error) {
	if err := validateEditInputs(o); err != nil {
		return nil, nil, imageError(exitUsage, "invalid_input", "validate", err)
	}
	useJSON := o.supplied["svg-model"] || o.supplied["svg-reasoning-effort"] || o.supplied["mask-url"] || o.supplied["mask-file-id"] || o.supplied["markup-url"] || o.supplied["markup-file-id"]
	for _, source := range o.sources {
		useJSON = useJSON || source.kind != "local"
	}
	if !useJSON {
		file, contentType, err := buildEditBody(o, fields)
		if err != nil {
			return nil, nil, err
		}
		info, err := file.Stat()
		if err != nil {
			removeUpload(file)
			return nil, nil, err
		}
		return &httpPayload{file, info.Size(), contentType}, func() { removeUpload(file) }, nil
	}
	refs := make([]imageReference, 0, len(o.sources))
	for _, source := range o.sources {
		ref, err := sourceReference(source)
		if err != nil {
			return nil, nil, imageError(exitUsage, "invalid_input", "validate", err)
		}
		refs = append(refs, ref)
	}
	fields["images"] = refs
	for _, role := range []string{"mask", "markup"} {
		sources := o.overlaySources(role)
		if len(sources) > 0 {
			ref, err := sourceReference(sources[0])
			if err != nil {
				return nil, nil, imageError(exitUsage, "invalid_input", "validate", err)
			}
			fields[role] = ref
		}
	}

	file, err := os.CreateTemp("", "image-gen-upload-*")
	if err != nil {
		return nil, nil, fmt.Errorf("创建上传临时文件失败：%w", err)
	}
	if err := json.NewEncoder(file).Encode(fields); err != nil {
		removeUpload(file)
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		removeUpload(file)
		return nil, nil, err
	}
	return &httpPayload{file, info.Size(), "application/json"}, func() { removeUpload(file) }, nil
}
