# Contributing

欢迎提 issue 与 PR。这个仓只包含 CLI 本体；参数、退出码、失败信息解读这些**用法文档**在 [kiddingbaby/skills 的 image-gen skill](https://github.com/kiddingbaby/skills/blob/main/skills/image-gen/SKILL.md)，两边不要互相抄。

## 开发环境

- Go ≥ 1.26（`go.mod` 的 `go` 指令就是下限）
- 可选：[golangci-lint](https://golangci-lint.run/welcome/install/) ≥ 2.12，`make lint` 需要它

```sh
git clone git@github.com:kiddingbaby/image-gen.git
cd image-gen
make check      # 格式 + vet + lint + 测试 + 构建
make test       # 只跑测试
make cover      # 覆盖率（不设阈值）
```

`make check` 就是 CI 的门禁；CI 只是把它拆成三个 job 并行跑，命令相同。

## 改代码

- 行为改动先写会失败的测试再改实现。集成测试用 `httptest` 起假网关，不需要真实密钥，`go test ./...` 应当离线可跑。
- 错误处理：返回值能包装就 `%w`；确实要忽略的写 `_ =`，不要留裸调用（errcheck 会拦住）。
- 面向用户的文案（报错、告警）保持中文，退出码语义见 `main.go` 里的 `exit*` 常量。
- 参数命名与默认值属于对外契约：改它们就是破坏性变更，要升 minor 或 major，并同步 skills 仓的 `SKILL.md`。

## 提交与 PR

- 提交信息用 Conventional Commits：`feat:`、`fix:`、`refactor:`、`docs:`、`chore(deps):`。破坏性变更加 `!`。
- 一个 PR 做一件事；面向用户的变更在 `CHANGELOG.md` 的 `Unreleased` 记一条。
- PR 会跑 lint、ubuntu + windows 测试、govulncheck；三条都绿再请 review。

## 发布

1. 改 `VERSION`（纯 `X.Y.Z`），在 `CHANGELOG.md` 把 `Unreleased` 落成对应版本标题并补日期。
2. 合并后打 tag 并推送：`git tag -a vX.Y.Z -m "..." && git push origin vX.Y.Z`。
3. `.github/workflows/release.yml` 会核对 tag 与 `VERSION` 一致，交叉编译六个平台二进制、生成 `image-gen-checksums.txt`，用 `gh release create`（已存在则 `--clobber` 上传）发布。

发布产物的文件名是固定约定，不要改：`image-gen-<os>-<arch>[.exe]` 与 `image-gen-checksums.txt`——skills 仓的安装脚本按这两个名字下载并核对 SHA256。