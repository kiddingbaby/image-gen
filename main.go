// Command image-gen generates and edits images through an OpenAI-compatible
// image gateway. It is a single static binary with no runtime dependencies.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

const (
	// OpenAI 兼容接口的固定路径
	generatePath = "/images/generations"
	editPath     = "/images/edits"
	modelsPath   = "/models"
	// 模型列表的输出控制：超过 inline 上限就只给用法，超过 print 上限就截断。
	modelsInlineLimit = 50
	modelsPrintLimit  = 200
	// 整个响应体的上限。图片走 base64 会膨胀约 1.33 倍，
	// n=10 且尺寸较大时总量能到 100MB 以上，所以不能卡在 64MB。
	maxResponseBytes = 192 * 1024 * 1024
	fileModeSecret   = 0o600
	fileModeImage    = 0o644
	promptMaxBytes   = 1 << 20
)

// version 由 -ldflags "-X main.version=..." 注入；初始值必须保持常量，
// 否则 init 阶段的求值会覆盖链接器写入的值。
var version = "dev"

// effectiveVersion 在没有 ldflags（例如 go install）时退回模块版本。
func effectiveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "dev"
}

var configHint = "还没有配置生图网关（base_url）。任选其一：\n" +
	"  1) 环境变量（最快，零配置文件）：\n" +
	"     export IMAGE_GEN_BASE_URL=\"https://你的网关/v1\"\n" +
	"     export IMAGE_GEN_API_KEY=\"你的密钥\"\n" +
	"  2) 交互式向导：image-gen init\n" +
	"  3) 非交互配置（供 agent 或 CI 调用）：\n" +
	"     image-gen init --base-url https://你的网关/v1 --auth-env IMAGE_GEN_API_KEY\n" +
	"配置文件默认位置：" + defaultConfigPath()

var sizeRE = regexp.MustCompile(`^([1-9][0-9]*)x([1-9][0-9]*)$`)

type authConfig struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	Env     string   `toml:"env"`
	Value   string   `toml:"value"`
}

type config struct {
	BaseURL  string     `toml:"base_url"`
	Model    string     `toml:"model"`
	OutDir   string     `toml:"out_dir"`
	Auth     authConfig `toml:"auth"`
	Defaults struct {
		Size    string `toml:"size"`
		Quality string `toml:"quality"`
	} `toml:"defaults"`
}

type options struct {
	jsonlOutput                         bool
	ctx                                 context.Context
	markupPath, markupURL, markupFileID string
	sources                             []imageSource
	maskURL, maskFileID                 string
	imageParameters
	supplied             map[string]bool
	result               *imageResult
	configPath           string
	baseURL              string
	force                bool
	authEnv              string
	authCommand          string
	authCommandArgs      []string
	model                string
	size                 string
	quality              string
	background           string
	outputFormat         string
	outputCompression    int
	outputCompressionSet bool
	moderation           string
	n                    int
	out                  string
	timeout              time.Duration
	jsonOutput           bool
	quiet                bool
	noInput              bool
	prompt               string
	promptArgument       bool
	keyword              string
	imagePaths           []string
	maskPath             string
}

type optionalIntValue struct {
	target *int
	set    *bool
}

func (v *optionalIntValue) String() string { return "" }

func (v *optionalIntValue) Set(value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	*v.target = n
	*v.set = true
	return nil
}

func (v *optionalIntValue) Type() string { return "int" }

var stdinScanner = bufio.NewScanner(os.Stdin)

// stdinReader/stdinTTY 是测试接缝：生成命令在没给提示词时从 stdin 读取。
var stdinReader io.Reader = os.Stdin
var stdinTTY = stdinIsTerminal
var stderrTTY = stderrIsTerminal
var statusWriter io.Writer = os.Stderr

