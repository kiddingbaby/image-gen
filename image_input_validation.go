package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"strings"
)

const maxMarkupPixels = 16777216

type inputInfo struct {
	config        image.Config
	width, height int
	format        string
	raw           []byte
}

func inspectSource(source imageSource) (*inputInfo, error) {
	var raw []byte
	switch source.kind {
	case "file_id":
		_, err := sourceReference(source)
		return nil, err
	case "url":
		if _, err := sourceReference(source); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(source.value, "data:") {
			return nil, nil
		}
		header, encoded, ok := strings.Cut(source.value, ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil, errors.New("图片 data URL 必须使用 base64")
		}
		mime := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
			return nil, errors.New("图片 data URL 的 MIME 必须是 image/png、image/jpeg 或 image/webp")
		}
		var err error
		raw, err = base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("图片 data URL 的 base64 无效：%w", err)
		}
		info, err := inspectBytes(raw)
		if err != nil {
			return nil, err
		}
		if "image/"+info.format != mime {
			return nil, errors.New("图片 data URL 的 MIME 与内容不一致")
		}
		return info, nil
	case "local":
		stat, err := os.Stat(source.value)
		if err != nil {
			return nil, fmt.Errorf("读取图片 %s 失败：%w", source.value, err)
		}
		if !stat.Mode().IsRegular() {
			return nil, errors.New("图片必须是普通文件")
		}
		if stat.Size() <= 0 || stat.Size() >= maxInputImageBytes {
			return nil, errors.New("图片不能为空，且必须小于 50,000,000 字节")
		}
		f, err := os.Open(source.value)
		if err != nil {
			return nil, fmt.Errorf("读取图片失败：%w", err)
		}
		defer func() { _ = f.Close() }()
		raw, err = io.ReadAll(io.LimitReader(f, maxInputImageBytes))
		if err != nil {
			return nil, fmt.Errorf("读取图片失败：%w", err)
		}
		if int64(len(raw)) != stat.Size() {
			return nil, errors.New("图片在读取时发生变化")
		}
	}
	return inspectBytes(raw)
}
func inspectBytes(raw []byte) (*inputInfo, error) {
	if len(raw) == 0 || len(raw) >= maxInputImageBytes {
		return nil, errors.New("图片不能为空，且必须小于 50,000,000 字节")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("无法读取图片尺寸或格式，请提供 PNG/JPEG/WebP：%w", err)
	}
	if format != "png" && format != "jpeg" && format != "webp" {
		return nil, errors.New("图片必须是 PNG/JPEG/WebP")
	}
	info := &inputInfo{config: cfg, width: cfg.Width, height: cfg.Height, format: format, raw: raw}
	orientation := exifOrientation(raw, format)
	if orientation >= 5 && orientation <= 8 {
		info.width, info.height = info.height, info.width
	}
	return info, nil
}
func (o *options) overlaySources(role string) []imageSource {
	values := []imageSource{{"local", o.maskPath}, {"url", o.maskURL}, {"file_id", o.maskFileID}}
	if role == "markup" {
		values = []imageSource{{"local", o.markupPath}, {"url", o.markupURL}, {"file_id", o.markupFileID}}
	}
	var out []imageSource
	for i, suffix := range []string{"", "-url", "-file-id"} {
		if o.supplied[role+suffix] {
			out = append(out, values[i])
		}
	}
	return out
}
func validateEditInputs(o *options) error {
	masks, markups := o.overlaySources("mask"), o.overlaySources("markup")
	if len(masks) > 1 || len(markups) > 1 {
		return errors.New("mask 或 markup 的文件、URL、File ID 来源互斥")
	}
	if len(masks) > 0 && len(markups) > 0 {
		return errors.New("mask 与 markup 不能同时使用")
	}
	if len(masks) > 0 && o.outputFormat == "gif" {
		return errors.New("GIF 不支持 mask")
	}
	var first *inputInfo
	for i, source := range o.sources {
		info, err := inspectSource(source)
		if err != nil {
			return err
		}
		if i == 0 {
			first = info
		}
	}
	for _, overlay := range []struct {
		role    string
		sources []imageSource
	}{{"mask", masks}, {"markup", markups}} {
		if len(overlay.sources) == 0 {
			continue
		}
		source := overlay.sources[0]
		if overlay.role == "markup" && source.kind == "url" && !strings.HasPrefix(source.value, "https://") && !strings.HasPrefix(source.value, "data:") {
			return errors.New("markup 远程地址只接受 HTTPS 或 data URL")
		}
		info, err := inspectSource(source)
		if err != nil {
			return err
		}
		if overlay.role == "markup" && first != nil && int64(first.width)*int64(first.height) > maxMarkupPixels {
			return errors.New("markup 的第一张原图不能超过 16,777,216 像素")
		}
		if info == nil {
			continue
		}
		if info.format != "png" {
			return fmt.Errorf("--%s 必须是 PNG", overlay.role)
		}
		if !hasAlphaChannel(info.config.ColorModel) {
			return fmt.Errorf("--%s 必须带透明通道", overlay.role)
		}
		if overlay.role == "mask" {
			if first != nil && (info.config.Width != first.config.Width || info.config.Height != first.config.Height) {
				return errors.New("--mask 尺寸必须与第一张原图一致")
			}
		} else {
			if int64(info.width)*int64(info.height) > maxMarkupPixels {
				return errors.New("markup 不能超过 16,777,216 像素")
			}
			if first != nil && (info.width != first.width || info.height != first.height) {
				return errors.New("--markup 尺寸必须与方向处理后的第一张原图一致")
			}
			decoded, _, err := image.Decode(bytes.NewReader(info.raw))
			if err != nil {
				return fmt.Errorf("无法解码 markup PNG：%w", err)
			}
			transparent := false
			if opaque, ok := decoded.(interface{ Opaque() bool }); ok {
				transparent = !opaque.Opaque()
			} else {
				bounds := decoded.Bounds()
				for y := bounds.Min.Y; y < bounds.Max.Y && !transparent; y++ {
					for x := bounds.Min.X; x < bounds.Max.X; x++ {
						if _, _, _, a := decoded.At(x, y).RGBA(); a < 65535 {
							transparent = true
							break
						}
					}
				}
			}
			if !transparent {
				return errors.New("markup 必须包含实际透明像素")
			}
		}
	}
	return nil
}

