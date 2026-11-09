package main

// 集成测试：起一个假的 OpenAI 兼容网关（httptest），走 cmdGenerate / cmdModels
// 的完整链路——组请求、发 HTTP、解析响应、落盘、报错分类。
//
// 与 main_test.go 的分工：那边测纯函数，这边测「一次调用会发生什么」。
// 这里覆盖的都是单元测试看不到的路径：鉴权头、重试、超时、下载、退出码。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 1x1 透明 PNG，只用来验证落盘与格式识别。
var samplePNG, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

const samplePNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// resetEnv 把影响配置解析的环境变量清空，避免开发机上真实的网关配置漏进测试。
func resetEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"IMAGE_GEN_BASE_URL", "IMAGE_GEN_API_KEY", "IMAGE_GEN_MODEL",
		"IMAGE_GEN_OUT_DIR", "IMAGE_GEN_CONFIG",
	} {
		t.Setenv(k, "")
	}
	// 大多数用例只需要一个可用的密钥；需要验证其他密钥来源的用例再清空它。
	t.Setenv("IMAGE_GEN_API_KEY", "test-key")
	// 重试间隔默认是 1s + 2s；测试里压到 1ms，否则重试用例要白等 3 秒。
	old := retryDelay
	retryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelay = old })
}

// newGateway 起一个假网关。generate/models 传 nil 表示该路径不该被访问。
func newGateway(t *testing.T, generate, models http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// generatePath 是 "/images/generations"，网关前缀是 /v1。
	if generate != nil {
		mux.HandleFunc("/v1"+generatePath, generate)
	}
	if models != nil {
		mux.HandleFunc("/v1"+modelsPath, models)
	}
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(samplePNG)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// runCLI 复刻 main() 的分发逻辑，但不调用 os.Exit，这样能断言退出码。
// 返回错误（用 exitCodeOf 取码）和捕获到的 stdout + 告警。
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return captureRun(t, func() error { return run(args) })
}

// captureRun 把 stdout 与告警重定向到内存，跑完 fn 再还原。
func captureRun(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	var warn bytes.Buffer
	oldWarn := warnWriter
	warnWriter = &warn
	t.Cleanup(func() { warnWriter = oldWarn })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	// 边写边读：管道缓冲只有几十 KB，`completion bash` 这种十几 KB 往上的输出
	// 若等 fn 跑完再读，写满缓冲就永久阻塞（在 CI 容器里就是这样挂住的）。
	var buf bytes.Buffer
	drained := make(chan error, 1)
	go func() {
		_, err := io.Copy(&buf, r)
		drained <- err
	}()

	oldStdout := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = oldStdout

	// 关写端后读协程才会结束；读端要等它读完再关，否则会截断输出。
	readErr := w.Close()
	if err != nil {
		t.Fatalf("关管道失败: %v", readErr)
	}
	if err := <-drained; err != nil {
		t.Fatalf("读取 stdout 失败: %v", err)
	}
	_ = r.Close()
	return buf.String() + warn.String(), runErr
}

// generateArgs 是 generate 用例共用的命令前缀：固定子命令 + 隔离配置路径。
func generateArgs(t *testing.T) []string {
	t.Helper()
	return []string{"generate", "--config", filepath.Join(t.TempDir(), "absent.toml")}
}

// modelsArgs 是 models 用例共用的命令前缀。
func modelsArgs(t *testing.T) []string {
	t.Helper()
	return []string{"models", "--config", filepath.Join(t.TempDir(), "absent.toml")}
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s 失败: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func TestGenerateSavesBase64Image(t *testing.T) {
	resetEnv(t)
	var gotAuth, gotType string
	var gotBody map[string]any
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	out := t.TempDir()
	stdout, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "test-model", "--output-dir", out, "一只猫")...)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, 期望 Bearer test-key", gotAuth)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, 期望 application/json", gotType)
	}
	for _, k := range []string{"model", "prompt"} {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("请求体缺少字段 %q: %v", k, gotBody)
		}
	}
	if gotBody["model"] != "test-model" || gotBody["prompt"] != "一只猫" {
		t.Errorf("请求体 model/prompt = %v/%v", gotBody["model"], gotBody["prompt"])
	}

	files := listFiles(t, out)
	if len(files) != 1 || filepath.Ext(files[0]) != ".png" {
		t.Fatalf("输出文件 = %v, 期望 1 个 .png", files)
	}
	data, err := os.ReadFile(filepath.Join(out, files[0]))
	if err != nil {
		t.Fatalf("读产物失败: %v", err)
	}
	if !bytes.Equal(data, samplePNG) {
		t.Errorf("产物字节与响应不一致")
	}
	if !strings.Contains(stdout, filepath.Join(out, files[0])) {
		t.Errorf("stdout 没有打印产物路径: %q", stdout)
	}
}