type savedImage struct {
	Index  int    `json:"index"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Path   string `json:"path"`
	Format string `json:"format"`
	Bytes  int    `json:"bytes"`
}

// warnWriter 收集非致命提示（例如网关没采纳 output_format）。
var warnWriter io.Writer = os.Stderr

func warnf(format string, args ...any) {
	// 告警写不出去不该中断生成流程，显式忽略写入错误。
	_, _ = fmt.Fprintf(warnWriter, "image-gen: "+format+"\n", args...)
}

// 退出码让 agent 能区分该改参数、该补配置，还是该重试。
const (
	exitGeneral  = 1
	exitUsage    = 2
	exitConfig   = 3
	exitNetwork  = 4
	exitUpstream = 5
)

type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

func withExit(code int, err error) error {
	if err == nil {
		return nil
	}
	var coded *codedError
	if errors.As(err, &coded) {
		return err // 已有分类时保留更具体的那个
	}
	return &codedError{code: code, err: err}
}

func exitCodeOf(err error) int {
	var coded *codedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return exitGeneral
}

// maxRateLimitRetries 只对 429 重试：请求确定未被处理，重复不会重复计费。
const maxRateLimitRetries = 2

// retryDelay 是第 attempt 次重试前的等待；提成变量是为了让测试不必真等 3 秒。
var retryDelay = func(attempt int) time.Duration {
	return time.Duration(attempt+1)*time.Second + time.Duration(mathrand.IntN(250))*time.Millisecond
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fatal(err)
	}
}

// run 用 Cobra 执行命令树；单独抽出来便于测试。
func run(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	o := &options{ctx: ctx}
	root := rootCommand(o)
	root.SetArgs(args)
	cmd, err := root.ExecuteC()
	if cmd != nil && (cmd.Name() == "generate" || cmd.Name() == "edit") && (err != nil || o.result != nil) {
		return finishImage(o, cmd.Name(), err)
	}
	return err
}

func rootCommand(o *options) *cobra.Command {
	root := &cobra.Command{
		Use:           "image-gen",
		Short:         "通过 OpenAI 兼容网关生成或编辑图片",
		Long:          rootLong,
		Example:       rootExamples,
		Version:       effectiveVersion(),
		Args:          rootArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("image-gen {{.Version}}\n")
	// 不用 Cobra 默认的 -v 简写：-v 在 CLI 里常被理解为 verbose，
	// 版本只保留语义明确的 --version。
	root.Flags().Bool("version", false, "打印版本")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return withExit(exitUsage, err)
	})

	root.PersistentFlags().StringVar(&o.configPath, "config", defaultConfigPath(), "配置文件路径")
	root.PersistentFlags().StringVar(&o.baseURL, "base-url", "", "生图网关地址")
	root.PersistentFlags().DurationVar(&o.timeout, "timeout", 300*time.Second, "单次请求超时（如 30s、5m）")
	root.PersistentFlags().BoolVarP(&o.quiet, "quiet", "q", false, "只输出必要结果，不显示进度提示")
	_ = root.MarkPersistentFlagFilename("config", "toml")

	root.AddCommand(
		newGenerateCmd(o),
		newEditCmd(o),
		newModelsCmd(o),
		newInitCmd(o),
		newVersionCmd(),
	)
	return root
}

const rootLong = `通过兼容 OpenAI images 接口的网关生成或编辑图片。

提示词可从位置参数或 stdin 读取：
  image-gen generate "一只橘猫"
  echo "一只橘猫" | image-gen generate
  image-gen edit "把背景换成雨夜街道" --image original.png

配置优先级：命令行参数 > 环境变量 > config.toml。
退出码：0 成功；2 参数/用法错误；3 配置或密钥缺失；
        4 网络错误；5 上游返回错误；130 用户取消；1 本地运行错误。`

const rootExamples = `  image-gen generate "一只在纸上洇开的墨点"
  echo "一只橘猫" | image-gen generate --model <模型>
  image-gen models --json
  image-gen init --base-url https://你的网关/v1 --auth-env IMAGE_GEN_API_KEY`

func newGenerateCmd(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "generate [提示词]",
		Short:         "生成图片",
		Long:          "生成图片并落盘。提示词可写在位置参数里，也可从 stdin 读取；`-` 也显式表示 stdin。",
		Example:       "  image-gen generate \"一只橘猫\"\n  echo \"一只橘猫\" | image-gen generate --output-dir ./images\n  image-gen generate --json \"一只橘猫\"",
		Args:          maxArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
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
			return cmdGenerate(o, cfg)
		},
	}
	addGenerateFlags(cmd.Flags(), o)
	cmd.Flags().Lookup("quality").Usage += "；旧模型另支持 standard/hd"
	cmd.Flags().StringVar(&o.responseFormat, "response-format", "", "旧模型兼容：url/b64_json")
	cmd.Flags().StringVar(&o.style, "style", "", "旧模型兼容：vivid/natural")
	completeImageFlags(cmd)
	return cmd
}

func completeImageFlags(cmd *cobra.Command) {
	_ = cmd.MarkFlagDirname("output-dir")
	for flag, choices := range map[string][]string{
		"size":       {"auto", "1024x1024", "1024x1536", "1536x1024"},
		"quality":    {"low", "medium", "high", "xhigh", "max", "auto"},
		"format":     {"png", "jpeg", "webp", "gif", "svg"},
		"background": {"transparent", "opaque", "auto"},
		"moderation": {"auto", "low"},
	} {
		_ = cmd.RegisterFlagCompletionFunc(flag, cobra.FixedCompletions(choices, cobra.ShellCompDirectiveNoFileComp))
	}
}

func newModelsCmd(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "models [关键词]",
		Short:         "列出网关可用的图像模型",
		Long:          "列出网关通过 /v1/models 暴露的图像模型。给出关键词时按模型名过滤；--json 输出机器可读结果。",
		Example:       "  image-gen models\n  image-gen models gpt-image\n  image-gen models --json",
		Args:          maxArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.keyword = args[0]
			}
			cfg, err := loadConfig(o.configPath)
			if err != nil {
				return withExit(exitConfig, err)
			}
			return cmdModels(o, cfg)
		},
	}
	cmd.Flags().BoolVar(&o.jsonOutput, "json", false, "以 JSON 输出结果")
	return cmd
}

