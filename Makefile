# Makefile for Cross-Platform Go Compilation

# 使用 Bash 作为 shell，确保命令在不同系统上行为一致
SHELL := /bin/bash

# 定义当前版本信息
# VERSION 可以是固定的，也可以是动态生成的，例如：
# VERSION := 1.0.0
# 获取当前日期时间作为版本（推荐，每次编译都有唯一版本）
VERSION := $(shell date +%Y%m%d%H%M%S)
# 如果需要包含 Git commit hash，可以这样（需要有 Git 环境）：
# GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
# VERSION := $(shell date +%Y%m%d)-${GIT_COMMIT}

# .PHONY 声明所有目标为 phony，表示它们不是文件名，即使存在同名文件也会执行
.PHONY: all clean build build-all linux-amd64 linux-arm64 windows-amd64 darwin-amd64 darwin-arm64

# 定义二进制文件的名称
BINARY_NAME := k8s-backup

# 定义备份目录的通用前缀，用于 clean 目标
BACKUP_DIR_PREFIX := k8s-backup-

# LDFLAGS 用于向 Go 编译器传递链接器标志，用于嵌入版本信息
# -X main.version=${VERSION} 会将 VERSION 的值注入到 main 包的 version 变量中
LDFLAGS := -ldflags="-X 'main.version=${VERSION}'"

# -----------------------------------------------------------------------------
# 默认目标：为当前操作系统和架构编译
# -----------------------------------------------------------------------------
all: build
	@echo "✅ 本地编译完成: ${BINARY_NAME}"

# -----------------------------------------------------------------------------
# 清理目标：删除编译生成的可执行文件和备份目录
# -----------------------------------------------------------------------------
clean:
	@echo "🧹 正在清理生成的文件和备份目录..."
	rm -f ${BINARY_NAME}
	rm -f ${BINARY_NAME}-*
	# 查找并删除所有以 BACKUP_DIR_PREFIX 开头的目录
	find . -maxdepth 1 -type d -name "${BACKUP_DIR_PREFIX}*" -exec rm -rf {} +
	@echo "🗑️ 清理完成。"

# -----------------------------------------------------------------------------
# 构建目标：与 'all' 相同，明确的本地构建命令
# -----------------------------------------------------------------------------
build:
	@echo "🏗️ 正在为当前系统编译 ${BINARY_NAME} (版本: ${VERSION})..."
	go build ${LDFLAGS} -o ${BINARY_NAME} .

# -----------------------------------------------------------------------------
# 跨平台编译目标
# -----------------------------------------------------------------------------

# 编译 Linux 64位 (AMD64)
linux-amd64:
	@echo "🏗️ 正在编译 Linux 64位 (AMD64) 版本 (版本: ${VERSION})..."
	GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o ${BINARY_NAME}-linux-amd64 .
	@echo "✅ Linux 64位 (AMD64) 版本编译完成: ${BINARY_NAME}-linux-amd64"

# 编译 Linux ARM 64位 (例如 Raspberry Pi 3/4)
linux-arm64:
	@echo "🏗️ 正在编译 Linux ARM 64位 版本 (版本: ${VERSION})..."
	GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o ${BINARY_NAME}-linux-arm64 .
	@echo "✅ Linux ARM 64位 版本编译完成: ${BINARY_NAME}-linux-arm64"

# 编译 Windows 64位 (AMD64)
windows-amd64:
	@echo "🏗️ 正在编译 Windows 64位 (AMD64) 版本 (版本: ${VERSION})..."
	GOOS=windows GOARCH=amd64 go build ${LDFLAGS} -o ${BINARY_NAME}-windows-amd64.exe .
	@echo "✅ Windows 64位 (AMD64) 版本编译完成: ${BINARY_NAME}-windows-amd64.exe"

# 编译 macOS Intel 64位 (AMD64)
darwin-amd64:
	@echo "🏗️ 正在编译 macOS Intel 64位 (AMD64) 版本 (版本: ${VERSION})..."
	GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -o ${BINARY_NAME}-darwin-amd64 .
	@echo "✅ macOS Intel 64位 (AMD64) 版本编译完成: ${BINARY_NAME}-darwin-amd64"

# 编译 macOS Apple Silicon (ARM64)
darwin-arm64:
	@echo "🏗️ 正在编译 macOS Apple Silicon (ARM64) 版本 (版本: ${VERSION})..."
	GOOS=darwin GOARCH=arm64 go build ${LDFLAGS} -o ${BINARY_NAME}-darwin-arm64 .
	@echo "✅ macOS Apple Silicon (ARM64) 版本编译完成: ${BINARY_NAME}-darwin-arm64"

# -----------------------------------------------------------------------------
# 一键编译所有平台版本
# -----------------------------------------------------------------------------
build-all: linux-amd64 linux-arm64 windows-amd64 darwin-amd64 darwin-arm64
	@echo "✨ 所有平台版本编译完成！"

