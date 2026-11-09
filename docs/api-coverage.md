# Images API 能力对照

核对日期：2026-11-09。工作区实现版本：0.12.0。以下「已实现」表示通过 CLI、HTTP 假网关和实际文件的协议测试；未发起真实服务生成请求，不代表所有网关、模型和账户均接受这些组合。

请求、输出、错误及验收契约见 [兼容接口规格](specs/openai-images-compatibility.md)。范围是直接 Images API，不含 Responses API 多轮会话或 HTTP 服务端。

## 官方来源

- [生成接口 G](https://developers.openai.com/api/reference/resources/images/methods/generate)
- [编辑接口 E](https://developers.openai.com/api/reference/resources/images/methods/edit)
- [图像生成指南](https://developers.openai.com/api/docs/guides/image-generation)
- [生成流事件](https://developers.openai.com/api/reference/resources/images/generation-streaming-events)、[编辑流事件](https://developers.openai.com/api/reference/resources/images/edit-streaming-events)
- [错误码](https://developers.openai.com/api/docs/guides/error-codes)

Context7 检索与上述原始页面共同用于核对；遇到缓存片段或页面冲突，按规格中逐项写明的策略处理。

## 参数覆盖

G/E 顶层字段并集 23 项全部可表达，嵌套字段见对应行。可选参数未设置时省略；显式 0、false、auto 保留。

| 官方字段 | CLI 映射及实现状态 |
| --- | --- |
| model | --model / 环境变量 / 配置；G 必需，E 可省略；不猜测默认模型 |
| prompt | 位置参数或 stdin；trim 后合法 UTF-8，1–32,000 码点 |
| n | --count，1–10；SVG 只接受 1 |
| size | --size，auto 或正整数 WIDTHxHEIGHT；模型专属限制交给上游 |
| quality | --quality；现代枚举完整覆盖，G 另允许 standard/hd |
| background | --background，transparent/opaque/auto；透明仅 PNG/WebP |
| output_format | --format，PNG/JPEG/WebP/GIF/SVG，jpg 归一为 jpeg |
| output_compression | --output-compression，0–100，仅 JPEG/WebP |
| moderation | --moderation，auto/low |
| complexity | --complexity，low/medium/high |
| frame_count | --frame-count，GIF 2–16 帧 |
| frame_duration_ms | --frame-duration-ms，GIF 至少 20 ms；不自行舍入 |
| stream | --stream[=true\|false]，SSE 与 JSON 回退 |
| partial_images | --partial-images，0–3；显式传入要求 stream=true |
| safety_identifier | --safety-identifier，最多 128 码点 |
| svg | --svg-model → svg.model；--svg-reasoning-effort → svg.reasoning.effort；嵌套 JSON 对象 |
| user | --user，弃用兼容参数，显式发送 |
| response_format | G 的 --response-format，url/b64_json，旧模型兼容 |
| style | G 的 --style，vivid/natural，旧模型兼容 |
| images | E 的有序 --image / --image-url / --image-file-id；JSON images[].image_url/file_id 或 multipart image[] |
| mask | E 的 --mask / --mask-url / --mask-file-id；PNG alpha 与已知原图尺寸校验 |
| markup | E 的 --markup / --markup-url / --markup-file-id；透明像素、EXIF 方向后尺寸、像素上限校验 |
| input_fidelity | E 的 --input-fidelity，high/low，默认省略 |

本地或 data URL 图片限定 PNG/JPEG/WebP；SVG 编辑输入不受支持。每个文件严格小于 50,000,000 字节，JSON image_url 最多 20,971,520 字符。远程输入不自动下载，不调用 Files API。

全部输入是本地文件且没有 svg 子对象时使用 multipart；混合引用或嵌套 svg 时使用 JSON，本地图片转为 data URL。两种编码都支持完整重传和 307/308，请求结束清理临时文件。

## 响应、产物与失败

| 契约 | 实现状态 |
| --- | --- |
| base64 / URL | 解码或下载后按实际格式原子落盘；不发送 API key 到图片下载地址 |
| SVG / GIF | 识别实际字节，SVG 校验 XML 根元素，不执行脚本、不加载外部资源 |
| 元数据 | JSON 保留 created、usage、svg_usage、实际参数、revised_prompt、未知字段和精确数值；去除 base64；保存 x-request-id |
| SSE | 支持 LF/CRLF、多行 data、心跳、网络分块、大事件、未知事件；总响应限制 192 MiB |
| 完成判定 | 按 n 收集 completed；partial_image_index 只表示预览序号；EOF/[DONE] 缺最终图报失败 |
| 输出 | 文本最终路径；--json 单对象；--jsonl 实时事件且末尾恰好一条 result |
| 部分失败 | 已保存图片和预览保留；下载 URL 留在元数据中；数量不符报 incomplete_result |
| 错误 | 稳定错误码、stage、generation_status、retry.scope、原上游 error 对象 |
| 重试 | 限流 429 最多两次且遵守 Retry-After；额度 429、超时、5xx、流中断不重复 POST；临时下载失败仅重试 URL |
| 取消 | Ctrl-C 取消网络请求，返回 130，清理上传临时文件，保留产物 |

## 迁移与服务验证状态

0.12.0 不再强制发送 size=1024x1024、quality=medium。旧偏好通过显式参数或配置 `[defaults]` 保留。JSON 保留原 model/images 字段并扩充版本化结构；编辑省略模型时 model=null。未知图片内容现在报错，不再按请求格式猜扩展名。

以下项目已经能正确构造请求，但实际服务行为尚未验证：

1. 编辑输出 SVG：E schema 接受，指南却明确不支持；CLI 允许表达，不宣称官方已支持。
2. GIF 帧间隔超过 2000 ms：schema 只有最小值，指南列到 2000；CLI 不裁剪，交给上游判断。
3. JPEG/WebP 原图加 PNG mask：参数页与指南的同格式要求不一致；保留请求表达。
4. stream+n>1：CLI 按接收顺序收齐最终图片；网关和模型是否支持仍需实际证据。

测试入口见 [compatibility_test.go](../compatibility_test.go)、[image_process_test.go](../image_process_test.go)、[edit_test.go](../edit_test.go)。源码边界为参数与校验、编辑输入、HTTP 传输、JSON/SSE 解码和结果渲染；实现文件以 image_ 前缀区分职责。
