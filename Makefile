.PHONY: run build vet test clean

# 本地运行
run:
	go run main.go

# 编译
build:
	go build -o nim-relay main.go

# 代码检查
vet:
	go vet ./...

# 运行测试
test:
	go test ./...

# 清理
clean:
	rm -f nim-relay
