# image-gen

[![CI](https://github.com/kiddingbaby/image-gen/actions/workflows/ci.yml/badge.svg)](https://github.com/kiddingbaby/image-gen/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kiddingbaby/image-gen)](https://github.com/kiddingbaby/image-gen/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/kiddingbaby/image-gen)](go.mod)
[![License](https://img.shields.io/github/license/kiddingbaby/image-gen)](LICENSE)

把提示词发给 OpenAI 兼容的生图网关，图片落盘后把绝对路径打到 stdout。单文件静态二进制，无运行时依赖；网关地址、密钥、默认模型都由配置与环境变量决定，不写死。

参数表、退出码、失败信息怎么读这些用法细节在 [skills/image-gen](https://github.com/kiddingbaby/skills/blob/main/skills/image-gen/SKILL.md)——那是 agent 装的 skill 文档，本 README 只讲怎么装、怎么跑起来。

## 安装

```sh
go install github.com/kiddingbaby/image-gen@latest     # 需要 Go ≥ 1.26，装到 $GOBIN
```

没有 Go 就用下面的脚本下二进制，会顺带核对发布产物的 SHA256：

```sh
os=$(uname -s | tr 'A-Z' 'a-z'); arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
base="https://github.com/kiddingbaby/image-gen/releases/latest/download"
tmp=$(mktemp -d)
curl -fsSL "$base/image-gen-$os-$arch" -o "$tmp/image-gen"
curl -fsSL "$base/image-gen-checksums.txt" -o "$tmp/checksums.txt"
want=$(grep " image-gen-$os-$arch\$" "$tmp/checksums.txt" | cut -d' ' -f1)
got=$(openssl dgst -sha256 "$tmp/image-gen" | awk '{print $NF}')
[ "$want" = "$got" ] || { echo "校验失败：二进制与 checksums 不一致，别装" >&2; exit 1; }
install -m 755 "$tmp/image-gen" ~/.local/bin/image-gen
```

Windows 下载 `image-gen-windows-amd64.exe` 或 `image-gen-windows-arm64.exe`，对一下同一次发布里的 `image-gen-checksums.txt` 再放进 PATH。也可以从源码装：

```sh
git clone https://github.com/kiddingbaby/image-gen.git
cd image-gen && make build      # 产出 ./image-gen
```

安装完确认一下：`image-gen --version` 应当有输出（等价于 `image-gen version`）。

## 配置

环境变量最快，不需要配置文件：

```sh
export IMAGE_GEN_BASE_URL="https://你的网关/v1"
export IMAGE_GEN_API_KEY="你的密钥"
```

要落成文件就用非交互写法（agent 与 CI 用这个）：

```sh
image-gen init --base-url https://你的网关/v1 --auth-env IMAGE_GEN_API_KEY

# 用密钥管理器取密钥：命令与参数分开传，参数里有空格也安全
image-gen init --base-url https://你的网关/v1 \
  --auth-command pass --auth-command-arg show --auth-command-arg "ai/image key"
```

配置默认写到 `~/.config/image-gen/config.toml`（macOS 为 `~/Library/Application Support/`，Windows 为 `%AppData%`），权限 600；用 `--config` 或 `IMAGE_GEN_CONFIG` 改路径，`--force` 才覆盖。所有配置项与 `[auth]` 的 command / env / value 三种取法见 [skills/image-gen/assets/config.example.toml](https://github.com/kiddingbaby/skills/blob/main/skills/image-gen/assets/config.example.toml)。

命令行参数 > 环境变量 > `config.toml`。

## 用法

```sh
image-gen generate "一只在机房里打盹的橘猫" --size 1024x1024 --quality high
echo "提示词" | image-gen generate                      # 从 stdin 读，适合脚本
image-gen models                                        # 列出网关可用图像模型
image-gen generate "..." --model <模型> --count 2 --format webp --output-dir ./out
```

默认 stdout 每行一个落盘绝对路径；加 `--json` 时 stdout 只有一个 JSON 对象，告警走 stderr。退出码区分「改命令」（2）、「补配置」（3）、「可重试的网络错误」（4）与「上游报错」（5），完整对照在 skill 文档里。

## 开发

```sh
make check      # 完整门禁：格式 + vet + lint + 测试 + 构建
make test       # 只跑测试（离线，用 httptest 假网关）
make lint       # golangci-lint，配置见 .golangci.yml
make cover      # 覆盖率，不设阈值
make help       # 列出全部目标
```

需要 Go ≥ 1.26；`make lint` 还需要 golangci-lint ≥ 2.12。CI 把这组命令拆成 lint / test（ubuntu + windows，发版前手动加 macOS）/ govulncheck 三个 job 并行跑，命令与本地一致。

## 发布

`VERSION`、`vX.Y.Z` tag 与 Release 资产是同一条版本轴：tag 推送后 `.github/workflows/release.yml` 先核对 tag 与 `VERSION` 一致，再交叉编译六个平台二进制、生成 `image-gen-checksums.txt` 并发布，所以 `/releases/latest/download/` 与 `go install ...@latest` 拿到的是同一个版本。流程细节见 [CONTRIBUTING.md](CONTRIBUTING.md#发布)。

## 相关

- [kiddingbaby/skills](https://github.com/kiddingbaby/skills) — 依赖本 CLI 的 `image-gen` skill：何时用、参数表、失败信息对照
- 生成网关需兼容 OpenAI `/v1/images/generations`；只接了 chat 端点的模型在这里用不了

## 许可与反馈

[MIT](LICENSE)。安全问题请走私密渠道，见 [SECURITY.md](SECURITY.md)；参与贡献前请读 [CONTRIBUTING.md](CONTRIBUTING.md) 与 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。