#!/bin/bash

# 多平台构建脚本
# 生成 macOS、Linux、Windows 平台的可执行文件

APP_NAME="dhcp-server"
VERSION="1.0.0"
BUILD_DIR="build"
LDFLAGS="-s -w -X main.Version=${VERSION}"

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}================================${NC}"
echo -e "${BLUE}  DHCP Server 多平台构建脚本${NC}"
echo -e "${BLUE}     Version: ${VERSION}${NC}"
echo -e "${BLUE}================================${NC}"

# 清理旧的构建文件
echo -e "${YELLOW}清理旧的构建文件...${NC}"
rm -rf ${BUILD_DIR}
mkdir -p ${BUILD_DIR}

# 整理依赖
echo -e "${YELLOW}整理 Go 模块依赖...${NC}"
go mod tidy

# 检查编译环境
echo -e "${YELLOW}检查编译环境...${NC}"
go version

# 构建函数
build_platform() {
    local os=$1
    local arch=$2
    local ext=$3
    local platform_name=$4
    
    local output_name="${APP_NAME}-${platform_name}-${arch}${ext}"
    local output_path="${BUILD_DIR}/${output_name}"
    
    echo -e "${YELLOW}构建 ${platform_name} ${arch}...${NC}"
    
    # 禁用 CGO 以避免交叉编译问题
    CGO_ENABLED=0 GOOS=${os} GOARCH=${arch} go build \
        -ldflags="${LDFLAGS}" \
        -o "${output_path}" \
        .
    
    if [ $? -eq 0 ]; then
        echo -e "${GREEN}✓ ${platform_name} ${arch} 构建成功: ${output_path}${NC}"
        # 显示文件大小
        if [[ "$OSTYPE" == "darwin"* ]]; then
            size=$(ls -lh "${output_path}" | awk '{print $5}')
        else
            size=$(ls -lh "${output_path}" | awk '{print $5}')
        fi
        echo -e "  文件大小: ${size}"
    else
        echo -e "${RED}✗ ${platform_name} ${arch} 构建失败${NC}"
        return 1
    fi
}

# 构建各平台版本
echo -e "\n${BLUE}开始构建各平台版本...${NC}"

# macOS
build_platform "darwin" "amd64" "" "macos"
build_platform "darwin" "arm64" "" "macos"

# Linux
build_platform "linux" "amd64" "" "linux"
build_platform "linux" "arm64" "" "linux"
build_platform "linux" "386" "" "linux"

# Windows
build_platform "windows" "amd64" ".exe" "windows"
build_platform "windows" "386" ".exe" "windows"

echo -e "\n${BLUE}================================${NC}"
echo -e "${GREEN}构建完成！${NC}"
echo -e "${BLUE}================================${NC}"

# 显示构建结果
echo -e "\n${YELLOW}构建文件列表:${NC}"
ls -la ${BUILD_DIR}/

# 创建发布包
echo -e "\n${YELLOW}创建发布包...${NC}"
cd ${BUILD_DIR}

# 为每个平台创建压缩包
for file in dhcp-server-*; do
    if [[ "$file" == *.exe ]]; then
        # Windows 平台
        platform=$(echo $file | sed 's/dhcp-server-\(.*\)\.exe/\1/')
        zip_name="${APP_NAME}-${platform}-v${VERSION}.zip"
        zip "${zip_name}" "${file}"
        echo -e "${GREEN}✓ 创建 ${zip_name}${NC}"
    else
        # Unix 平台
        platform=$(echo $file | sed 's/dhcp-server-\(.*\)/\1/')
        tar_name="${APP_NAME}-${platform}-v${VERSION}.tar.gz"
        tar -czf "${tar_name}" "${file}"
        echo -e "${GREEN}✓ 创建 ${tar_name}${NC}"
    fi
done

cd ..

echo -e "\n${BLUE}================================${NC}"
echo -e "${GREEN}发布包创建完成！${NC}"
echo -e "${BLUE}================================${NC}"

echo -e "\n${YELLOW}发布包列表:${NC}"
ls -la ${BUILD_DIR}/*.zip ${BUILD_DIR}/*.tar.gz 2>/dev/null

echo -e "\n${YELLOW}使用说明:${NC}"
echo -e "1. 选择对应平台的可执行文件"
echo -e "2. 将 config.yaml 文件放在同一目录"
echo -e "3. 运行程序: ./dhcp-server -config config.yaml"
echo -e "4. 或者使用参数: ./dhcp-server --help"

echo -e "\n${GREEN}构建脚本执行完成！${NC}" 