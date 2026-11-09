# image-gen

把提示词发给 OpenAI 兼容的生图网关，图片落盘后把绝对路径打到 stdout。单文件静态二进制，无运行时依赖。

这是 CLI 本体；模型选择、失败信息解读、尺寸与质量参数的完整说明在 skill 文档里：[skills/image-gen](https://github.com/kiddingbaby/skills/blob/main/skills/image-gen/SKILL.md)。agent 装的是那份文档，CLI 从这里装。

## 安装

```sh
go install github.com/kiddingbaby/image-gen@latest     # 需要 Go ≥ 1.26，装到 $GOBIN
```

没有 Go 就下对应平台的二进制。下面这段会顺带核对发布产物的 SHA256：

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

Windows 下载 `image-gen-windows-amd64.exe` 或 `image-gen-windows-arm64.exe`，对一下同一次发布里的 `image-gen-checksums.txt` 再放进 PATH。

## 配置

环境变量即可，不需要配置文件：

```sh
export IMAGE_GEN_BASE_URL="https://你的网关/v1"
export IMAGE_GEN_API_KEY="你的密钥"
```

要落成文件就用非交互写法（agent 与 CI 用这个）：

```sh
image-gen init --base-url https://你的网关/v1 --auth-env IMAGE_GEN_API_KEY
```

默认写到 `~/.config/image-gen/config.toml`（macOS 为 `~/Library/Application Support/`，Windows 为 `%AppData%`），权限 600。全部配置项见 [skills/image-gen/assets/config.example.toml](https://github.com/kiddingbaby/skills/blob/main/skills/image-gen/assets/config.example.toml)。

## 用法

```sh
image-gen generate "一只在机房里打盹的橘猫" --size 1024x1024 --quality high
echo "提示词" | image-gen generate       # 从 stdin 读，适合脚本
image-gen models                         # 列出网关可用图像模型
```

默认 stdout 每行一个落盘绝对路径；`--json` 时只有一个 JSON 对象，告警走 stderr。完整参数表、退出码与失败信息对照在 skill 文档里。

## 开发

```sh
make check     # gofmt + vet + test + build，与 CI 同一条命令
make test      # 只跑测试
make fmt       # 只修格式
```

## 版本

根 `VERSION`、`vX.Y.Z` tag 与 Release 资产同一条版本轴；发布工作流会核对 tag 与 `VERSION` 一致，`go install ...@latest` 与 Release 下载拿到同一个版本。

## License

[MIT](LICENSE)