func TestGenerateDownloadsURLImage(t *testing.T) {
	resetEnv(t)
	var srv *httptest.Server
	srv = newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"url": srv.URL + "/img/a.png"},
		}})
	}, nil)

	out := t.TempDir()
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "m", "--output-dir", out, "x")...)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	files := listFiles(t, out)
	if len(files) != 1 || filepath.Ext(files[0]) != ".png" {
		t.Fatalf("输出文件 = %v, 期望 1 个 .png", files)
	}
}

// 下载失败发生在拿到 200 响应之后，属于独立的网络往返，
// 应归为 exit 4（可重试），而不是 exit 5（上游拒绝）。
func TestGenerateDownloadFailureIsNetworkError(t *testing.T) {
	resetEnv(t)
	var srv *httptest.Server
	srv = newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"url": srv.URL + "/gone.png"},
		}})
	}, nil)
	out := t.TempDir()
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "m", "--output-dir", out, "x")...)
	if err == nil {
		t.Fatal("期望下载失败，实际成功")
	}
	if got := exitCodeOf(err); got != exitNetwork {
		t.Errorf("退出码 = %d, 期望 %d（网络错误）: %v", got, exitNetwork, err)
	}
	if files := listFiles(t, out); len(files) != 0 {
		t.Errorf("失败时不应落盘任何文件，实际 %v", files)
	}
}

func TestGenerateUpstreamErrorIsExit5(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": map[string]any{"message": "bad model"}})
	}, nil)

	out := t.TempDir()
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "m", "--output-dir", out, "x")...)
	if err == nil {
		t.Fatal("期望失败，实际成功")
	}
	if got := exitCodeOf(err); got != exitUpstream {
		t.Errorf("退出码 = %d, 期望 %d（上游错误）", got, exitUpstream)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("错误信息没带上 HTTP 状态码: %v", err)
	}
}

func TestGenerateRetriesOn429ThenSucceeds(t *testing.T) {
	resetEnv(t)
	var calls atomic.Int32
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "slow down"}})
			return
		}
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	out := t.TempDir()
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "m", "--output-dir", out, "x")...)
	if err != nil {
		t.Fatalf("429 重试后应成功，实际: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("请求次数 = %d, 期望 3", got)
	}
	if files := listFiles(t, out); len(files) != 1 {
		t.Errorf("输出文件 = %v, 期望 1 个", files)
	}
}

func TestGenerateGivesUpAfterRetryLimit(t *testing.T) {
	resetEnv(t)
	var calls atomic.Int32
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, map[string]any{"error": map[string]any{"message": "slow down"}})
	}, nil)

	out := t.TempDir()
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "m", "--output-dir", out, "x")...)
	if err == nil {
		t.Fatal("持续 429 应最终失败")
	}
	if got := exitCodeOf(err); got != exitUpstream {
		t.Errorf("退出码 = %d, 期望 %d", got, exitUpstream)
	}
	// 首次 + maxRateLimitRetries 次重试，不能无限重试。
	if got, want := calls.Load(), int32(maxRateLimitRetries+1); got != want {
		t.Errorf("请求次数 = %d, 期望 %d", got, want)
	}
}

func TestGenerateTimeoutIsNetworkError(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	out := t.TempDir()
	start := time.Now()
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1", "--timeout", "100ms",
		"--model", "m", "--output-dir", out, "x")...)
	if err == nil {
		t.Fatal("超时应失败")
	}
	if got := exitCodeOf(err); got != exitNetwork {
		t.Errorf("退出码 = %d, 期望 %d（网络错误）", got, exitNetwork)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("超时没有生效，耗时 %v", elapsed)
	}
}

