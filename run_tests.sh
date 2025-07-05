#!/bin/bash

# DHCP Server 综合测试脚本
# 创建时间: $(date)
# 测试用例总数: 42个

set -e

echo "================================="
echo "DHCP Server 综合测试套件"
echo "================================="
echo "开始时间: $(date)"
echo ""

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 创建测试报告目录
REPORT_DIR="test_reports"
mkdir -p "$REPORT_DIR"

# 设置测试报告文件
REPORT_FILE="$REPORT_DIR/test_report_$(date +%Y%m%d_%H%M%S).txt"
JSON_REPORT="$REPORT_DIR/test_report_$(date +%Y%m%d_%H%M%S).json"

echo "测试报告将保存到: $REPORT_FILE"
echo ""

# 初始化测试计数器
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0
SKIPPED_TESTS=0

# 测试分类
echo -e "${BLUE}测试分类说明:${NC}"
echo "1. 配置管理测试 (测试用例 1-3)"
echo "2. IP地址池测试 (测试用例 4-8)"
echo "3. DHCP服务器测试 (测试用例 9-15)"
echo "4. 网关健康检查测试 (测试用例 16-22)"
echo "5. HTTP API测试 (测试用例 23-35)"
echo "6. 高级功能测试 (测试用例 36-42)"
echo ""

# 函数：记录测试开始
log_test_start() {
    echo -e "${YELLOW}[测试开始]${NC} $1"
}

# 函数：记录测试成功
log_test_pass() {
    echo -e "${GREEN}[测试通过]${NC} $1"
    ((PASSED_TESTS++))
}

# 函数：记录测试失败
log_test_fail() {
    echo -e "${RED}[测试失败]${NC} $1"
    echo "  错误信息: $2"
    ((FAILED_TESTS++))
}

# 函数：记录测试跳过
log_test_skip() {
    echo -e "${YELLOW}[测试跳过]${NC} $1"
    echo "  跳过原因: $2"
    ((SKIPPED_TESTS++))
}

# 函数：运行单个测试
run_single_test() {
    local test_name=$1
    local test_func=$2
    local test_desc=$3
    
    log_test_start "$test_name: $test_desc"
    ((TOTAL_TESTS++))
    
    if go test -run "^$test_func$" -v dhcp_comprehensive_test.go 2>&1 | grep -q "PASS"; then
        log_test_pass "$test_name"
        return 0
    else
        log_test_fail "$test_name" "详见详细日志"
        return 1
    fi
}

# 开始测试报告记录
{
    echo "DHCP Server 综合测试报告"
    echo "========================="
    echo "测试开始时间: $(date)"
    echo "测试文件: dhcp_comprehensive_test.go"
    echo "总测试用例数: 42"
    echo ""
} > "$REPORT_FILE"

# 运行所有测试并生成详细报告
echo -e "${BLUE}开始运行所有测试...${NC}"
echo ""

# 配置管理测试 (1-3)
echo -e "${BLUE}=== 配置管理测试 ===${NC}"
run_single_test "Test-01" "TestConfigLoad" "配置文件加载测试"
run_single_test "Test-02" "TestConfigValidation" "配置验证测试"
run_single_test "Test-03" "TestConfigBackup" "配置备份测试"

# IP地址池测试 (4-8)
echo -e "${BLUE}=== IP地址池测试 ===${NC}"
run_single_test "Test-04" "TestIPPoolCreation" "IP地址池创建测试"
run_single_test "Test-05" "TestIPAllocation" "IP地址分配测试"
run_single_test "Test-06" "TestIPRelease" "IP地址释放测试"
run_single_test "Test-07" "TestStaticBinding" "静态绑定测试"
run_single_test "Test-08" "TestLeaseExpiration" "租约过期测试"

# DHCP服务器测试 (9-15)
echo -e "${BLUE}=== DHCP服务器测试 ===${NC}"
run_single_test "Test-09" "TestDHCPServerCreation" "DHCP服务器创建测试"
run_single_test "Test-10" "TestDHCPServerPoolAccess" "DHCP服务器池访问测试"
run_single_test "Test-11" "TestDHCPServerChecker" "DHCP服务器检查器测试"
run_single_test "Test-12" "TestDHCPLeaseHistory" "DHCP租约历史测试"
run_single_test "Test-13" "TestDHCPAllLeases" "DHCP所有租约测试"
run_single_test "Test-14" "TestDHCPActiveLeases" "DHCP活跃租约测试"
run_single_test "Test-15" "TestDHCPHistoryFiltering" "DHCP历史过滤测试"

# 网关健康检查测试 (16-22)
echo -e "${BLUE}=== 网关健康检查测试 ===${NC}"
run_single_test "Test-16" "TestHealthCheckerCreation" "健康检查器创建测试"
run_single_test "Test-17" "TestHealthCheckerStatus" "健康检查器状态测试"
run_single_test "Test-18" "TestHealthCheckerPreferred" "健康检查器优先网关测试"
run_single_test "Test-19" "TestHealthCheckerDefault" "健康检查器默认网关测试"
run_single_test "Test-20" "TestHealthCheckerStop" "健康检查器停止测试"
run_single_test "Test-21" "TestHealthCheckerUnknownGateway" "健康检查器未知网关测试"
run_single_test "Test-22" "TestHealthCheckerMultipleGateways" "健康检查器多网关测试"

