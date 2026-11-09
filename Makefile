# 本地开发入口。CI 跑的是同一组命令，只是没有 make 包装。
.DEFAULT_GOAL := check

GO ?= go
GOLANGCI_LINT ?= golangci-lint
BIN := image-gen

.PHONY: check fmt fmt-check vet lint test cover build clean help

help: ## 列出可用目标
	@awk 'BEGIN {FS = ":.*?## "} /^[a-z-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

check: fmt-check vet lint test build ## 完整门禁：格式、vet、lint、测试、构建

fmt: ## 格式化（会改文件）
	gofmt -w $$(git ls-files '*.go')

fmt-check: ## 只校验格式，不改文件
	@out="$$(gofmt -l $$(git ls-files '*.go'))"; \
	if [ -n "$$out" ]; then \
		printf '以下文件未格式化，请运行 make fmt：\n%s\n' "$$out" >&2; \
		exit 1; \
	fi

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint（配置见 .golangci.yml）
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		printf '未安装 golangci-lint；安装见 https://golangci-lint.run/welcome/install/\n' >&2; \
		exit 1; \
	}
	$(GOLANGCI_LINT) run ./...

test: ## 跑测试
	$(GO) test ./...

cover: ## 跑测试并输出覆盖率（不设阈值）
	$(GO) test -cover ./...

build: ## 构建本机二进制到 ./image-gen
	$(GO) build -trimpath -o $(BIN) .

clean: ## 清理构建产物
	rm -f $(BIN) $(BIN).exe
	rm -rf dist