func TestGenerateMultipleImagesGetIndexSuffix(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
			map[string]any{"b64_json": samplePNGB64},
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	out := t.TempDir()
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1", "--count", "3",
		"--model", "m", "--output-dir", out, "x")...)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	files := listFiles(t, out)
	if len(files) != 3 {
		t.Fatalf("输出文件 = %v, 期望 3 个", files)
	}
	for i := 0; i < 3; i++ {
		if !strings.HasSuffix(files[i], "-"+strconv.Itoa(i)+".png") {
			t.Errorf("第 %d 个文件 %q 缺少 -%d 后缀", i, files[i], i)
		}
	}
}

// 网关可能不采纳 output_format：请求 webp 却返回 PNG 时，
// 扩展名必须跟着实际字节走，并且要给出提示。
func TestGenerateRenamesToActualFormat(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	out := t.TempDir()
	stdout, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "m", "--format", "webp", "--output-dir", out, "x")...)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	files := listFiles(t, out)
	if len(files) != 1 || filepath.Ext(files[0]) != ".png" {
		t.Fatalf("输出文件 = %v, 期望按实际字节命名为 .png", files)
	}
	if !strings.Contains(stdout, "实际格式") {
		t.Errorf("没有提示格式不符: %q", stdout)
	}
}

func TestModelsListsDeclaredImageModelsOnly(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]string{
			{"id": "img-a", "mode": "image_generation"},
			{"id": "chat-b", "mode": "chat"},
			{"id": "img-c", "mode": "image_generation"},
		}})
	})

	stdout, _ := runCLI(t, append(modelsArgs(t),
		"--base-url", srv.URL+"/v1")...)
	if !strings.Contains(stdout, "img-a") || !strings.Contains(stdout, "img-c") {
		t.Errorf("缺少图像模型: %q", stdout)
	}
	if strings.Contains(stdout, "chat-b") {
		t.Errorf("不该列出非图像模型: %q", stdout)
	}
}

func TestModelsKeywordFiltersDeclaredImageModels(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]string{
			{"id": "img-apple", "mode": "image_generation"},
			{"id": "img-banana", "mode": "image_generation"},
			{"id": "chat-apple", "mode": "chat"},
		}})
	})

	stdout, _ := runCLI(t, append(modelsArgs(t),
		"--base-url", srv.URL+"/v1", "apple")...)
	if !strings.Contains(stdout, "img-apple") {
		t.Errorf("缺少匹配的图像模型: %q", stdout)
	}
	if strings.Contains(stdout, "img-banana") || strings.Contains(stdout, "chat-apple") {
		t.Errorf("关键词过滤没有生效: %q", stdout)
	}
}

func TestModelsWithoutCapabilitySummarizesLargeList(t *testing.T) {
	resetEnv(t)
	entries := make([]map[string]string, 0, 3000)
	for i := 0; i < 3000; i++ {
		entries = append(entries, map[string]string{"id": "model-" + strconv.Itoa(i)})
	}
	srv := newGateway(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": entries})
	})

	stdout, _ := runCLI(t, append(modelsArgs(t),
		"--base-url", srv.URL+"/v1")...)
	if !strings.Contains(stdout, "3000") {
		t.Errorf("应给出总数: %q", stdout)
	}
	if !strings.Contains(stdout, "关键词") {
		t.Errorf("应提示用关键词过滤: %q", stdout)
	}
}

func TestModelsKeywordFilterAndTruncation(t *testing.T) {
	resetEnv(t)
	entries := make([]map[string]string, 0, 250)
	for i := 0; i < 250; i++ {
		entries = append(entries, map[string]string{"id": "image-" + strconv.Itoa(i)})
	}
	entries = append(entries, map[string]string{"id": "chat-only"})
	srv := newGateway(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": entries})
	})

	stdout, _ := runCLI(t, append(modelsArgs(t),
		"--base-url", srv.URL+"/v1", "image")...)
	if strings.Contains(stdout, "chat-only") {
		t.Errorf("关键词过滤没有生效: %q", stdout)
	}
	if !strings.Contains(stdout, "还有 50 个") {
		t.Errorf("超过打印上限应截断并提示: %q", stdout)
	}
}

// 配置优先级：命令行 > 环境变量 > config.toml。这里验证 config.toml 能独立生效。
func TestGenerateUsesConfigFileValues(t *testing.T) {
	resetEnv(t)
	var gotBody map[string]any
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(
		"base_url = \""+srv.URL+"/v1\"\nmodel = \"cfg-model\"\n"), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	out := t.TempDir()
	_, err := runCLI(t, "generate", "--config", cfgPath, "--output-dir", out, "x")
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if gotBody["model"] != "cfg-model" {
		t.Errorf("model = %v, 期望取自 config.toml 的 cfg-model", gotBody["model"])
	}
}

