# Images API 兼容接口实现验收

日期：2026-11-09。实现依据：[规格](../specs/openai-images-compatibility.md)。版本：CLI 0.12.0 / image-gen skill 0.8.0。

## 设计审查

设计可实施，未发现需要改换协议或入口的阻塞问题。审查补入两处限定：markup 远程地址仅 HTTPS；取消前后按是否发出请求、是否收齐最终图判断 generation_status。官方页面冲突继续显式保留，不把模拟网关接受请求当作真实服务可用证明。

## 验收证据

测试观察公共 CLI、HTTP 请求/响应、真实文件和子进程退出码。实现采用逐轮红→绿：先观察请求仍强制带旧默认值、参数不存在、响应字段丢失、429 额度错误重发、流事件无法解析、取消无终态等预期失败，再完成对应实现。

| 验收项 | 测试证据 |
| --- | --- |
| 可选字段、默认值与配置覆盖 | TestOptionalDefaultsOnWire |
| 官方参数全集、嵌套对象与格式 | TestOfficialImageParameters、TestEditReferenceEncodingAndOrder、TestMarkupAndReferenceValidation、已有编辑参数测试 |
| 通用组合及非法输入在 POST 前拒绝 | TestImageValidationBeforePOST、TestNormalizedFormatsAndMachineFailures、TestEditRejectsInvalidInputsBeforeRequest |
| Unicode prompt 与配置错误 | TestPromptCodepointBoundaryAndConfigFailure、TestImageValidationBeforePOST |
| SVG/GIF 文件与未知内容 | TestOfficialImageParameters、TestJSONResultLifecycle |
| multipart/JSON 编码、顺序、mask/markup | TestEditReferenceEncodingAndOrder、TestMarkupAndReferenceValidation、TestEditUploadsReferencesAndMask |
| 输入大小、data URL MIME、透明像素与 EXIF 方向 | TestMarkupAndReferenceValidation、TestEditWebPWithPaletteMask |
| 上游元数据、精确数值、request ID 与原始错误 | TestJSONResultLifecycle、TestErrorResponseMetadataAndStreamFallback、TestRequestHeaderSurvivesErrorBodyTimeout |
| SSE 分块、多行、心跳、大事件、未知事件 | TestStreamingLifecycle |
| 多图终态、不完整流与流内错误 | TestStreamingLifecycle |
| 单 JSON、JSONL 恰好一个终态、流式回退 | TestStreamingLifecycle、TestStreamingFallbackAndTimeout |
| 限流、额度、Retry-After、超时和请求体重传 | TestQuotaAndRetryAfter、TestEditRequestOutcomes、TestStreamingFallbackAndTimeout、已有生成重试测试 |
| 仅重试下载、部分成功和异常数量不覆盖 | TestDownloadRetriesKeepGeneratedResult、TestJSONResultLifecycle、TestUnexpectedExtraImagesArePreserved |
| 资源上限、输出失败、CLI 语法错误 | TestStreamingResponseResourceLimit、TestJSONSyntaxErrorAfterModeAndClosedOutput |
| Ctrl-C、退出码 130、清理和预览保留 | TestCancelStreamingSubprocess、TestCancelWhileReadingPrompt、TestCancelWhileResolvingAuth（Unix；Windows Go 不支持通过 Process.Signal 发送 Interrupt，这些测试显式跳过） |
| 既有命令回归 | generate/edit/models/init/version/completion、配置优先级和路径测试 |

新增测试在 [compatibility_test.go](../../compatibility_test.go)、[image_process_test.go](../../image_process_test.go)、[edit_test.go](../../edit_test.go)。移除已退出生产调用路径的旧响应解析/保存函数及对应私有测试；实际行为由上述 CLI 测试覆盖。

## 检查记录

工具链使用 Go 1.26.10。完整门禁、竞态检测、漏洞扫描、六个平台构建和配套 skills 校验均通过。下列是完整门禁输出；跨平台只做构建，未在非本机平台执行测试。

```text
go vet ./...
golangci-lint run ./...
0 issues.
go test ./...
ok  github.com/kiddingbaby/image-gen  4.150s
go build -trimpath -o image-gen .
```

```text
go test -race ./...
ok  github.com/kiddingbaby/image-gen  7.193s

govulncheck ./...
No vulnerabilities found.

linux/amd64 OK
linux/arm64 OK
darwin/amd64 OK
darwin/arm64 OK
windows/amd64 OK
windows/arm64 OK
```

配套 skills 仓运行 `bash scripts/check.sh`：gofmt、vet、测试、模板镜像核对通过，11 个 skill 校验通过。Markdown 本地链接和 JSON/TOML 示例另行检查。

## 发布前实现复审

固定基点：CLI `9d9db67`，配套 skill `f21d887`。审查使用两个独立 agent，分别检查 Standards 与 Spec。

- Standards：初审无发现。修复复审指出取消测试依赖固定启动延迟；已改为 stdin 读取开始的就绪标记，消除慢 CI 误报。测试通过公共 CLI、HTTP 和文件系统观察行为。
- Spec：发现并修复三类问题。请求发送前等待 stdin / 外部取密钥不响应 Ctrl-C；GIF 只匹配四字节前缀；响应数组里有非对象项时，其他项的 base64 未剥离。
- 回归测试：TestCancelWhileReadingPrompt、TestCancelWhileResolvingAuth、TestRejectsIncompleteGIFSignature、TestMalformedRowsDoNotLeakImageData，均先观察失败再修复。
- 修复后 make check、竞态检测和漏洞扫描通过；发布前另检查远程 CI 与发布产物。

## 验证边界

未发起真实生成/编辑调用，不产生模型费用。SVG 编辑输出、GIF 大帧间隔、混合格式 mask 和 stream+n>1 的实际服务可用性仍未验证。CLI 已能表达与处理这些协议；网关能力、模型限制和账户权限由实际服务裁决。
