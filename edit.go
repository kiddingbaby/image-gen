package main

import (
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	_ "golang.org/x/image/webp"
)

const maxInputImageBytes = 50_000_000

func newEditCmd(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit [提示词]",
		Short: "编辑已有图片，或使用参考图生成图片",
		Long:  "提交本地图片、URL 或 File ID 和修改要求，编辑结果另存为新文件。提示词可来自参数或 stdin；`-` 显式表示 stdin。",
		Example: "  image-gen edit \"把背景换成雨夜街道\" --image original.png\n" +
			"  echo \"改成水彩风格\" | image-gen edit --image original.png --json",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && stdinTTY() {
				return cmd.Help()
			}
			if len(args) == 1 {
				o.prompt = args[0]
				o.promptArgument = true
			}
			cfg, err := loadConfig(o.configPath)
			if err != nil {
				return withExit(exitConfig, err)
			}
			captureImageFlags(cmd, o)
			return cmdImage(o, cfg, true)
		},
	}
	addGenerateFlags(cmd.Flags(), o)
	cmd.Flags().StringVar(&o.inputFidelity, "input-fidelity", "", "输入保真度：high/low（默认省略）")
	addSourceFlags(cmd.Flags(), o)
	cmd.Flags().StringVar(&o.maskPath, "mask", "", "局部编辑 mask：与第一张原图等尺寸、带透明通道的 PNG")
	_ = cmd.MarkFlagFilename("image", "png", "jpg", "jpeg", "webp")
	_ = cmd.MarkFlagFilename("mask", "png")
	_ = cmd.MarkFlagFilename("markup", "png")
	completeImageFlags(cmd)
	return cmd
}

// buildEditBody 使用临时文件保存 multipart，以便在 429 时完整重传。
// 调用方负责在请求完成后关闭并删除返回的文件。
func buildEditBody(o *options, fields map[string]any) (*os.File, string, error) {
	f, err := os.CreateTemp("", "image-gen-upload-*")
	if err != nil {
		return nil, "", fmt.Errorf("创建上传临时文件失败：%w", err)
	}
	ok := false
	defer func() {
		if !ok {
			removeUpload(f)
		}
	}()
	w := multipart.NewWriter(f)
	for key, value := range fields {
		if err := w.WriteField(key, fmt.Sprint(value)); err != nil {
			return nil, "", fmt.Errorf("写入编辑参数失败：%w", err)
		}
	}
	var firstImage image.Config
	for i, path := range o.imagePaths {
		cfg, err := writeImagePart(w, "image[]", path, nil)
		if err != nil {
			return nil, "", err
		}
		if i == 0 {
			firstImage = cfg
		}
	}
	if o.maskPath != "" {
		if _, err := writeImagePart(w, "mask", o.maskPath, &firstImage); err != nil {
			return nil, "", err
		}
	}
	if o.markupPath != "" {
		if _, err := writeImagePart(w, "markup", o.markupPath, nil); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("完成上传数据失败：%w", err)
	}
	ok = true
	return f, w.FormDataContentType(), nil
}

func writeImagePart(w *multipart.Writer, field, path string, maskFor *image.Config) (image.Config, error) {
	// 先 stat 拒绝目录、FIFO 等非普通文件，避免读取时阻塞。
	info, err := os.Stat(path)
	if err != nil {
		return image.Config{}, withExit(exitUsage, fmt.Errorf("读取图片 %s 失败：%w", path, err))
	}
	if !info.Mode().IsRegular() {
		return image.Config{}, withExit(exitUsage, fmt.Errorf("图片 %s 必须是普通文件", path))
	}
	if info.Size() <= 0 || info.Size() >= maxInputImageBytes {
		return image.Config{}, withExit(exitUsage, fmt.Errorf("图片 %s 不能为空，且必须小于 50,000,000 字节", path))
	}
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, withExit(exitUsage, fmt.Errorf("读取图片 %s 失败：%w", path, err))
	}
	defer func() { _ = f.Close() }()
	cfg, format, err := image.DecodeConfig(io.LimitReader(f, maxInputImageBytes))
	if err != nil {
		return image.Config{}, withExit(exitUsage, fmt.Errorf("无法读取图片 %s 的尺寸或格式；请提供有效的 PNG、JPEG 或 WebP：%w", path, err))
	}
	if format != "png" && format != "jpeg" && format != "webp" {
		return image.Config{}, withExit(exitUsage, fmt.Errorf("图片 %s 必须是 PNG、JPEG 或 WebP", path))
	}
	if maskFor != nil {
		if format != "png" {
			return image.Config{}, withExit(exitUsage, fmt.Errorf("--mask 图片 %s 必须是 PNG", path))
		}
		if cfg.Width != maskFor.Width || cfg.Height != maskFor.Height {
			return image.Config{}, withExit(exitUsage, fmt.Errorf("--mask 尺寸 %dx%d 必须与第一张原图 %dx%d 一致", cfg.Width, cfg.Height, maskFor.Width, maskFor.Height))
		}
		if !hasAlphaChannel(cfg.ColorModel) {
			return image.Config{}, withExit(exitUsage, fmt.Errorf("--mask 图片 %s 必须带透明通道，透明区域表示编辑范围", path))
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name": field, "filename": filepath.Base(path),
	}))
	h.Set("Content-Type", "image/"+format)
	part, err := w.CreatePart(h)
	if err != nil {
		return image.Config{}, fmt.Errorf("创建图片上传字段失败：%w", err)
	}
	n, err := io.Copy(part, io.NewSectionReader(f, 0, info.Size()))
	if err != nil {
		return image.Config{}, fmt.Errorf("写入图片上传内容失败：%w", err)
	}
	if n != info.Size() {
		return image.Config{}, withExit(exitUsage, fmt.Errorf("图片 %s 在读取时发生变化，请重试", path))
	}
	return cfg, nil
}

// 只读 PNG 的颜色模型，避免为了检查透明通道而解码整张大图。
func hasAlphaChannel(model color.Model) bool {
	if palette, ok := model.(color.Palette); ok {
		for _, c := range palette {
			if _, _, _, a := c.RGBA(); a < 0xffff {
				return true
			}
		}
		return false
	}
	return model == color.NRGBAModel || model == color.NRGBA64Model
}

func removeUpload(f *os.File) {
	_ = f.Close()
	_ = os.Remove(f.Name())
}