func TestGenerateWithoutGatewayIsExit3(t *testing.T) {
	resetEnv(t)
	out := t.TempDir()
	base := generateArgs(t)
	// 不带 --model 时也必须报缺网关（3）：若先报缺模型（2），用户会按提示去跑
	// 同样需要网关的 `image-gen models`，卡在同一步。
	cases := map[string][]string{
		"带 --model":  append(append([]string{}, base...), "--model", "m", "--output-dir", out, "x"),
		"不带 --model": append(append([]string{}, base...), "--output-dir", out, "x"),
	}
	for name, args := range cases {
		_, err := runCLI(t, args...)
		if err == nil {
			t.Fatalf("%s：没有 base_url 时应失败", name)
		}
		if got := exitCodeOf(err); got != exitConfig {
			t.Errorf("%s：退出码 = %d, 期望 %d（配置缺失）", name, got, exitConfig)
		}
	}
}

func TestGenerateMissingKeyIsExit3(t *testing.T) {
	resetEnv(t)
	t.Setenv("IMAGE_GEN_API_KEY", "")
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", "http://gw.example/v1", "--model", "m", "x")...)
	if err == nil {
		t.Fatal("没有密钥时应失败")
	}
	if got := exitCodeOf(err); got != exitConfig {
		t.Errorf("退出码 = %d, 期望 %d（配置缺失）", got, exitConfig)
	}
	if !strings.Contains(err.Error(), "IMAGE_GEN_API_KEY") {
		t.Errorf("错误信息应说明密钥来源: %v", err)
	}
}

// --init 的非交互写法是 agent / CI 的主要接入方式，写出来的配置必须能直接用。
func TestInitArgsWritesUsableConfig(t *testing.T) {
	resetEnv(t)
	var gotAuth string
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	_, err := runCLI(t, "init", "--config", cfgPath,
		"--base-url", srv.URL+"/v1", "--auth-env", "TEST_GW_KEY", "--model", "init-model")
	if err != nil {
		t.Fatalf("--init 失败: %v", err)
	}

	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("配置文件没写出来: %v", err)
	}
	assertSecretMode(t, cfgPath)

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("回读配置失败: %v", err)
	}
	if cfg.BaseURL != srv.URL+"/v1" || cfg.Model != "init-model" || cfg.Auth.Env != "TEST_GW_KEY" {
		t.Fatalf("配置内容不符: %+v", cfg)
	}

	// 用刚生成的配置跑一次生成：密钥从 [auth] env 指向的变量取。
	t.Setenv("IMAGE_GEN_API_KEY", "")
	t.Setenv("TEST_GW_KEY", "sk-from-env")
	out := t.TempDir()
	_, err = runCLI(t, "generate", "--config", cfgPath, "--output-dir", out, "x")
	if err != nil {
		t.Fatalf("用生成的配置生成失败: %v", err)
	}
	if gotAuth != "Bearer sk-from-env" {
		t.Errorf("Authorization = %q, 期望从 [auth] env 取到 sk-from-env", gotAuth)
	}
	if files := listFiles(t, out); len(files) != 1 {
		t.Errorf("输出文件 = %v, 期望 1 个", files)
	}
}

func TestInitArgsRefusesOverwriteWithoutForce(t *testing.T) {
	resetEnv(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("base_url = \"http://old/v1\"\n"), 0o600); err != nil {
		t.Fatalf("预置配置失败: %v", err)
	}

	_, err := runCLI(t, "init", "--config", cfgPath, "--base-url", "http://new/v1")
	if err == nil {
		t.Fatal("已存在的配置在没有 --force 时应拒绝覆盖")
	}

	_, err = runCLI(t, "init", "--config", cfgPath, "--base-url", "http://new/v1", "--force")
	if err != nil {
		t.Fatalf("--force 时应覆盖: %v", err)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil || cfg.BaseURL != "http://new/v1" {
		t.Fatalf("覆盖后内容不符: %+v (%v)", cfg, err)
	}
}

// 没有终端又没给 --base-url 时，不能卡在等输入上，要直接给出替代写法。
func TestInitWithoutTerminalOrArgsIsExit2(t *testing.T) {
	resetEnv(t)
	// go test 的 stdin 是 /dev/null（字符设备），会被当成终端；
	// 换成管道才能模拟 agent / CI 的非终端环境。
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	oldStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = pw.Close()
		_ = pr.Close()
	})

	_, err = runCLI(t, "init", "--config", filepath.Join(t.TempDir(), "c.toml"))
	if err == nil {
		t.Fatal("非终端且无参数时应报错")
	}
	if got := exitCodeOf(err); got != exitUsage {
		t.Errorf("退出码 = %d, 期望 %d（用法错误）", got, exitUsage)
	}
	if !strings.Contains(err.Error(), "--auth-env") {
		t.Errorf("错误信息应给出非交互写法: %v", err)
	}
}

