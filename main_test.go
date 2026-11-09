package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

func TestEndpoint(t *testing.T) {
	cases := []struct {
		base string
		path string
		want string
	}{
		{"https://gw.example/v1", "/models", "https://gw.example/v1/models"},
		{"https://gw.example/v1/", "/models", "https://gw.example/v1/models"},
		{"https://gw.example", "/models", "https://gw.example/v1/models"},
	}
	for _, c := range cases {
		if got := endpoint(c.base, c.path); got != c.want {
			t.Errorf("endpoint(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

func TestValidateSize(t *testing.T) {
	for _, size := range []string{"auto", "1024x1024", "1024x1536"} {
		if _, err := validateSize(size); err != nil {
			t.Errorf("validateSize(%q) unexpected error: %v", size, err)
		}
	}
	for _, size := range []string{"", "0x0", "-1x100", "100x", "999999999999999999999x1"} {
		if _, err := validateSize(size); err == nil {
			t.Errorf("validateSize(%q) expected error", size)
		}
	}
}

func TestTomlStringRoundTrip(t *testing.T) {
	value := `sk-"weird\key`
	var got struct {
		Value string `toml:"value"`
	}
	if err := toml.Unmarshal([]byte("value = "+tomlString(value)), &got); err != nil {
		t.Fatalf("toml parse failed: %v", err)
	}
	if got.Value != value {
		t.Errorf("round trip = %q, want %q", got.Value, value)
	}
}

func TestResolveAuthValue(t *testing.T) {
	t.Setenv("IMAGE_GEN_API_KEY", "")
	key, err := resolveAuth(authConfig{Value: "secret"})
	if err != nil {
		t.Fatalf("resolveAuth failed: %v", err)
	}
	if key != "secret" {
		t.Errorf("resolveAuth = %q, want secret", key)
	}
}

func TestValidateSizeSyntax(t *testing.T) {
	ok := []string{"auto", "256x256", "512x512", "1024x1024", "1536x1024", "1024x1536", "3840x2160", "2160x3840", "1023x1024", "1000x1000", "3840x1080", "4096x2048", "3840x3840"}
	for _, size := range ok {
		if _, err := validateSize(size); err != nil {
			t.Errorf("validateSize(%q) 应当通过，却报错：%v", size, err)
		}
	}
	// 模型专属的边长、比例与像素预算交给上游；本地只检查通用语法。
	bad := []string{"0x0", "abc", "1.5x100", "99999999999999999999999x1"}
	for _, size := range bad {
		if _, err := validateSize(size); err == nil {
			t.Errorf("validateSize(%q) 应当被拒", size)
		}
	}
}

func TestValidateQuality(t *testing.T) {
	for _, q := range []string{"low", "medium", "high", "xhigh", "max", "auto"} {
		if _, err := validateQuality(q); err != nil {
			t.Errorf("validateQuality(%q) 应当通过：%v", q, err)
		}
	}
	for _, q := range []string{"", "ultra", "HIGH"} {
		if _, err := validateQuality(q); err == nil {
			t.Errorf("validateQuality(%q) 应当被拒", q)
		}
	}
}

func TestNormalizeOutputFormat(t *testing.T) {
	cases := map[string]string{"png": "png", "jpeg": "jpeg", "jpg": "jpeg", "webp": "webp"}
	for in, want := range cases {
		got, err := normalizeOutputFormat(in)
		if err != nil || got != want {
			t.Errorf("normalizeOutputFormat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := normalizeOutputFormat("bmp"); err == nil {
		t.Error("normalizeOutputFormat(\"bmp\") 应当被拒")
	}
}

func TestBuildPayloadOmitsUnsetOptionals(t *testing.T) {
	base := buildPayload(&options{n: 1}, "m", "p", "1024x1024", "medium", "png", "", "")
	for _, key := range []string{"background", "moderation", "output_compression"} {
		if _, ok := base[key]; ok {
			t.Errorf("未设置的 %s 不应出现在请求体里", key)
		}
	}
	full := buildPayload(&options{n: 2, supplied: map[string]bool{"count": true, "format": true}, outputCompressionSet: true, outputCompression: 80},
		"m", "p", "1024x1024", "high", "webp", "transparent", "low")
	for _, key := range []string{"background", "moderation", "output_compression"} {
		if _, ok := full[key]; !ok {
			t.Errorf("显式设置的 %s 应当出现在请求体里", key)
		}
	}
	if full["output_format"] != "webp" || full["n"] != 2 {
		t.Errorf("请求体字段不对：%v", full)
	}
}

func TestSniffImageExt(t *testing.T) {
	cases := map[string]string{
		"\x89PNG\r\n\x1a\nrest":        "png",
		"\xff\xd8\xff\xe0rest":         "jpeg",
		"RIFF\x00\x00\x00\x00WEBPVP8 ": "webp",
		"GIF89a":                       "gif",
		"not-an-image":                 "",
	}
	for data, want := range cases {
		if got := sniffImageExt([]byte(data)); got != want {
			t.Errorf("sniffImageExt(%q) = %q, want %q", data, got, want)
		}
	}
}

func TestTimeoutOfValidation(t *testing.T) {
	for _, d := range []time.Duration{time.Second, 100 * time.Millisecond} {
		got, err := timeoutOf(&options{timeout: d})
		if err != nil || got != d {
			t.Errorf("timeoutOf(%v) = %v, %v; want %v", d, got, err, d)
		}
	}

	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := timeoutOf(&options{timeout: d}); err == nil {
			t.Errorf("timeoutOf(%v) 应当报错", d)
		}
	}
}

func TestSafeFileStem(t *testing.T) {
	cases := map[string]string{
		"../../evil\\..\\name":   "evil_.._name",
		"provider/gpt-image":     "gpt-image",
		"../../":                 "image",
		"...":                    "image",
		strings.Repeat("a", 100): strings.Repeat("a", 80),
	}
	for in, want := range cases {
		if got := safeFileStem(in); got != want {
			t.Errorf("safeFileStem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("IMAGE_GEN_CONFIG", "/tmp/explicit.toml")
	if got := defaultConfigPath(); got != "/tmp/explicit.toml" {
		t.Errorf("IMAGE_GEN_CONFIG 应优先，得到 %q", got)
	}

	// 默认路径必须跟随各平台的用户配置目录：Linux 走 XDG，
	// macOS 走 ~/Library/Application Support，Windows 走 %AppData%。
	t.Setenv("IMAGE_GEN_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	want, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("当前平台取不到用户配置目录：%v", err)
	}
	p := filepath.Join(want, "image-gen", "config.toml")
	if got := defaultConfigPath(); got != p {
		t.Errorf("defaultConfigPath() = %q, want %q", got, p)
	}
}

func TestWriteConfigFileGuardAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	if err := writeConfigFile(path, "https://gw/v1", "m", []string{"[auth]", `env = "K"`}, false); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	assertSecretMode(t, path)
	if err := writeConfigFile(path, "https://gw2/v1", "", nil, false); err == nil {
		t.Error("已存在且未加 --force 时应当报错")
	}
	if err := writeConfigFile(path, "https://gw2/v1", "", nil, true); err != nil {
		t.Errorf("--force 应当允许覆盖：%v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "gw2") {
		t.Errorf("覆盖后内容未更新：%s", data)
	}
}

func TestExitCodeOf(t *testing.T) {
	if got := exitCodeOf(usageErr(errors.New("x"))); got != exitUsage {
		t.Errorf("usage 错误应映射到 %d，得到 %d", exitUsage, got)
	}
	if got := exitCodeOf(errors.New("plain")); got != exitGeneral {
		t.Errorf("普通错误应映射到 %d，得到 %d", exitGeneral, got)
	}
	// 已分类的错误不应被外层覆盖
	wrapped := withExit(exitConfig, usageErr(errors.New("x")))
	if got := exitCodeOf(wrapped); got != exitUsage {
		t.Errorf("外层不应覆盖内层分类，得到 %d", got)
	}
}

func usageErr(err error) error { return withExit(exitUsage, err) }

func TestPickImageModelsPrefersDeclaredMode(t *testing.T) {
	// 有 mode：只列 image_generation，读图模型不算
	entries := []modelEntry{
		{ID: "hubimg/gpt-image-2.5-flare", Mode: "image_generation"},
		{ID: "hub/medium/1024-x-1024/gpt-image-1"},
		{ID: "gpt-4o", Mode: "chat"},
		{ID: "hubimg/gpt-image-2.5-sunburst", Mode: "image_generation"},
	}
	ids, confident := pickImageModels(entries)
	if !confident {
		t.Fatal("网关声明了 mode，应当 confident")
	}
	if len(ids) != 2 || ids[0] != "hubimg/gpt-image-2.5-flare" || ids[1] != "hubimg/gpt-image-2.5-sunburst" {
		t.Errorf("只应列出两个生图模型，实际：%v", ids)
	}

	// 没有 mode：无法判断，全量返回且不算 confident
	plain := []modelEntry{{ID: "b"}, {ID: "a"}}
	ids, confident = pickImageModels(plain)
	if confident || len(ids) != 2 || ids[0] != "a" {
		t.Errorf("应全量返回并标记为不确定，实际：%v confident=%v", ids, confident)
	}

	// 有 mode 但一个都不匹配：不能直接给空列表
	none := []modelEntry{{ID: "x", Mode: "chat"}}
	ids, confident = pickImageModels(none)
	if confident || len(ids) != 1 {
		t.Errorf("无匹配时应退回全量，实际：%v confident=%v", ids, confident)
	}
}

func TestResolveTargetPrecedence(t *testing.T) {
	cfg := config{BaseURL: "https://cfg/v1", Auth: authConfig{Value: "cfg-key"}}
	o := &options{}

	t.Setenv("IMAGE_GEN_BASE_URL", "https://env/v1")
	t.Setenv("IMAGE_GEN_API_KEY", "env-key")
	got, err := resolveTarget(o, cfg)
	if err != nil {
		t.Fatalf("resolveTarget 失败：%v", err)
	}
	if got.baseURL != "https://env/v1" || got.key != "env-key" {
		t.Errorf("环境变量应覆盖配置：%+v", got)
	}

	t.Setenv("IMAGE_GEN_BASE_URL", "")
	t.Setenv("IMAGE_GEN_API_KEY", "")
	got, err = resolveTarget(o, cfg)
	if err != nil {
		t.Fatalf("resolveTarget 失败：%v", err)
	}
	if got.baseURL != "https://cfg/v1" || got.key != "cfg-key" {
		t.Errorf("无环境变量时应回落到配置：%+v", got)
	}

	if _, err := resolveTarget(&options{}, config{}); err == nil {
		t.Error("没有任何 base_url 时应当报错")
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	raw := "网关报错：模型不存在"
	got := truncate(raw, 10) // 10 字节会切在「报」字的中间
	if !utf8.ValidString(got) {
		t.Errorf("截断后出现无效 UTF-8：%q", got)
	}
	if truncate("short", 100) != "short" {
		t.Error("未超长时不应改动")
	}
}

func TestFilterModels(t *testing.T) {
	ids := []string{"gpt-image-2.5", "grok-imagine-image-2.0", "Qwen/Qwen-Image", "gpt-4o"}
	got := filterModels(ids, "IMAGE")
	want := []string{"gpt-image-2.5", "grok-imagine-image-2.0", "Qwen/Qwen-Image"}
	if len(got) != len(want) {
		t.Fatalf("匹配数 %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个是 %q，期望 %q", i, got[i], want[i])
		}
	}
	if n := len(filterModels(ids, "zzz")); n != 0 {
		t.Errorf("无匹配时应为空，得到 %d 个", n)
	}
}

// assertSecretMode 断言配置文件只有所有者可读写。
//
// Windows 的文件权限是只读位加 ACL，os.Stat 只能报 0666，POSIX 权限位在这个
// 平台上不可观测——与其断言一个拿不到的值，不如在 Windows 上跳过这一项；
// 真实保护由 ACL 与用户目录下的默认继承决定，不是 0600 位。
func assertSecretMode(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("取 %s 的文件信息失败：%v", path, err)
	}
	if perm := info.Mode().Perm(); perm != fileModeSecret {
		t.Errorf("%s 权限 = %#o, want %#o", path, perm, fileModeSecret)
	}
}