func newInitCmd(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "init",
		Short:         "写入 config.toml",
		Long:          "生成 image-gen 配置文件。无参数时进入交互向导；给出 --base-url 后改为非交互写入。",
		Example:       "  image-gen init\n  image-gen init --base-url https://你的网关/v1 --auth-env IMAGE_GEN_API_KEY",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cmdInit(o); err != nil {
				return withExit(exitConfig, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&o.model, "model", "m", "", "默认模型")
	cmd.Flags().BoolVar(&o.force, "force", false, "覆盖已存在的配置文件")
	cmd.Flags().StringVar(&o.authEnv, "auth-env", "", "非交互：密钥从该环境变量读取")
	cmd.Flags().StringVar(&o.authCommand, "auth-command", "", "非交互：取密钥的可执行文件（参数用 --auth-command-arg 重复传入）")
	cmd.Flags().StringArrayVar(&o.authCommandArgs, "auth-command-arg", nil, "非交互：--auth-command 的参数，可重复")
	cmd.Flags().BoolVar(&o.noInput, "no-input", false, "不进行任何交互提示")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "version",
		Short:         "打印版本",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("image-gen %s\n", effectiveVersion())
		},
	}
}

func addGenerateFlags(f *pflag.FlagSet, o *options) {
	addImageParameterFlags(f, o)
	f.StringVarP(&o.model, "model", "m", "", "图像模型名")
	f.StringVar(&o.size, "size", "", "尺寸：auto 或 WIDTHxHEIGHT（默认省略，采用上游 auto）")
	f.StringVar(&o.quality, "quality", "", "质量：low/medium/high/xhigh/max/auto（默认省略，采用上游 auto）")
	f.StringVar(&o.background, "background", "", "背景：transparent/opaque/auto（默认不发送）")
	f.StringVar(&o.outputFormat, "format", "", "输出格式：png/jpeg/webp/gif/svg")
	f.Var(&optionalIntValue{
		target: &o.outputCompression,
		set:    &o.outputCompressionSet,
	}, "output-compression", "压缩 0-100（仅 jpeg/webp，默认不发送）")
	f.StringVar(&o.moderation, "moderation", "", "审核：auto/low（默认不发送）")
	f.IntVarP(&o.n, "count", "n", 1, "生成张数 1-10")
	f.StringVarP(&o.out, "output-dir", "o", "", "输出目录（默认当前目录）")
	f.BoolVar(&o.jsonOutput, "json", false, "以单个 JSON 对象输出结果和错误")
	f.BoolVar(&o.jsonlOutput, "jsonl", false, "实时 JSONL 事件，要求 --stream=true，与 --json 互斥")
}

func maxArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > n {
			return withExit(exitUsage, fmt.Errorf("%s 最多接受 %d 个位置参数，收到 %d 个", cmd.CommandPath(), n, len(args)))
		}
		return nil
	}
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return withExit(exitUsage, fmt.Errorf("%s 不接受位置参数，收到 %d 个", cmd.CommandPath(), len(args)))
	}
	return nil
}

func rootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("未知命令 %q；运行 image-gen --help 查看可用命令", args[0])
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		msg += "；你是指 " + strings.Join(suggestions, "、") + "？"
	}
	return withExit(exitUsage, errors.New(msg))
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "image-gen: %v\n", err)
	os.Exit(exitCodeOf(err))
}

// defaultConfigPath 按平台惯例定位用户配置：Linux 走 XDG，macOS 走
// Application Support，Windows 走 %AppData%；--config 与 IMAGE_GEN_CONFIG 优先。
func defaultConfigPath() string {
	if v := os.Getenv("IMAGE_GEN_CONFIG"); v != "" {
		return v
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "image-gen", "config.toml")
	}
	return "config.toml"
}

func loadConfig(path string) (config, error) {
	var c config
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, fmt.Errorf("读取配置文件 %s 失败：%w", path, err)
	}
	if err := toml.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("解析配置文件 %s 失败：%w", path, err)
	}
	return c, nil
}

// timeoutOf 校验 --timeout；所有网络请求共用这一个值。
// 非正数会让 http.Client 关闭超时，必须在发请求前拒绝。
func timeoutOf(o *options) (time.Duration, error) {
	if o.timeout <= 0 {
		return 0, errors.New("--timeout 必须大于 0，例如 30s、5m")
	}
	return o.timeout, nil
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// target 是一次请求所需的连接信息。优先级规则只在这里实现一次：
// 命令行 > 环境变量 > config.toml。
type target struct {
	baseURL string
	key     string
}

// httpPayload 支持 JSON 和文件上的 multipart。每次请求使用独立的读取位置，
// 429 重试可以完整重传，而不用把所有输入图片放进内存。
type httpPayload struct {
	reader      io.ReaderAt
	size        int64
	contentType string
}

func resolveTarget(o *options, cfg config) (target, error) {
	baseURL := baseURLOf(o, cfg)
	if baseURL == "" {
		return target{}, withExit(exitConfig, errors.New(configHint))
	}
	key, err := resolveAuth(cfg.Auth)
	if err != nil {
		return target{}, withExit(exitConfig, err)
	}
	return target{baseURL: baseURL, key: key}, nil
}

// request 发出一次 HTTP 请求，统一处理鉴权头与超时。
func (t target) request(path string, payload []byte, timeout time.Duration) (int, []byte, error) {
	var body *httpPayload
	if payload != nil {
		body = &httpPayload{bytes.NewReader(payload), int64(len(payload)), "application/json"}
	}
	return t.requestPayload(path, body, timeout)
}

func (t target) requestPayload(path string, payload *httpPayload, timeout time.Duration) (int, []byte, error) {
	authName, authValue := authHeader(t.key)
	return httpCallWithRetry(endpoint(t.baseURL, path), authName, authValue, payload, timeout)
}

// authHeader 是 OpenAI 兼容接口的鉴权方式；接原生厂商 API 时在这里分支。
func authHeader(key string) (string, string) { return "Authorization", "Bearer " + key }

// baseURLOf 按「命令行 > 环境变量 > config.toml」解析网关地址。
func baseURLOf(o *options, cfg config) string {
	return first(o.baseURL, os.Getenv("IMAGE_GEN_BASE_URL"), cfg.BaseURL)
}

func resolveAuth(a authConfig) (string, error) {
	if k := strings.TrimSpace(os.Getenv("IMAGE_GEN_API_KEY")); k != "" {
		return k, nil
	}
	if a.Command != "" {
		var stderr bytes.Buffer
		cmd := exec.Command(a.Command, a.Args...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			detail := strings.TrimSpace(stderr.String())
			if detail != "" {
				return "", fmt.Errorf("执行密钥命令失败：%w（%s）", err, detail)
			}
			return "", fmt.Errorf("执行密钥命令失败：%w", err)
		}
		text := strings.TrimSpace(string(out))
		if text == "" {
			return "", fmt.Errorf("密钥命令 %s 返回为空", a.Command)
		}
		key := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
		if key == "" {
			return "", fmt.Errorf("密钥命令 %s 返回为空", a.Command)
		}
		return key, nil
	}
	if a.Env != "" {
		if k := strings.TrimSpace(os.Getenv(a.Env)); k != "" {
			return k, nil
		}
	}
	if k := strings.TrimSpace(a.Value); k != "" {
		return k, nil
	}
	return "", errors.New("未配置密钥。任选其一：\n" +
		"  1) 环境变量：export IMAGE_GEN_API_KEY=<key>\n" +
		"  2) 配置文件 config.toml 的 [auth] 段（command+args / env / value）\n" +
		"  3) 交互式向导：image-gen init")
}

func endpoint(base, path string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + path
	}
	return base + "/v1" + path
}