// [auth] command 是 pass / op 这类密钥管理器的接入方式。
func TestAuthCommandProvidesKey(t *testing.T) {
	resetEnv(t)
	var gotAuth string
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	t.Setenv("IMAGE_GEN_API_KEY", "")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	content := "base_url = \"" + srv.URL + "/v1\"\nmodel = \"m\"\n\n[auth]\ncommand = \"printf\"\nargs = [\"sk-from-command\"]\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	_, err := runCLI(t, "generate", "--config", cfgPath, "--output-dir", t.TempDir(), "x")
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if gotAuth != "Bearer sk-from-command" {
		t.Errorf("Authorization = %q, 期望从 [auth] command 取到密钥", gotAuth)
	}
}

// 命令与参数分开传，参数里可以有空格，不会被 shell 式拆分破坏。
func TestInitArgsWithAuthCommand(t *testing.T) {
	resetEnv(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	_, err := runCLI(t, "init", "--config", cfgPath,
		"--base-url", "http://gw.example/v1",
		"--auth-command", "pass",
		"--auth-command-arg", "show",
		"--auth-command-arg", "ai/image key")
	if err != nil {
		t.Fatalf("init 失败: %v", err)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("回读配置失败: %v", err)
	}
	if cfg.Auth.Command != "pass" {
		t.Errorf("command = %q, 期望 pass", cfg.Auth.Command)
	}
	if len(cfg.Auth.Args) != 2 || cfg.Auth.Args[0] != "show" || cfg.Auth.Args[1] != "ai/image key" {
		t.Errorf("args = %v, 期望 [show ai/image key]", cfg.Auth.Args)
	}
}

func TestInitAuthCommandArgRequiresCommand(t *testing.T) {
	resetEnv(t)
	_, err := runCLI(t, "init",
		"--config", filepath.Join(t.TempDir(), "config.toml"),
		"--base-url", "http://gw.example/v1",
		"--auth-command-arg", "show")
	if err == nil {
		t.Fatal("只给 --auth-command-arg 应报错")
	}
	if got := exitCodeOf(err); got != exitUsage {
		t.Errorf("退出码 = %d, 期望 %d", got, exitUsage)
	}
}

func TestInitAuthCommandPathWithSpaces(t *testing.T) {
	resetEnv(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	command := filepath.Join(t.TempDir(), "tool with space", "get-key")
	_, err := runCLI(t, "init",
		"--config", cfgPath,
		"--base-url", "http://gw.example/v1",
		"--auth-command", command)
	if err != nil {
		t.Fatalf("带空格的可执行文件路径应被接受: %v", err)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if cfg.Auth.Command != command {
		t.Errorf("command = %q, 期望 %q", cfg.Auth.Command, command)
	}
}

// 上游给的回传 URL 必须是 http/https，否则不该发请求。
func TestGenerateRejectsNonHTTPImageURL(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"url": "ftp://example.com/a.png"},
		}})
	}, nil)

	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "m", "--output-dir", t.TempDir(), "x")...)
	if err == nil {
		t.Fatal("非 http(s) 的图片 URL 应被拒绝")
	}
	if got := exitCodeOf(err); got != exitNetwork {
		t.Errorf("退出码 = %d, 期望 %d", got, exitNetwork)
	}
}

// 输出目录不可写时，错误要能带上是哪个目录。
func TestGenerateReportsUnwritableOutDir(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	// 用一个普通文件当输出目录，MkdirAll 会失败。
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", srv.URL+"/v1",
		"--model", "m", "--output-dir", blocker, "x")...)
	if err == nil {
		t.Fatal("输出目录不可写时应失败")
	}
	if !strings.Contains(err.Error(), "输出目录") {
		t.Errorf("错误信息没点名输出目录: %v", err)
	}
}