# HTTP API测试 (23-35)
echo -e "${BLUE}=== HTTP API测试 ===${NC}"
run_single_test "Test-23" "TestAPIServerCreation" "API服务器创建测试"
run_single_test "Test-24" "TestAPIHealthEndpoint" "API健康端点测试"
run_single_test "Test-25" "TestAPILeasesEndpoint" "API租约端点测试"
run_single_test "Test-26" "TestAPIActiveLeasesEndpoint" "API活跃租约端点测试"
run_single_test "Test-27" "TestAPIHistoryEndpoint" "API历史端点测试"
run_single_test "Test-28" "TestAPIStatsEndpoint" "API统计端点测试"
run_single_test "Test-29" "TestAPIGatewaysEndpoint" "API网关端点测试"
run_single_test "Test-30" "TestAPIDevicesEndpoint" "API设备端点测试"
run_single_test "Test-31" "TestAPIBindingsEndpoint" "API绑定端点测试"
run_single_test "Test-32" "TestAPIConfigEndpoint" "API配置端点测试"
run_single_test "Test-33" "TestAPIConfigValidateEndpoint" "API配置验证端点测试"
run_single_test "Test-34" "TestAPIDeviceAddEndpoint" "API设备添加端点测试"
run_single_test "Test-35" "TestAPIStaticBindingAddEndpoint" "API静态绑定添加端点测试"

# 高级功能测试 (36-42)
echo -e "${BLUE}=== 高级功能测试 ===${NC}"
run_single_test "Test-36" "TestConfigDeviceManagement" "配置设备管理测试"
run_single_test "Test-37" "TestConfigGatewayManagement" "配置网关管理测试"
run_single_test "Test-38" "TestConfigBindingManagement" "配置绑定管理测试"
run_single_test "Test-39" "TestIPPoolStats" "IP地址池统计测试"
run_single_test "Test-40" "TestIPPoolCleanup" "IP地址池清理测试"
run_single_test "Test-41" "TestLeaseRenewal" "租约续期测试"
run_single_test "Test-42" "TestConcurrentIPAllocation" "并发IP分配测试"

echo ""
echo -e "${BLUE}=== 测试完成 ===${NC}"
echo "结束时间: $(date)"
echo ""

# 生成测试总结
echo -e "${BLUE}测试总结:${NC}"
echo "总测试数: $TOTAL_TESTS"
echo -e "通过测试: ${GREEN}$PASSED_TESTS${NC}"
echo -e "失败测试: ${RED}$FAILED_TESTS${NC}"
echo -e "跳过测试: ${YELLOW}$SKIPPED_TESTS${NC}"

# 计算通过率
if [ $TOTAL_TESTS -gt 0 ]; then
    PASS_RATE=$(( (PASSED_TESTS * 100) / TOTAL_TESTS ))
    echo -e "通过率: ${GREEN}$PASS_RATE%${NC}"
else
    echo -e "通过率: ${RED}0%${NC}"
fi

# 写入测试报告
{
    echo ""
    echo "测试总结:"
    echo "========="
    echo "总测试数: $TOTAL_TESTS"
    echo "通过测试: $PASSED_TESTS"
    echo "失败测试: $FAILED_TESTS"
    echo "跳过测试: $SKIPPED_TESTS"
    echo "通过率: $PASS_RATE%"
    echo ""
    echo "测试结束时间: $(date)"
} >> "$REPORT_FILE"

# 生成JSON格式的测试报告
cat > "$JSON_REPORT" << EOF
{
    "test_report": {
        "timestamp": "$(date -Iseconds)",
        "total_tests": $TOTAL_TESTS,
        "passed_tests": $PASSED_TESTS,
        "failed_tests": $FAILED_TESTS,
        "skipped_tests": $SKIPPED_TESTS,
        "pass_rate": $PASS_RATE,
        "test_categories": {
            "config_management": {"tests": 3, "range": "1-3"},
            "ip_pool": {"tests": 5, "range": "4-8"},
            "dhcp_server": {"tests": 7, "range": "9-15"},
            "health_checker": {"tests": 7, "range": "16-22"},
            "http_api": {"tests": 13, "range": "23-35"},
            "advanced_features": {"tests": 7, "range": "36-42"}
        }
    }
}
EOF

echo ""
echo "详细测试报告已保存到: $REPORT_FILE"
echo "JSON测试报告已保存到: $JSON_REPORT"

# 运行完整的测试套件生成详细输出
echo ""
echo -e "${BLUE}运行完整测试套件 (详细输出)...${NC}"
echo ""

go test -v dhcp_comprehensive_test.go > "$REPORT_DIR/detailed_test_output_$(date +%Y%m%d_%H%M%S).log" 2>&1

echo "详细测试输出已保存到: $REPORT_DIR/detailed_test_output_$(date +%Y%m%d_%H%M%S).log"

# 如果有失败的测试，返回错误代码
if [ $FAILED_TESTS -gt 0 ]; then
    echo -e "${RED}有 $FAILED_TESTS 个测试失败${NC}"
    exit 1
else
    echo -e "${GREEN}所有测试通过！${NC}"
    exit 0
fi 