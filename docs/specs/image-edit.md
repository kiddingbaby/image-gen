# 图片编辑 Spec

本文件保留基础编辑切片的历史约定，以下不作为当前完整 CLI 契约。完整官方参数、引用输入、输出格式与流式扩展见 [OpenAI Images API 兼容接口设计](openai-images-compatibility.md)；后续兼容性变更以该设计为准。

## Problem Statement

已有图片生成后，用户需要保留主体、修改背景，或结合参考图继续创作；当前 CLI 只能从提示词生成新图。

## Solution

新增 `image-gen edit [提示词] --image <本地图片>`。重复 `--image` 可提供参考图，`--mask` 可指定第一张图片的编辑区域。结果沿用生成命令的落盘与输出契约。

## User Stories

1. 用户可以从参数或 stdin 提供修改要求，上传已有图片并获得新的图片文件。
2. 用户可以按顺序提供多张参考图，并用可选 PNG mask 引导第一张图的局部修改。
3. 用户可以继续使用已有配置、模型、质量、尺寸、格式、张数、输出目录及 JSON 输出选项。
4. 输入文件或参数不合法时，在请求网关前明确报错；网络、上游和本地错误沿用现有退出码分类。

## Context / Authority

- 需求与范围：本次对话中用户确认增加图片编辑，CLI 入口为 `edit`。
- 项目约定：[CONTRIBUTING.md](../../CONTRIBUTING.md)。
- 接口：[OpenAI Images Edit](https://developers.openai.com/api/reference/resources/images/methods/edit)。
- 用法文档：[image-gen skill](https://github.com/kiddingbaby/skills/blob/main/skills/image-gen/SKILL.md)。

## Contracts & Data

- `--image` 必填，可重复；1–16 张本地 PNG、JPEG、WebP，顺序保持不变；单文件小于 50 MiB。
- `--mask` 可选：PNG，与第一张原图宽高一致，带透明通道，以透明区域标记修改范围。mask 是模型的编辑指引，不保证像素级边界。
- 上传使用 `POST /v1/images/edits` 和 multipart/form-data；图片字段为 `image[]`，mask 字段为 `mask`。
- 共享参数与默认值沿用 `generate`；未设置的可选字段不发送。
- 成功 stdout 为绝对路径列表，`--json` 为现有的 model/images 对象；原图不覆盖。
- 输入错误返回 2；配置错误 3；网络错误 4；上游错误 5；本地写入等错误 1。
- 仅 429 按现有策略重试，并完整重传请求；超时和 5xx 不自动重新编辑。

## Implementation Decisions

- 复用配置解析、参数校验、请求重试、响应解析和结果落盘。
- multipart 请求使用临时文件保存，避免多张原图同时占用大量内存；所有退出路径清理临时文件。
- 本地文件校验在发送请求前完成，上传 MIME 根据内容判断。

## Testing Decisions

- 沿用已有公开 CLI 测试入口和 httptest 假网关，验证 HTTP 协议、落盘内容、stdout 和退出码。
- 首个失败测试：通过 `edit --image` 上传 PNG，网关返回图片，断言原图上传正确、产物正确且原图未被改写。
- 后续覆盖参考图顺序、mask、stdin/配置/JSON、输入拒绝、URL 响应、429 重传、超时和上游失败。

## Verification

- `make check`（格式、vet、lint、全部测试、构建）。
- `go test -race ./...`。
- 同步 skill 用法后运行该仓库校验。

## Docs to Update

- README / CHANGELOG。
- skills 仓库的 image-gen 用法、内容版本和触发语。

## Out of Scope

生成记录、恢复下载、批量任务、流式预览、远程图片输入和新增模型专属参数。