func TestModelRequired(t *testing.T) {
	resetEnv(t)
	_, err := runCLI(t, append(generateArgs(t),
		"--base-url", "http://gw.example/v1", "x")...)
	if err == nil {
		t.Fatal("没给模型时应失败")
	}
	if got := exitCodeOf(err); got != exitUsage {
		t.Errorf("退出码 = %d, 期望 %d（用法错误）", got, exitUsage)
	}
	if !strings.Contains(err.Error(), "image-gen models") {
		t.Errorf("错误信息应提示先查模型: %v", err)
	}
}

// runCommand 走真实的 run() 分发，用来覆盖子命令、帮助与机器输出。
func runCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runCLI(t, args...)
}

func TestRootHelpListsSubcommands(t *testing.T) {
	resetEnv(t)
	stdout, err := runCommand(t, "--help")
	if err != nil {
		t.Fatalf("--help 失败: %v", err)
	}
	for _, want := range []string{"generate", "models", "init", "version", "help"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("根帮助缺少 %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "stdin") {
		t.Errorf("根帮助应说明支持 stdin:\n%s", stdout)
	}
}

func TestHelpGenerateShowsFlags(t *testing.T) {
	resetEnv(t)
	stdout, err := runCommand(t, "help", "generate")
	if err != nil {
		t.Fatalf("help generate 失败: %v", err)
	}
	for _, want := range []string{"usage:", "--json", "--model", "stdin", "--output-dir"} {
		if !strings.Contains(strings.ToLower(stdout), want) {
			t.Errorf("generate 帮助缺少 %q:\n%s", want, stdout)
		}
	}
}

func TestModelsSubcommandJSON(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"id": "img-a", "mode": "image_generation"},
			map[string]any{"id": "chat-a", "mode": "chat"},
		}})
	})

	stdout, err := runCommand(t,
		"models",
		"--config", filepath.Join(t.TempDir(), "absent.toml"),
		"--base-url", srv.URL+"/v1",
		"--json",
	)
	if err != nil {
		t.Fatalf("models --json 失败: %v", err)
	}
	var got modelsResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, stdout)
	}
	if !got.CapabilityDeclared {
		t.Error("capability_declared 应为 true")
	}
	if len(got.Models) != 1 || got.Models[0] != "img-a" {
		t.Errorf("models = %#v, 期望 [img-a]", got.Models)
	}
}

func TestModelsSubcommandKeywordNoMatch(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"id": "img-a", "mode": "image_generation"},
		}})
	})

	_, err := runCommand(t,
		"models", "missing",
		"--config", filepath.Join(t.TempDir(), "absent.toml"),
		"--base-url", srv.URL+"/v1",
	)
	if err == nil {
		t.Fatal("没有匹配模型时应失败")
	}
	if got := exitCodeOf(err); got != exitUsage {
		t.Errorf("退出码 = %d, 期望 %d", got, exitUsage)
	}
}