func httpCall(rawURL, authName, authValue string, payload *httpPayload, timeout time.Duration) (int, []byte, error) {
	var body io.Reader
	method := http.MethodGet
	if payload != nil {
		body = io.NewSectionReader(payload.reader, 0, payload.size)
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set(authName, authValue)
	if payload != nil {
		req.Header.Set("Content-Type", payload.contentType)
		req.ContentLength = payload.size
		// 307/308 重定向需要重新读取请求体；与每次 429 重试一样使用独立游标。
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(payload.reader, 0, payload.size)), nil
		}
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("网络错误：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("读取响应失败：%w", err)
	}
	if len(data) > maxResponseBytes {
		return resp.StatusCode, nil, fmt.Errorf("响应体超过 %d 字节，已拒绝", maxResponseBytes)
	}
	return resp.StatusCode, data, nil
}

// httpCallWithRetry 只对 429 重试。生成接口是非幂等的：一旦超时或 5xx，
// 上游可能已经出图并计费，自动重试会重复扣费，所以那些情况直接报错。
func httpCallWithRetry(rawURL, authName, authValue string, payload *httpPayload, timeout time.Duration) (int, []byte, error) {
	var status int
	var data []byte
	var err error
	for attempt := 0; attempt <= maxRateLimitRetries; attempt++ {
		status, data, err = httpCall(rawURL, authName, authValue, payload, timeout)
		if err != nil || status != http.StatusTooManyRequests || attempt == maxRateLimitRetries {
			return status, data, err
		}
		time.Sleep(retryDelay(attempt))
	}
	return status, data, err
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func validateModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("未指定模型：用 --model <名字>，或先跑 image-gen models 看网关可用模型，再把 model 写进 config.toml")
	}
	if strings.ContainsAny(model, " \t\n") {
		return "", errors.New("--model 不能包含空白字符")
	}
	return model, nil
}

func normalizeOutputFormat(format string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png":
		return "png", nil
	case "jpeg":
		return "jpeg", nil
	case "jpg":
		return "jpeg", nil
	case "webp", "gif", "svg":
		return strings.ToLower(strings.TrimSpace(format)), nil
	default:
		return "", errors.New("--format 必须是 png、jpeg、jpg、webp、gif 或 svg")
	}
}

func validateSize(size string) (string, error) {
	size = strings.TrimSpace(size)
	if size == "auto" {
		return size, nil
	}
	m := sizeRE.FindStringSubmatch(size)
	if m == nil {
		return "", errors.New("--size 必须是 auto 或 WIDTHxHEIGHT，例如 1024x1024")
	}
	w, errW := strconv.Atoi(m[1])
	h, errH := strconv.Atoi(m[2])
	if errW != nil || errH != nil {
		return "", errors.New("--size 数值过大")
	}
	if w <= 0 || h <= 0 {
		return "", errors.New("--size 宽高必须为正整数")
	}
	return size, nil
}

func validateQuality(quality string) (string, error) {
	quality = strings.TrimSpace(quality)
	switch quality {
	case "low", "medium", "high", "xhigh", "max", "auto", "standard", "hd":
		return quality, nil
	default:
		return "", errors.New("--quality 必须是 low、medium、high、xhigh、max 或 auto")
	}
}

func validateBackground(background string) (string, error) {
	background = strings.TrimSpace(background)
	switch background {
	case "", "transparent", "opaque", "auto":
		return background, nil
	default:
		return "", errors.New("--background 必须是 transparent、opaque 或 auto")
	}
}

