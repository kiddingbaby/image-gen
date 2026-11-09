# 本地开发入口。完整门禁是 check；其余 target 只是快速局部循环。
# CI 不经过 make，直接调用 scripts/check.sh。
.DEFAULT_GOAL := check

.PHONY: check fmt test build

# 完整校验：gofmt、vet、test、build。
check:
	bash scripts/check.sh

# 只修格式；失败时不会继续跑检查。
fmt:
	bash scripts/check.sh --fix

# 快速循环：只跑测试，不替代 check。
test:
	go test ./...

# 快速循环：只编译，不替代 check。
build:
	go build -o /dev/null .