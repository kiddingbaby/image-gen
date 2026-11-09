package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeEditPNG(t *testing.T, name string, width, height int, opaque bool) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	if opaque {
		for y := range height {
			for x := range width {
				img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return writeEditFile(t, name, buf.Bytes())
}

func writeEditFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEditUploadsImageAndSavesResult(t *testing.T) {
	resetEnv(t)
	input := filepath.Join(t.TempDir(), "原图, one.png")
	if err := os.WriteFile(input, samplePNG, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images/edits" {
			t.Errorf("请求 = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("未使用已配置的密钥")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("解析 multipart: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		for key, want := range map[string]string{
			"model": "edit-model", "prompt": "把背景换成雨夜街道",
		} {
			if got := r.FormValue(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		for _, key := range []string{"background", "moderation", "output_compression", "mask", "n", "size", "quality", "output_format"} {
			if _, ok := r.MultipartForm.Value[key]; ok {
				t.Errorf("不应发送未设置的 %s", key)
			}
		}
		files := r.MultipartForm.File["image[]"]
		if len(files) != 1 {
			t.Errorf("上传图片数 = %d, want 1", len(files))
			http.Error(w, "missing image", http.StatusBadRequest)
			return
		}
		if files[0].Filename != filepath.Base(input) || files[0].Header.Get("Content-Type") != "image/png" {
			t.Errorf("上传文件信息 = %+v", files[0])
		}
		f, err := files[0].Open()
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		raw, err := io.ReadAll(f)
		if err != nil || !bytes.Equal(raw, samplePNG) {
			t.Errorf("上传图片内容不一致: %v", err)
		}
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
	}))
	defer srv.Close()

	out := t.TempDir()
	stdout, err := runCLI(t, "edit", "把背景换成雨夜街道", "--image", input,
		"--config", filepath.Join(t.TempDir(), "absent.toml"), "--base-url", srv.URL,
		"--model", "edit-model", "--output-dir", out)
	if err != nil {
		t.Fatalf("编辑失败: %v", err)
	}
	path := strings.TrimSpace(stdout)
	if !filepath.IsAbs(path) || filepath.Dir(path) != out || path == input {
		t.Fatalf("输出路径 = %q", path)
	}
	for _, path := range []string{path, input} {
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, samplePNG) {
			t.Errorf("文件 %s 内容不一致: %v", path, err)
		}
	}
}

func TestEditUploadsReferencesAndMask(t *testing.T) {
	resetEnv(t)
	input := writeEditPNG(t, "source.png", 2, 1, true)
	mask := writeEditPNG(t, "mask.png", 2, 1, false)
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewNRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	reference := writeEditFile(t, "reference.data", jpegBytes.Bytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		files := r.MultipartForm.File["image[]"]
		masks := r.MultipartForm.File["mask"]
		if len(files) != 2 || len(masks) != 1 {
			t.Errorf("图片数=%d mask 数=%d", len(files), len(masks))
			http.Error(w, "missing files", http.StatusBadRequest)
			return
		}
		for i, file := range append(files, masks...) {
			wantPath := []string{input, reference, mask}[i]
			wantType := []string{"image/png", "image/jpeg", "image/png"}[i]
			if file.Filename != filepath.Base(wantPath) || file.Header.Get("Content-Type") != wantType {
				t.Errorf("第 %d 个上传文件信息错误: %+v", i, file)
			}
			f, err := file.Open()
			if err != nil {
				t.Error(err)
				return
			}
			got, err := io.ReadAll(f)
			f.Close()
			want, readErr := os.ReadFile(wantPath)
			if err != nil || readErr != nil || !bytes.Equal(got, want) {
				t.Errorf("第 %d 个上传文件字节不一致: %v / %v", i, err, readErr)
			}
		}
		for key, want := range map[string]string{
			"n": "2", "size": "auto", "quality": "high", "output_format": "webp",
			"background": "transparent", "moderation": "low", "output_compression": "0",
		} {
			if got := r.FormValue(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64}, map[string]any{"b64_json": samplePNGB64},
		}})
	}))
	defer srv.Close()
	out := t.TempDir()
	stdout, err := runCLI(t, "edit", "参考第二张图的风格修改选区", "--image", input,
		"--image", reference, "--mask", mask, "--base-url", srv.URL, "--model", "m",
		"--config", filepath.Join(t.TempDir(), "absent.toml"), "--output-dir", out,
		"--count", "2", "--size", "auto", "--quality", "high", "--format", "webp",
		"--background", "transparent", "--moderation", "low", "--output-compression", "0")
	if err != nil {
		t.Fatalf("参考图编辑失败: %v", err)
	}
	files := listFiles(t, out)
	if len(files) != 2 {
		t.Fatalf("输出文件 = %v", files)
	}
	for _, name := range files {
		if filepath.Ext(name) != ".png" || !strings.Contains(stdout, filepath.Join(out, name)) {
			t.Errorf("没有按实际图片格式落盘并返回路径: %s / %s", name, stdout)
		}
	}
}