func validateModeration(moderation string) (string, error) {
	moderation = strings.TrimSpace(moderation)
	switch moderation {
	case "", "auto", "low":
		return moderation, nil
	default:
		return "", errors.New("--moderation 必须是 auto 或 low")
	}
}

// sniffImageExt 按 magic bytes 判断真实图片格式，认不出时返回空串。
func sniffImageExt(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	case len(b) >= 4 && string(b[:4]) == "GIF8":
		return "gif"
	}
	if isSVG(b) {
		return "svg"
	}
	return ""
}

type modelsResult struct {
	CapabilityDeclared bool     `json:"capability_declared"`
	Models             []string `json:"models"`
}

func cmdModels(o *options, cfg config) error {
	timeout, err := timeoutOf(o)
	if err != nil {
		return withExit(exitUsage, err)
	}
	t, err := resolveTarget(o, cfg)
	if err != nil {
		return err
	}
	statusf(o, "正在读取模型列表……")
	status, body, err := t.request(modelsPath, nil, timeout)
	if err != nil {
		return withExit(exitNetwork, err)
	}
	if status >= 400 {
		return withExit(exitUpstream, fmt.Errorf("列出模型失败（HTTP %d）：%s", status, truncate(string(body), 500)))
	}
	var resp struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return withExit(exitUpstream, errors.New("模型列表响应格式无效"))
	}
	listing, confident := pickImageModels(resp.Data)

	// models 命令的位置参数是关键词，不是 prompt。网关即使声明了
	// 生图能力，也仍要应用关键词过滤，否则 `models grok` 会静默列出全部模型。
	keyword := strings.TrimSpace(o.keyword)
	matched := listing
	if keyword != "" {
		matched = filterModels(listing, keyword)
		if len(matched) == 0 {
			return withExit(exitUsage, fmt.Errorf("没有名字包含 %q 的模型（共 %d 个）", keyword, len(listing)))
		}
	}

	if o.jsonOutput {
		if matched == nil {
			matched = []string{}
		}
		data, err := json.Marshal(modelsResult{CapabilityDeclared: confident, Models: matched})
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if keyword != "" {
		if confident {
			fmt.Printf("名字包含 %q 的图像模型（%d 个）：\n", keyword, len(matched))
		} else {
			fmt.Printf("名字包含 %q 的模型（%d 个，仅按名称匹配，不代表都能生图）：\n", keyword, len(matched))
		}
		printModels(matched)
		return nil
	}

	// 网关声明了能力：直接给答案。
	if confident {
		fmt.Printf("可用图像模型（%d 个）：\n", len(listing))
		printModels(listing)
		return nil
	}

	// 没有能力字段时只能按名字过滤；模型不多就全列出来，上千个全列没有可读性，给用法。
	if len(listing) <= modelsInlineLimit {
		fmt.Printf("网关可用模型（%d 个，未声明能力）：\n", len(listing))
		printModels(listing)
		return nil
	}
	fmt.Printf("网关没有声明模型能力，共 %d 个模型。\n", len(listing))
	fmt.Println("用 `image-gen models <关键词>` 按名字过滤，例如：")
	fmt.Println("  image-gen models image")
	return nil
}

func printModels(ids []string) {
	if len(ids) > modelsPrintLimit {
		for _, id := range ids[:modelsPrintLimit] {
			fmt.Printf("  - %s\n", id)
		}
		fmt.Printf("  ……还有 %d 个，缩小关键词再看\n", len(ids)-modelsPrintLimit)
		return
	}
	for _, id := range ids {
		fmt.Printf("  - %s\n", id)
	}
}

func filterModels(ids []string, keyword string) []string {
	kw := strings.ToLower(keyword)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.Contains(strings.ToLower(id), kw) {
			out = append(out, id)
		}
	}
	return out
}

func readPrompt(o *options) error {
	if o.promptArgument && o.prompt != "-" {
		return nil
	}
	if p := strings.TrimSpace(o.prompt); p != "" && p != "-" {
		return nil
	}
	if o.prompt != "-" && stdinTTY() {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(stdinReader, promptMaxBytes+1))
	if err != nil {
		return fmt.Errorf("读取标准输入失败：%w", err)
	}
	if len(data) > promptMaxBytes {
		return fmt.Errorf("标准输入超过 %d 字节", promptMaxBytes)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return errors.New("标准输入里没有提示词")
	}
	o.prompt = prompt
	return nil
}

func cmdGenerate(o *options, cfg config) error {
	return cmdImage(o, cfg, false)
}

