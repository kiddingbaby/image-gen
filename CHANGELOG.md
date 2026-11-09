# Changelog

本文件记录面向用户的变更。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Changed

- 仓库工程化补齐：CI 拆成 lint（golangci-lint）/ test（ubuntu + windows，发版前手动加 macOS）/ govulncheck 三个 job；新增 `.golangci.yml`、dependabot、issue 与 PR 模板、`CONTRIBUTING.md`、`SECURITY.md`、`CODE_OF_CONDUCT.md`。
- 本地入口收敛到 `make`（`check` / `fmt` / `fmt-check` / `vet` / `lint` / `test` / `cover` / `build` / `clean`），删掉仓库自用的 `scripts/check.sh`；发布工作流的 tag 与 `VERSION` 核对逻辑不变。
- 仓库已公开，只在 GitHub 维护：删掉 Gitea 侧的 `validate.yml` 与 Gitea 镜像地址，安装与发布地址只剩 GitHub。

### Fixed

- 补齐 6 处未检查的错误返回值（`resp.Body.Close`、`fmt.Fprintf`、测试里的 `w.Write` / `f.Close`）与 1 处未用 `%w` 包装的错误。仅错误处理方式变化，CLI 行为不变。

## [0.11.3] - 2026-11-09

### Added

- 首个独立发布：从 [kiddingbaby/skills](https://github.com/kiddingbaby/skills) 的 `cmd/image-gen` 拆出，代码未改动。发布 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64` 六个二进制，附 `image-gen-checksums.txt`。
- 安装方式：`go install github.com/kiddingbaby/image-gen@latest`，或从 Release 下载二进制（安装脚本会核对 SHA256）。