// JSON 断言所用类型沿用公开输出字段，不依赖实现类型。
type editResult struct {
	Model  string `json:"model"`
	Images []struct {
		Path   string `json:"path"`
		Format string `json:"format"`
		Bytes  int    `json:"bytes"`
	} `json:"images"`
}

func decodeEditResult(t *testing.T, stdout string) editResult {
	t.Helper()
	var result editResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout 不是单一 JSON 对象: %q / %v", stdout, err)
	}
	return result
}

func TestEditRejectsInvalidInputsBeforeRequest(t *testing.T) {
	resetEnv(t)
	valid := writeEditPNG(t, "source.png", 2, 1, true)
	transparent := writeEditPNG(t, "mask.png", 2, 1, false)
	wrongSize := writeEditPNG(t, "wrong-size.png", 1, 1, false)
	textFile := writeEditFile(t, "fake.png", []byte("not an image"))
	empty := writeEditFile(t, "empty.png", nil)
	truncated := writeEditFile(t, "truncated.png", samplePNG[:12])
	oversized := writeEditFile(t, "large.png", samplePNG)
	if err := os.Truncate(oversized, 50_000_000); err != nil {
		t.Fatal(err)
	}
	tooMany := []string{}
	for range 17 {
		tooMany = append(tooMany, "--image", valid)
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing image", nil, "--image"},
		{"empty image argument", []string{"--image", ""}, "图片"},
		{"missing file", []string{"--image", filepath.Join(t.TempDir(), "missing.png")}, "图片"},
		{"directory", []string{"--image", t.TempDir()}, "普通文件"},
		{"empty file", []string{"--image", empty}, "图片"},
		{"unsupported format", []string{"--image", textFile}, "PNG"},
		{"truncated image", []string{"--image", truncated}, "图片"},
		{"oversized image", []string{"--image", oversized}, "50,000,000"},
		{"too many images", tooMany, "16"},
		{"invalid reference", []string{"--image", valid, "--image", textFile}, "PNG"},
		{"mask without image", []string{"--mask", transparent}, "--image"},
		{"missing mask", []string{"--image", valid, "--mask", filepath.Join(t.TempDir(), "missing.png")}, "图片"},
		{"invalid mask format", []string{"--image", valid, "--mask", textFile}, "PNG"},
		{"mask dimensions", []string{"--image", valid, "--mask", wrongSize}, "尺寸"},
		{"mask without alpha", []string{"--image", valid, "--mask", valid}, "透明通道"},
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
	}))
	defer srv.Close()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"edit", "修改背景", "--base-url", srv.URL, "--model", "m",
				"--config", filepath.Join(t.TempDir(), "absent.toml"), "--output-dir", t.TempDir()}
			stdout, err := runCLI(t, append(args, tc.args...)...)
			if err == nil || exitCodeOf(err) != 2 || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %v, want 退出码 2 且包含 %q", err, tc.want)
			}
			if stdout != "" {
				t.Errorf("无效输入不应输出图片路径: %q", stdout)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("无效输入发出了 %d 次网关请求", got)
	}
}

func TestEditUsesStdinConfigAndJSON(t *testing.T) {
	resetEnv(t)
	t.Setenv("IMAGE_GEN_API_KEY", "")
	input := writeEditPNG(t, "source.png", 1, 1, true)
	oldReader, oldTTY := stdinReader, stdinTTY
	stdinReader, stdinTTY = strings.NewReader("  改成水彩风格\n"), func() bool { return false }
	t.Cleanup(func() { stdinReader, stdinTTY = oldReader, oldTTY })
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/result.png" {
			w.Write(samplePNG)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("prompt") != "改成水彩风格" || r.FormValue("model") != "configured-model" ||
			r.Header.Get("Authorization") != "Bearer config-key" {
			t.Errorf("未使用 stdin 或配置值: %v / %s", r.MultipartForm.Value, r.Header.Get("Authorization"))
		}
		writeJSON(w, map[string]any{"data": []any{map[string]any{"url": srv.URL + "/result.png"}}})
	}))
	defer srv.Close()
	out := t.TempDir()
	configPath := writeEditFile(t, "config.toml", []byte(fmt.Sprintf(
		"base_url = %q\nmodel = 'configured-model'\nout_dir = %q\n[auth]\nvalue = 'config-key'\n", srv.URL, out)))
	stdout, err := runCLI(t, "--config", configPath, "edit", "-", "--image", input, "--json")
	if err != nil {
		t.Fatalf("编辑失败: %v", err)
	}
	result := decodeEditResult(t, stdout)
	if result.Model != "configured-model" || len(result.Images) != 1 {
		t.Fatalf("JSON = %+v", result)
	}
	img := result.Images[0]
	if img.Format != "png" || img.Bytes != len(samplePNG) || filepath.Dir(img.Path) != out {
		t.Errorf("图片元数据 = %+v", img)
	}
	raw, err := os.ReadFile(img.Path)
	if err != nil || !bytes.Equal(raw, samplePNG) {
		t.Errorf("下载图片内容不一致: %v", err)
	}
}

func TestEditRequestOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		attempts int32
		code     int
	}{
		{"rate limit then success", 429, 2, 0},
		{"rate limit exhausted", 429, 3, 5},
		{"upstream failure", 500, 1, 5},
		{"timeout", 0, 1, 4},
		{"redirect", 307, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetEnv(t)
			input := writeEditPNG(t, "source.png", 2, 1, true)
			var calls atomic.Int32
			bodies := make(chan []byte, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || r.ContentLength != int64(len(body)) {
					t.Errorf("上传未完成: %v / %d != %d", err, r.ContentLength, len(body))
				}
				bodies <- body
				if tc.status == 0 {
					time.Sleep(100 * time.Millisecond)
					return
				}
				if tc.status == 307 && attempt == 1 {
					http.Redirect(w, r, "/redirected-edits", http.StatusTemporaryRedirect)
					return
				}
				if (tc.status == 429 && (tc.code != 0 || attempt == 1)) || tc.status == 500 {
					w.WriteHeader(tc.status)
					writeJSON(w, map[string]any{"error": map[string]any{"message": "upstream unavailable"}})
					return
				}
				writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
			}))
			defer srv.Close()
			uploadTemp := t.TempDir()
			for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(key, uploadTemp)
			}
			out := t.TempDir()
			args := []string{"edit", "修改背景", "--image", input, "--base-url", srv.URL,
				"--model", "m", "--config", filepath.Join(t.TempDir(), "absent.toml"), "--output-dir", out}
			if tc.status == 0 {
				args = append(args, "--timeout", "40ms")
			}
			stdout, err := runCLI(t, args...)
			if tc.code == 0 {
				if err != nil {
					t.Fatalf("编辑失败: %v", err)
				}
				if raw, readErr := os.ReadFile(strings.TrimSpace(stdout)); readErr != nil || !bytes.Equal(raw, samplePNG) {
					t.Errorf("输出图片内容不一致: %v", readErr)
				}
			} else if err == nil || exitCodeOf(err) != tc.code || stdout != "" {
				t.Errorf("错误 = %v / stdout=%q, want 退出码 %d", err, stdout, tc.code)
			}
			if got := calls.Load(); got != tc.attempts {
				t.Errorf("请求次数 = %d, want %d", got, tc.attempts)
			}
			var firstBody []byte
			for range calls.Load() {
				body := <-bodies
				if firstBody == nil {
					firstBody = body
				} else if !bytes.Equal(firstBody, body) {
					t.Error("重试或重定向未完整重传 multipart")
				}
			}
			if files, _ := filepath.Glob(filepath.Join(uploadTemp, "image-gen-upload-*")); len(files) != 0 {
				t.Errorf("上传临时文件未清理: %v", files)
			}
		})
	}
}

func TestEditWebPWithPaletteMask(t *testing.T) {
	resetEnv(t)
	// 用 libwebp 的 lossless encoder 生成的 2x1 红色图片。
	webp, err := base64.StdEncoding.DecodeString("UklGRhwAAABXRUJQVlA4TA8AAAAvAQAAAAcQ/Y/+ByKi/wEA")
	if err != nil {
		t.Fatal(err)
	}
	input := writeEditFile(t, "source.webp", webp)
	maskImage := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Transparent, color.Black})
	maskImage.SetColorIndex(1, 0, 1)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, maskImage); err != nil {
		t.Fatal(err)
	}
	mask := writeEditFile(t, "palette-mask.png", encoded.Bytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		files := r.MultipartForm.File["image[]"]
		if len(files) != 1 || files[0].Header.Get("Content-Type") != "image/webp" {
			t.Error("WebP 没有按实际格式上传")
		}
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
	}))
	defer srv.Close()
	stdout, err := runCLI(t, "edit", "替换选区", "--image", input, "--mask", mask,
		"--base-url", srv.URL, "--model", "m", "--json",
		"--config", filepath.Join(t.TempDir(), "absent.toml"), "--output-dir", t.TempDir())
	if err != nil {
		t.Fatalf("WebP 编辑失败: %v", err)
	}
	result := decodeEditResult(t, stdout)
	if len(result.Images) != 1 {
		t.Fatalf("JSON = %+v", result)
	}
	if raw, err := os.ReadFile(result.Images[0].Path); err != nil || !bytes.Equal(raw, samplePNG) {
		t.Errorf("编辑产物不一致: %v", err)
	}
}