func cmdImage(o *options, cfg config, editing bool) error {
	operationName := "generate"
	if editing {
		operationName = "edit"
	}
	o.result = newImageResult(operationName)
	o.result.jsonl = o.jsonlOutput
	if o.outputFormat != "" {
		normalized, err := normalizeOutputFormat(o.outputFormat)
		if err != nil {
			return withExit(exitUsage, err)
		}
		o.outputFormat = normalized
	}
	o.quality = strings.TrimSpace(o.quality)
	if err := validateImageParameters(o, editing); err != nil {
		return withExit(exitUsage, err)
	}
	// 先校验参数再解析配置：参数写错时给出明确提示，别被「没配网关」盖掉。
	if editing && (len(o.sources) == 0 || len(o.sources) > 16) {
		return withExit(exitUsage, errors.New("edit 必须通过 --image、--image-url 或 --image-file-id 提供 1–16 张图片"))
	}
	sizeValue := first(o.size, cfg.Defaults.Size)
	qualityValue := first(o.quality, cfg.Defaults.Quality)
	size, err := validateSize(first(sizeValue, "auto"))
	if err != nil {
		return withExit(exitUsage, err)
	}
	quality, err := validateQuality(first(qualityValue, "auto"))
	if editing && (quality == "standard" || quality == "hd") {
		return withExit(exitUsage, errors.New("edit 的 --quality 不支持 standard/hd"))
	}
	if err != nil {
		return withExit(exitUsage, err)
	}
	background, err := validateBackground(o.background)
	if err != nil {
		return withExit(exitUsage, err)
	}
	moderation, err := validateModeration(o.moderation)
	if err != nil {
		return withExit(exitUsage, err)
	}
	format, err := normalizeOutputFormat(first(o.outputFormat, "png"))
	if err != nil {
		return withExit(exitUsage, err)
	}
	if background == "transparent" && format != "png" && format != "webp" {
		return withExit(exitUsage, errors.New("透明背景要求 --format 为 png 或 webp"))
	}
	if o.n < 1 || o.n > 10 {
		return withExit(exitUsage, errors.New("--count 必须在 1 到 10 之间"))
	}
	// 官方定义：output_compression 只对 webp / jpeg 生效。
	if o.outputCompressionSet {
		if o.outputCompression < 0 || o.outputCompression > 100 {
			return withExit(exitUsage, errors.New("--output-compression 必须在 0 到 100 之间"))
		}
		if format != "jpeg" && format != "webp" {
			return withExit(exitUsage, fmt.Errorf(
				"--output-compression 只对 jpeg/webp 生效，当前 --format 是 %s", format))
		}
	}
	_, err = timeoutOf(o)
	if err != nil {
		return withExit(exitUsage, err)
	}
	if err := readPrompt(o); err != nil {
		return withExit(exitUsage, err)
	}
	prompt := strings.TrimSpace(o.prompt)
	if !utf8.ValidString(prompt) || utf8.RuneCountInString(prompt) > 32000 {
		return withExit(exitUsage, errors.New("提示词必须是合法 UTF-8，且不超过 32000 个字符"))
	}
	if prompt == "" {
		return withExit(exitUsage, errors.New("提示词不能为空；可以写参数，也可以用 stdin 管道传入"))
	}
	// 先确认网关配置：缺 base_url 属于配置问题（退出码 3），不能被「缺 --model」（2）盖掉，
	// 否则首次使用的用户会按 2 的提示去跑同样需要网关的 `image-gen models`。
	if baseURLOf(o, cfg) == "" {
		return withExit(exitConfig, errors.New(configHint))
	}
	model := first(o.model, os.Getenv("IMAGE_GEN_MODEL"), cfg.Model)
	if model != "" || !editing {
		model, err = validateModel(model)
	}
	if err != nil {
		return withExit(exitUsage, err)
	}
	if model != "" {
		o.result.Model = &model
	}
	t, err := resolveTarget(o, cfg)
	if err != nil {
		return imageError(exitConfig, "auth_resolution_failed", "config", err)
	}

	if sizeValue == "" {
		size = ""
	}
	if qualityValue == "" {
		quality = ""
	}
	if model != "" {
		o.result.Model = &model
	}
	outDir, err := prepareOutputDir(first(o.out, os.Getenv("IMAGE_GEN_OUT_DIR"), cfg.OutDir))
	if err != nil {
		return imageError(exitGeneral, "save_failed", "save", err)
	}
	fields := buildPayload(o, model, prompt, size, quality, format, background, moderation)
	operation, path := "生成", generatePath
	var payload *httpPayload
	if editing {
		operation, path = "编辑", editPath
		var cleanup func()
		payload, cleanup, err = editPayload(o, fields)
		if err != nil {
			return err
		}
		defer cleanup()
	} else {
		body, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		payload = &httpPayload{bytes.NewReader(body), int64(len(body)), "application/json"}
	}
	statusf(o, "正在%s图片（模型 %s）……", operation, model)
	if err := o.context().Err(); err != nil {
		return imageNetworkError(err)
	}
	o.result.generationStatus = "unknown"
	response, err := t.imageResponse(path, payload, o)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	o.result.httpStatus = &response.StatusCode
	if id := response.Header.Get("X-Request-ID"); id != "" {
		o.result.RequestID = &id
	}
	if response.StatusCode < 400 && strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return o.result.consumeStream(response.Body, o, outDir)
	}
	body, err := readImageResponse(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 400 {
		err := o.result.upstreamError(response.StatusCode, body)
		if response.StatusCode == 429 && !quotaError(body) {
			var failure *imageFailure
			if errors.As(err, &failure) {
				failure.Retry.Scope = "request"
				failure.Retry.AfterSeconds = retryAfter(response.Header.Get("Retry-After"))
			}
		}
		return err
	}

	return o.result.consumeJSON(body, o, outDir)
}

// modelEntry 是 /v1/models 单条的最小结构：OpenAI 只保证 id，
// LiteLLM 之类的网关会额外给 mode。
type modelEntry struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