func TestGenerateSubcommandJSON(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)
	out := t.TempDir()

	stdout, err := runCommand(t,
		"generate",
		"--config", filepath.Join(t.TempDir(), "absent.toml"),
		"--base-url", srv.URL+"/v1",
		"--model", "test-model",
		"--output-dir", out,
		"--json",
		"一只猫",
	)
	if err != nil {
		t.Fatalf("generate --json 失败: %v", err)
	}
	var got struct {
		Model  string       `json:"model"`
		Images []savedImage `json:"images"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, stdout)
	}
	if got.Model != "test-model" || len(got.Images) != 1 {
		t.Fatalf("结果不正确: %+v", got)
	}
	if got.Images[0].Format != "png" || got.Images[0].Bytes != len(samplePNG) {
		t.Errorf("图片元数据不正确: %+v", got.Images[0])
	}
	if _, err := os.Stat(got.Images[0].Path); err != nil {
		t.Errorf("JSON 中的图片路径不可用: %v", err)
	}
}

func TestGeneratePromptFromStdin(t *testing.T) {
	resetEnv(t)
	var gotBody map[string]any
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	oldReader, oldTTY := stdinReader, stdinTTY
	stdinReader = strings.NewReader("来自 stdin 的提示词\n")
	stdinTTY = func() bool { return false }
	t.Cleanup(func() {
		stdinReader = oldReader
		stdinTTY = oldTTY
	})

	_, err := runCommand(t,
		"generate",
		"--config", filepath.Join(t.TempDir(), "absent.toml"),
		"--base-url", srv.URL+"/v1",
		"--model", "test-model",
		"--output-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("从 stdin 生成失败: %v", err)
	}
	if got, _ := gotBody["prompt"].(string); got != "来自 stdin 的提示词" {
		t.Errorf("prompt = %q, 期望来自 stdin 的内容", got)
	}
}

func TestGlobalFlagsBeforeSubcommand(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"id": "img-a", "mode": "image_generation"},
		}})
	})

	stdout, err := runCommand(t,
		"--config", filepath.Join(t.TempDir(), "absent.toml"),
		"--base-url", srv.URL+"/v1",
		"models", "--json",
	)
	if err != nil {
		t.Fatalf("通用 flag 放在子命令前失败: %v", err)
	}
	if !strings.Contains(stdout, `"img-a"`) {
		t.Errorf("输出不正确:\n%s", stdout)
	}
}

func TestVersionAndHelpCommands(t *testing.T) {
	resetEnv(t)
	for _, args := range [][]string{
		{"version"},
		{"--version"},
		{"help", "version"},
		{"help"},
	} {
		stdout, err := runCommand(t, args...)
		if err != nil {
			t.Errorf("run(%v) 失败: %v", args, err)
			continue
		}
		if !strings.Contains(stdout, "image-gen") && !strings.Contains(stdout, "generate") {
			t.Errorf("run(%v) 输出不完整:\n%s", args, stdout)
		}
	}

	_, err := runCommand(t, "version", "extra")
	if err == nil {
		t.Error("version 带位置参数应失败")
	} else if got := exitCodeOf(err); got != exitUsage {
		t.Errorf("version 退出码 = %d, 期望 %d", got, exitUsage)
	}
}

func TestCompletionCommand(t *testing.T) {
	resetEnv(t)
	stdout, err := runCommand(t, "completion", "bash")
	if err != nil {
		t.Fatalf("completion bash 失败: %v", err)
	}
	if !strings.Contains(stdout, "image-gen") {
		t.Errorf("completion 输出不像 bash 脚本:\n%s", stdout[:min(len(stdout), 500)])
	}
}

func TestInitSubcommandNonInteractive(t *testing.T) {
	resetEnv(t)
	cfgPath := filepath.Join(t.TempDir(), "nested", "config.toml")

	stdout, err := runCommand(t,
		"init",
		"--config", cfgPath,
		"--base-url", "http://gw.example/v1",
		"--model", "test-model",
		"--auth-env", "MY_IMAGE_KEY",
	)
	if err != nil {
		t.Fatalf("init 子命令失败: %v", err)
	}
	data, readErr := os.ReadFile(cfgPath)
	if readErr != nil {
		t.Fatalf("读取生成的配置失败: %v", readErr)
	}
	text := string(data)
	for _, want := range []string{
		`base_url = "http://gw.example/v1"`,
		`model = "test-model"`,
		`env = "MY_IMAGE_KEY"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("配置缺少 %q:\n%s", want, text)
		}
	}
	if !strings.Contains(stdout, "已写入") {
		t.Errorf("init 应确认写入结果:\n%s", stdout)
	}
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		t.Errorf("配置文件不可用: %v", statErr)
	}
	assertSecretMode(t, cfgPath)

	_, err = runCommand(t, "init", "--config", cfgPath, "extra")
	if err == nil || exitCodeOf(err) != exitUsage {
		t.Errorf("init 带位置参数应返回用法错误，实际: %v", err)
	}
}

func TestBarePromptAndLegacyFlagsRejected(t *testing.T) {
	resetEnv(t)
	for _, args := range [][]string{
		{"一只猫"},
		{"--models", "apple"},
		{"--init"},
		{"--base-url", "http://gw.example/v1", "x"},
	} {
		_, err := runCommand(t, args...)
		if err == nil {
			t.Errorf("run(%v) 应被拒绝", args)
			continue
		}
		if got := exitCodeOf(err); got != exitUsage {
			t.Errorf("run(%v) 退出码 = %d, 期望 %d: %v", args, got, exitUsage, err)
		}
	}
}

