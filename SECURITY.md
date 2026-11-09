# Security Policy

## 支持范围

只对最新发布版本提供修复：当前是 `v0.11.3`。`main` 上的 `Unreleased` 内容可用，但不承诺稳定。

## 报告漏洞

**不要用公开 issue 报告漏洞。** 用 GitHub 的私密渠道：

- 打开 <https://github.com/kiddingbaby/image-gen/security/advisories/new> 提交 Security Advisory
- 或发邮件到 kiddingbaby@163.com，标题以 `[image-gen security]` 开头

尽量带上：受影响版本、复现步骤、影响面判断（例如「密钥会被写进日志」）。48 小时内会得到首次回复；修复发布后会在 advisory 与 `CHANGELOG.md` 里同时说明。

## 这个工具会碰到的敏感面

排查与报告时优先看这几处：

- **API 密钥**：`IMAGE_GEN_API_KEY`、`[auth] value`、以及 `auth-command` 的输出。配置文件默认写成 0600；错误信息与日志里不应出现密钥原文。
- **`auth-command`**：会在 PATH 上执行外部命令取密钥，参数走 `--auth-command-arg` 而不是拼进 shell 字符串。
- **上游 URL**：`base_url` 与响应里的图片 `url` 都由网关给出，只能跟随 `http`/`https`。
- **写盘**：默认写到当前目录，文件名由模型 id 与本地时间生成，不应被上游数据影响成路径穿越。

## 不在此范围

- 网关或具体图像模型的行为问题（内容合规、画质、汉字渲染）——那是上游服务的事，请找网关方。
- 需要已控制本机或已拿到密钥才能利用的问题。
- 从 `main` 分支自行构建的旧提交。