// pickImageModels 优先相信网关自报的 mode 字段；没有能力信息时退回全量列表，
// 交给调用方判断——靠名字猜生图模型误报率太高（读图模型名字里常有 image）。
func pickImageModels(entries []modelEntry) ([]string, bool) {
	var all, imgs []string
	declared := false
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		all = append(all, id)
		if strings.TrimSpace(e.Mode) != "" {
			declared = true
		}
		if e.Mode == "image_generation" {
			imgs = append(imgs, id)
		}
	}
	if declared && len(imgs) > 0 {
		sort.Strings(imgs)
		return uniqueSorted(imgs), true
	}
	sort.Strings(all)
	return uniqueSorted(all), false
}

// buildPayload 按官方字段名组装请求体；可选字段只在显式设置时发送，
// 免得老网关因为不认识的键直接 400。
func buildPayload(o *options, model, prompt, size, quality, format, background, moderation string) map[string]any {
	return requestFields(o, model, prompt, size, quality, format, background, moderation)
}

func cmdInit(o *options) error {
	cfgPath := o.configPath
	if o.baseURL != "" || o.authEnv != "" || o.authCommand != "" || len(o.authCommandArgs) > 0 {
		return cmdInitArgs(o, cfgPath)
	}
	if o.noInput {
		return withExit(exitUsage, errors.New("--no-input 已禁用交互，需同时给出 --base-url（以及 --auth-env 或 --auth-command）"))
	}
	if !stdinIsTerminal() {
		return withExit(exitUsage, errors.New("当前不是交互终端，配置向导无法提问。改用非交互写法：\n"+
			"  image-gen init --base-url https://你的网关/v1 --auth-env IMAGE_GEN_API_KEY\n"+
			"或跳过配置文件，直接 export IMAGE_GEN_BASE_URL 与 IMAGE_GEN_API_KEY"))
	}
	fmt.Println("image-gen: 交互式首次配置")
	fmt.Printf("配置将写入：%s\n", cfgPath)
	if _, err := os.Stat(cfgPath); err == nil && !o.force {
		answer, askErr := ask("该文件已存在，是否覆盖？[y/N] ", "")
		if askErr != nil {
			fmt.Println("已取消，未做任何修改。")
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		default:
			fmt.Println("已取消，未做任何修改。")
			return nil
		}
	}

	baseURL, err := ask(
		"生图网关地址 base_url（如 https://你的网关/v1）：",
		os.Getenv("IMAGE_GEN_BASE_URL"),
	)
	if err != nil {
		return err
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return errors.New("base_url 不能为空，已取消")
	}

	model, err := ask(
		"默认模型 model（可留空，生成时再指定）：",
		os.Getenv("IMAGE_GEN_MODEL"),
	)
	if err != nil {
		return err
	}
	model = strings.TrimSpace(model)

	fmt.Println("密钥方式（写入 [auth] 段）：")
	fmt.Println("  1) 环境变量（推荐，密钥不落盘）")
	fmt.Println("  2) 明文写入 config.toml（自动 chmod 600）")
	fmt.Println("  3) 命令获取（如 pass / op）")
	fmt.Println("  4) 暂不配置（运行时用 IMAGE_GEN_API_KEY）")
	choice, err := ask("请选择：", "1")
	if err != nil {
		return err
	}
	choice = strings.TrimSpace(choice)
	if choice == "" {
		choice = "1"
	}

	var authLines []string
	envName := ""
	switch choice {
	case "1":
		envName, err = ask("环境变量名：", "IMAGE_GEN_API_KEY")
		if err != nil {
			return err
		}
		envName = strings.TrimSpace(envName)
		if envName == "" {
			envName = "IMAGE_GEN_API_KEY"
		}
		authLines = []string{"[auth]", "env = " + tomlString(envName)}
	case "2":
		value, askErr := ask("密钥 value：", "")
		if askErr != nil {
			return askErr
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("value 不能为空，已取消")
		}
		authLines = []string{"[auth]", "value = " + tomlString(value)}
	case "3":
		command, askErr := ask("取密钥命令（如 pass / op）：", "")
		if askErr != nil {
			return askErr
		}
		command = strings.TrimSpace(command)
		if command == "" {
			return errors.New("command 不能为空，已取消")
		}
		rawArgs, askErr := ask("命令参数，空格分隔（可留空）：", "")
		if askErr != nil {
			return askErr
		}
		extraArgs := strings.Fields(rawArgs)
		authLines = []string{
			"[auth]",
			"command = " + tomlString(command),
			"args = " + tomlArray(extraArgs),
		}
	case "4":
	default:
		return fmt.Errorf("无效选择：%q", choice)
	}

	if err := writeConfigFile(cfgPath, baseURL, model, authLines, true); err != nil {
		return err
	}
	if model == "" {
		fmt.Println("还没设默认模型：跑 `image-gen models` 看可用模型，再用 --model 指定，或在 config.toml 写 model = \"...\"。")
	}
	switch choice {
	case "1":
		fmt.Printf("\n已写入 %s（权限 600）。\n下一步导出密钥：export %s=\"...\"\n", cfgPath, envName)
	case "4":
		fmt.Printf("\n已写入 %s（权限 600）。\n下一步导出密钥：export IMAGE_GEN_API_KEY=\"...\"\n", cfgPath)
	default:
		fmt.Printf("\n已写入 %s（权限 600）。\n", cfgPath)
	}
	fmt.Println("验证：image-gen models")
	return nil
}