func TestGenerateDefaultOutputIsCurrentDirectory(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{
			map[string]any{"b64_json": samplePNGB64},
		}})
	}, nil)

	t.Chdir(t.TempDir())
	stdout, err := runCommand(t,
		"generate",
		"--config", filepath.Join(t.TempDir(), "absent.toml"),
		"--base-url", srv.URL+"/v1",
		"--model", "m",
		"x",
	)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	path := strings.TrimSpace(stdout)
	if !filepath.IsAbs(path) {
		t.Fatalf("stdout 应输出绝对路径，实际 %q", path)
	}
	if filepath.Dir(path) != mustGetwd(t) {
		t.Errorf("默认输出目录应为当前目录，实际路径 %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("默认输出文件不可用: %v", err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestQuietAndTTYControlStatus(t *testing.T) {
	oldTTY, oldWriter := stderrTTY, statusWriter
	stderrTTY = func() bool { return true }
	var buf bytes.Buffer
	statusWriter = &buf
	t.Cleanup(func() {
		stderrTTY = oldTTY
		statusWriter = oldWriter
	})

	statusf(&options{}, "working")
	if !strings.Contains(buf.String(), "working") {
		t.Errorf("TTY 下应显示进度，实际 %q", buf.String())
	}

	buf.Reset()
	statusf(&options{quiet: true}, "working")
	if buf.Len() != 0 {
		t.Errorf("--quiet 下不应显示进度，实际 %q", buf.String())
	}
}

func TestStdinIsTerminalRejectsDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", os.DevNull, err)
	}
	defer func() { _ = f.Close() }()
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })
	if stdinIsTerminal() {
		t.Error("/dev/null 不应被当成交互终端")
	}
}

func TestInitNoInputFailsFast(t *testing.T) {
	resetEnv(t)
	_, err := runCommand(t,
		"init",
		"--config", filepath.Join(t.TempDir(), "config.toml"),
		"--no-input",
	)
	if err == nil {
		t.Fatal("--no-input 且无 --base-url 时应失败")
	}
	if got := exitCodeOf(err); got != exitUsage {
		t.Errorf("退出码 = %d, 期望 %d", got, exitUsage)
	}
	if !strings.Contains(err.Error(), "--base-url") {
		t.Errorf("错误应提示 --base-url: %v", err)
	}
}

func TestVersionShorthandVRejected(t *testing.T) {
	resetEnv(t)
	_, err := runCommand(t, "-v")
	if err == nil {
		t.Fatal("-v 不应再表示 version")
	}
	if got := exitCodeOf(err); got != exitUsage {
		t.Errorf("退出码 = %d, 期望 %d", got, exitUsage)
	}
}

func TestGenerateNoArgsTTYShowsHelp(t *testing.T) {
	resetEnv(t)
	oldTTY := stdinTTY
	stdinTTY = func() bool { return true }
	t.Cleanup(func() { stdinTTY = oldTTY })

	stdout, err := runCommand(t, "generate")
	if err != nil {
		t.Fatalf("generate 无参数应显示帮助而不是报错: %v", err)
	}
	if !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "generate") {
		t.Errorf("输出不像 generate 帮助:\n%s", stdout)
	}
}

func TestInvalidTimeoutIsUsageError(t *testing.T) {
	resetEnv(t)
	for _, args := range [][]string{
		{"generate", "--config", filepath.Join(t.TempDir(), "absent.toml"), "--timeout", "0s", "x"},
		{"generate", "--config", filepath.Join(t.TempDir(), "absent.toml"), "--timeout", "300", "x"},
		{"models", "--config", filepath.Join(t.TempDir(), "absent.toml"), "--timeout", "nope"},
	} {
		_, err := runCommand(t, args...)
		if err == nil {
			t.Errorf("run(%v) 应因 timeout 非法而失败", args)
			continue
		}
		if got := exitCodeOf(err); got != exitUsage {
			t.Errorf("run(%v) 退出码 = %d, 期望 %d: %v", args, got, exitUsage, err)
		}
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	resetEnv(t)
	for _, args := range [][]string{
		{"generate", "a", "b"},
		{"generate", "--does-not-exist"},
		{"generate", "--out", "images"},
		{"generate", "--output-format", "png"},
		{"generate", "--n", "2"},
		{"models", "a", "b"},
	} {
		_, err := runCommand(t, args...)
		if err == nil {
			t.Errorf("run(%v) 应失败", args)
			continue
		}
		if got := exitCodeOf(err); got != exitUsage {
			t.Errorf("run(%v) 退出码 = %d, 期望 %d", args, got, exitUsage)
		}
	}
}
