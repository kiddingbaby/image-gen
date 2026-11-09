# Changelog

本文件记录面向用户的变更。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added

- 完整映射 Images API 生成/编辑参数：SVG/GIF、complexity、帧控制、input_fidelity、safety_identifier、嵌套 svg 选项，以及旧模型的 response_format/style/user。
- 编辑支持按全局顺序混合本地图片、HTTP(S)/data URL 和 File ID；新增 mask 引用与透明 PNG markup，校验方向后的尺寸、透明像素和编码长度，自动选择 multipart 或 JSON。
- SSE 流式预览与最终产物；`--jsonl` 输出实时事件，`--json` 始终返回单对象。Ctrl-C 返回 130，清理上传临时文件并保留产物。
- JSON 结果保留上游元数据、usage/svg_usage、request ID 和未知字段；结构化错误区分阶段、生成状态与重试范围，多图失败保留已保存图片。

- 新增 `edit` 命令：上传本地图片并按提示词编辑，支持重复 `--image` 提供最多 16 张参考图，以及 `--mask` 引导第一张图的局部修改。复用生成命令的配置、输出参数、绝对路径和 JSON 输出，编辑结果另存为新文件。
- 编辑前校验 PNG/JPEG/WebP 文件格式、单文件小于 50,000,000 字节，以及 PNG mask 的尺寸和透明通道。multipart 上传使用临时文件，支持 429 重传和 307/308 重定向，请求结束后清理。

### Changed

- **0.12.0 兼容性变更**：未指定的可选字段不再发送，size/quality 使用上游默认 auto。要保持历史效果，显式设置 `--size 1024x1024 --quality medium`，或写入 `[defaults]` 的 size/quality。编辑允许省略 model，JSON model 此时为 null。
- prompt 统一限制为合法 UTF-8、1–32,000 个 Unicode 码点；本地上传严格小于 50,000,000 字节；尺寸仅检查正整数语法，模型专属限制交给上游。
- 未知图片内容改为失败；结果数量不符返回 incomplete_result。JSON 增加 schema_version=1 及错误契约，原 model/images 字段保留。

- 仓库工程化补齐：CI 拆成 lint（golangci-lint）/ test（ubuntu + windows，发版前手动加 macOS）/ govulncheck 三个 job；新增 `.golangci.yml`、dependabot、issue 与 PR 模板、`CONTRIBUTING.md`、`SECURITY.md`、`CODE_OF_CONDUCT.md`。
- 本地入口收敛到 `make`（`check` / `fmt` / `fmt-check` / `vet` / `lint` / `test` / `cover` / `build` / `clean`），删掉仓库自用的 `scripts/check.sh`；发布工作流的 tag 与 `VERSION` 核对逻辑不变。
- 仓库已公开，只在 GitHub 维护：删掉 Gitea 侧的 `validate.yml` 与 Gitea 镜像地址，安装与发布地址只剩 GitHub。

### Fixed

- 额度类 429 不再自动重试；限流尊重 Retry-After，超时、5xx 和流中断不重复 POST。图片 URL 临时下载错误仅重试同一 URL，不重复生成。
- 请求 1 张但网关返回多张时不再覆盖产物；错误响应和读取错误体超时仍保留 request ID 等可用元数据。

- 补齐 6 处未检查的错误返回值（`resp.Body.Close`、`fmt.Fprintf`、测试里的 `w.Write` / `f.Close`）与 1 处未用 `%w` 包装的错误。仅错误处理方式变化，CLI 行为不变。

## [0.11.3] - 2026-11-09

### Added

- 首个独立发布：从 [kiddingbaby/skills](https://github.com/kiddingbaby/skills) 的 `cmd/image-gen` 拆出，代码未改动。发布 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64` 六个二进制，附 `image-gen-checksums.txt`。
- 安装方式：`go install github.com/kiddingbaby/image-gen@latest`，或从 Release 下载二进制（安装脚本会核对 SHA256）。