// cmdInitArgs 写入非交互配置，不读 stdin，供 agent、CI 与脚本调用。
func cmdInitArgs(o *options, cfgPath string) error {
	baseURL := strings.TrimSpace(o.baseURL)
	if baseURL == "" {
		return withExit(exitUsage, errors.New("非交互 init 需要 --base-url"))
	}
	if len(o.authCommandArgs) > 0 && o.authCommand == "" {
		return withExit(exitUsage, errors.New("--auth-command-arg 需要同时给出 --auth-command"))
	}
	var authLines []string
	switch {
	case o.authCommand != "":
		authLines = []string{"[auth]", "command = " + tomlString(o.authCommand)}
		if len(o.authCommandArgs) > 0 {
			authLines = append(authLines, "args = "+tomlArray(o.authCommandArgs))
		}
	case o.authEnv != "":
		authLines = []string{"[auth]", "env = " + tomlString(o.authEnv)}
	}
	model := strings.TrimSpace(o.model)
	if err := writeConfigFile(cfgPath, baseURL, model, authLines, o.force); err != nil {
		return err
	}
	fmt.Printf("已写入 %s（权限 600）。\n", cfgPath)
	if o.authEnv != "" {
		fmt.Printf("下一步导出密钥：export %s=\"...\"\n", o.authEnv)
	}
	if model == "" {
		fmt.Println("还没设默认模型：跑 `image-gen models` 看可用模型，再用 --model 指定，或在 config.toml 写 model = \"...\"。")
	}
	return nil
}

// writeConfigFile 原子写入 config.toml；已存在且未要求覆盖时报错，不静默改写。
func writeConfigFile(cfgPath, baseURL, model string, authLines []string, force bool) error {
	if _, err := os.Stat(cfgPath); err == nil && !force {
		return fmt.Errorf("%s 已存在；加 --force 覆盖", cfgPath)
	}
	lines := []string{
		"# image-gen 配置（由 `image-gen init` 生成）",
		"# 三级覆盖：命令行参数 > 环境变量 > 本文件。",
		"",
		"base_url = " + tomlString(baseURL),
	}
	if model != "" {
		lines = append(lines, "model = "+tomlString(model))
	}
	if len(authLines) > 0 {
		lines = append(lines, "")
		lines = append(lines, authLines...)
	}
	text := strings.Join(lines, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	if err := atomicWrite(cfgPath, []byte(text), fileModeSecret); err != nil {
		return fmt.Errorf("写入配置文件 %s 失败：%w", cfgPath, err)
	}
	return nil
}

// stdinIsTerminal 报告 stdin 是否为交互终端；/dev/null 不算。
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func stderrIsTerminal() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// statusf 只在 stderr 是终端且未加 --quiet 时显示进度。
func statusf(o *options, format string, args ...any) {
	if o.quiet || !stderrTTY() {
		return
	}
	_, _ = fmt.Fprintf(statusWriter, "image-gen: "+format+"\n", args...)
}

func ask(prompt, defaultValue string) (string, error) {
	suffix := ""
	if defaultValue != "" {
		suffix = fmt.Sprintf(" [%s]", defaultValue)
	}
	fmt.Printf("%s%s ", prompt, suffix)
	if !stdinScanner.Scan() {
		if err := stdinScanner.Err(); err != nil {
			return "", err
		}
		if defaultValue != "" {
			return defaultValue, nil
		}
		return "", errors.New("未检测到交互输入。可用环境变量或手动写 config.toml")
	}
	value := strings.TrimSpace(stdinScanner.Text())
	if value == "" {
		value = defaultValue
	}
	return value, nil
}

func tomlString(value string) string {
	if value == "" {
		return "\"\""
	}
	if !strings.ContainsAny(value, "\"\\\n\r\t\x00") {
		return "\"" + value + "\""
	}
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	)
	return "\"" + replacer.Replace(value) + "\""
}

func tomlArray(items []string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = tomlString(item)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func lastSegment(value string) string {
	if i := strings.LastIndex(value, "/"); i >= 0 {
		return value[i+1:]
	}
	return value
}

// safeFileStem 只保留文件名安全字符，避免模型名里的路径分隔符或控制字符
// 影响落盘位置。模型 ID 通常已是 ASCII，这里不追求可读性，只求确定与安全。
func safeFileStem(value string) string {
	value = lastSegment(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	stem := strings.Trim(b.String(), "._-")
	if stem == "" {
		return "image"
	}
	if len(stem) > 80 {
		stem = stem[:80]
	}
	return stem
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%012d", time.Now().UnixNano()%1_000_000_000_000)
	}
	return hex.EncodeToString(buf)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	// 按字节截断可能切在多字节字符中间，丢掉残缺尾巴。
	return strings.ToValidUTF8(value[:limit], "")
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:0]
	for i, value := range values {
		if i == 0 || value != values[i-1] {
			out = append(out, value)
		}
	}
	return out
}

func captureImageFlags(cmd *cobra.Command, o *options) {
	o.supplied = map[string]bool{}
	cmd.Flags().Visit(func(f *pflag.Flag) { o.supplied[f.Name] = true })
}

func (o *options) context() context.Context {
	if o.ctx != nil {
		return o.ctx
	}
	return context.Background()
}