// 只读方向标签；不重绘图像，也不解析外部 EXIF 引用。
func exifOrientation(raw []byte, format string) uint16 {
	var exif []byte
	switch format {
	case "jpeg":
		for pos := 2; pos+4 <= len(raw); {
			if raw[pos] != 0xff {
				break
			}
			marker := raw[pos+1]
			if marker == 0xda || marker == 0xd9 {
				break
			}
			if marker == 0xff {
				pos++
				continue
			}
			if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
				pos += 2
				continue
			}
			length := int(binary.BigEndian.Uint16(raw[pos+2 : pos+4]))
			if length < 2 || pos+2+length > len(raw) {
				break
			}
			data := raw[pos+4 : pos+2+length]
			if marker == 0xe1 && bytes.HasPrefix(data, []byte("Exif\x00\x00")) {
				exif = data[6:]
				break
			}
			pos += 2 + length
		}
	case "png":
		for pos := 8; pos+12 <= len(raw); {
			length := uint64(binary.BigEndian.Uint32(raw[pos : pos+4]))
			if length > uint64(len(raw)-pos-12) {
				break
			}
			if string(raw[pos+4:pos+8]) == "eXIf" {
				exif = raw[pos+8 : pos+8+int(length)]
				break
			}
			pos += 12 + int(length)
		}
	case "webp":
		for pos := 12; pos+8 <= len(raw); {
			length := uint64(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
			if length > uint64(len(raw)-pos-8) {
				break
			}
			if string(raw[pos:pos+4]) == "EXIF" {
				exif = bytes.TrimPrefix(raw[pos+8:pos+8+int(length)], []byte("Exif\x00\x00"))
				break
			}
			pos += 8 + int(length) + int(length%2)
		}
	}
	if len(exif) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(exif[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(exif[2:4]) != 42 {
		return 1
	}
	offset := uint64(order.Uint32(exif[4:8]))
	if offset+2 > uint64(len(exif)) {
		return 1
	}
	count := int(order.Uint16(exif[offset : offset+2]))
	for i := 0; i < count; i++ {
		pos := offset + 2 + uint64(i)*12
		if pos+12 > uint64(len(exif)) {
			break
		}
		entry := exif[pos : pos+12]
		if order.Uint16(entry[:2]) == 0x112 && order.Uint16(entry[2:4]) == 3 && order.Uint32(entry[4:8]) == 1 {
			return order.Uint16(entry[8:10])
		}
	}
	return 1
}
