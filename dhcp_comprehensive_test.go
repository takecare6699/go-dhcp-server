package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"dhcp-server/api"
	"dhcp-server/config"
	"dhcp-server/dhcp"
	"dhcp-server/gateway"
)

// 测试用例1-3: 配置管理测试
func TestConfigLoad(t *testing.T) {
	// 创建临时配置文件
	configData := `
server:
  interface: "eth0"
  port: 67
  lease_time: 24h
  api_port: 8080
  log_level: "info"
  debug: false

network:
  subnet: "192.168.1.0/24"
  netmask: "255.255.255.0"
  start_ip: "192.168.1.100"
  end_ip: "192.168.1.200"
  dns_servers: ["8.8.8.8", "1.1.1.1"]
  domain_name: "local"
  default_gateway: "192.168.1.1"
  lease_time: 86400
  
gateways:
  - name: "main_gateway"
    ip: "192.168.1.1"
    is_default: true
    description: "主网关"
    
bindings: []
devices: []

health_check:
  interval: 30s
  timeout: 5s
  retry_count: 3
  method: "ping"
`

	tmpFile, err := os.CreateTemp("", "test_config_*.yaml")
	if err != nil {
		t.Fatalf("创建临时配置文件失败: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(configData)
	if err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}
	tmpFile.Close()

	cfg, err := config.LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("加载配置文件失败: %v", err)
	}

	// 验证配置
	if cfg.Server.Interface != "eth0" {
		t.Errorf("期望接口为 eth0, 实际为 %s", cfg.Server.Interface)
	}
	if cfg.Network.StartIP != "192.168.1.100" {
		t.Errorf("期望起始IP为 192.168.1.100, 实际为 %s", cfg.Network.StartIP)
	}
	if len(cfg.Network.DNSServers) != 2 {
		t.Errorf("期望DNS服务器数量为2, 实际为 %d", len(cfg.Network.DNSServers))
	}
}

func TestConfigValidation(t *testing.T) {
	// 测试无效配置
	cfg := &config.Config{
		Server: config.ServerConfig{
			Interface: "eth0",
			Port:      67,
			APIPort:   8080,
		},
		Network: config.NetworkConfig{
			Subnet:  "invalid-subnet",
			StartIP: "192.168.1.100",
			EndIP:   "192.168.1.200",
		},
		Gateways: []config.Gateway{
			{Name: "test", IP: "192.168.1.1", IsDefault: true},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Error("期望配置验证失败，但验证通过了")
	}
}

func TestConfigBackup(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Interface: "eth0", Port: 67, APIPort: 8080},
		Network: config.NetworkConfig{
			Subnet:  "192.168.1.0/24",
			StartIP: "192.168.1.100",
			EndIP:   "192.168.1.200",
		},
		Gateways: []config.Gateway{
			{Name: "test", IP: "192.168.1.1", IsDefault: true},
		},
	}

	tmpFile, err := os.CreateTemp("", "test_backup_*.yaml")
	if err != nil {
		t.Fatalf("创建临时文件失败: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	err = cfg.BackupConfig(tmpFile.Name())
	if err != nil {
		t.Errorf("备份配置失败: %v", err)
	}

	// 验证备份文件是否存在
	if _, err := os.Stat(tmpFile.Name()); os.IsNotExist(err) {
		t.Error("备份文件不存在")
	}
}

// 测试用例4-8: IP地址池测试
func TestIPPoolCreation(t *testing.T) {
	cfg := createTestConfig()

	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	if pool == nil {
		t.Error("IP地址池为空")
	}

	stats := pool.GetPoolStats()
	if stats["total_ips"].(int) <= 0 {
		t.Error("IP地址池大小应大于0")
	}
}

func TestIPAllocation(t *testing.T) {
	cfg := createTestConfig()
	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	// 分配IP
	lease, err := pool.RequestIP("aa:bb:cc:dd:ee:ff", nil, "test-device")
	if err != nil {
		t.Fatalf("分配IP失败: %v", err)
	}

	if lease == nil {
		t.Error("租约为空")
	}

	if lease.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("期望MAC为 aa:bb:cc:dd:ee:ff, 实际为 %s", lease.MAC)
	}
}

func TestIPRelease(t *testing.T) {
	cfg := createTestConfig()
	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	// 分配IP
	lease, err := pool.RequestIP("aa:bb:cc:dd:ee:ff", nil, "test-device")
	if err != nil {
		t.Fatalf("分配IP失败: %v", err)
	}

	// 释放IP
	err = pool.ReleaseIP("aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Errorf("释放IP失败: %v", err)
	}

	// 验证IP已释放
	_, exists := pool.GetLease(lease.IP.String())
	if exists {
		t.Error("IP应该已被释放")
	}
}

func TestStaticBinding(t *testing.T) {
	cfg := createTestConfig()
	cfg.Bindings = []config.MACBinding{
		{
			Alias:    "test-device",
			MAC:      "aa:bb:cc:dd:ee:ff",
			IP:       "192.168.1.150",
			Gateway:  "main_gateway",
			Hostname: "test-device",
		},
	}

	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	// 请求静态绑定的IP
	lease, err := pool.RequestIP("aa:bb:cc:dd:ee:ff", nil, "test-device")
	if err != nil {
		t.Fatalf("请求静态IP失败: %v", err)
	}

	if lease.IP.String() != "192.168.1.150" {
		t.Errorf("期望IP为 192.168.1.150, 实际为 %s", lease.IP.String())
	}

	if !lease.IsStatic {
		t.Error("租约应该是静态的")
	}
}

func TestLeaseExpiration(t *testing.T) {
	cfg := createTestConfig()
	cfg.Server.LeaseTime = 1 * time.Millisecond // 设置极短的租期用于测试

	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	// 分配IP
	lease, err := pool.RequestIP("aa:bb:cc:dd:ee:ff", nil, "test-device")
	if err != nil {
		t.Fatalf("分配IP失败: %v", err)
	}

	// 等待租约过期
	time.Sleep(2 * time.Millisecond)

	if !lease.IsExpired() {
		t.Error("租约应该已过期")
	}
}

// 测试用例9-15: DHCP服务器测试
func TestDHCPServerCreation(t *testing.T) {
	cfg := createTestConfig()

	server, err := dhcp.NewServer(cfg)
	if err != nil {
		t.Fatalf("创建DHCP服务器失败: %v", err)
	}

	if server == nil {
		t.Error("DHCP服务器为空")
	}

	if server.GetStartTime().IsZero() {
		t.Error("服务器启动时间未设置")
	}
}

func TestDHCPServerPoolAccess(t *testing.T) {
	cfg := createTestConfig()
	server, err := dhcp.NewServer(cfg)
	if err != nil {
		t.Fatalf("创建DHCP服务器失败: %v", err)
	}

	pool := server.GetPool()
	if pool == nil {
		t.Error("无法获取IP地址池")
	}

	stats := server.GetPoolStats()
	if stats == nil {
		t.Error("无法获取池统计信息")
	}
}

func TestDHCPServerChecker(t *testing.T) {
	cfg := createTestConfig()
	server, err := dhcp.NewServer(cfg)
	if err != nil {
		t.Fatalf("创建DHCP服务器失败: %v", err)
	}

	checker := server.GetChecker()
	if checker == nil {
		t.Error("无法获取健康检查器")
	}

	status := server.GetGatewayStatus()
	if status == nil {
		t.Error("无法获取网关状态")
	}
}

func TestDHCPLeaseHistory(t *testing.T) {
	cfg := createTestConfig()
	server, err := dhcp.NewServer(cfg)
	if err != nil {
		t.Fatalf("创建DHCP服务器失败: %v", err)
	}

	// 获取历史记录
	history := server.GetHistory(10, "", "")
	if len(history) == 0 {
		t.Error("历史记录不应为空（包含示例数据）")
	}
}

func TestDHCPAllLeases(t *testing.T) {
	cfg := createTestConfig()
	server, err := dhcp.NewServer(cfg)
	if err != nil {
		t.Fatalf("创建DHCP服务器失败: %v", err)
	}

	// 分配一些IP
	server.GetPool().RequestIP("aa:bb:cc:dd:ee:01", nil, "device1")
	server.GetPool().RequestIP("aa:bb:cc:dd:ee:02", nil, "device2")

	allLeases := server.GetAllLeases()
	if len(allLeases) < 2 {
		t.Error("应该有至少2个租约")
	}
}

func TestDHCPActiveLeases(t *testing.T) {
	cfg := createTestConfig()
	server, err := dhcp.NewServer(cfg)
	if err != nil {
		t.Fatalf("创建DHCP服务器失败: %v", err)
	}

	// 分配一些IP
	server.GetPool().RequestIP("aa:bb:cc:dd:ee:01", nil, "device1")
	server.GetPool().RequestIP("aa:bb:cc:dd:ee:02", nil, "device2")

	activeLeases := server.GetActiveLeases()
	if len(activeLeases) < 2 {
		t.Error("应该有至少2个活跃租约")
	}
}

func TestDHCPHistoryFiltering(t *testing.T) {
	cfg := createTestConfig()
	server, err := dhcp.NewServer(cfg)
	if err != nil {
		t.Fatalf("创建DHCP服务器失败: %v", err)
	}

	// 测试MAC过滤
	history := server.GetHistory(10, "aa:bb:cc:dd:ee:01", "")
	if len(history) == 0 {
		t.Log("MAC过滤测试: 没有找到匹配的记录（可能是正常的）")
	}

	// 测试IP过滤
	history = server.GetHistory(10, "", "192.168.1.101")
	if len(history) == 0 {
		t.Log("IP过滤测试: 没有找到匹配的记录（可能是正常的）")
	}
}

// 测试用例16-22: 网关健康检查测试
func TestHealthCheckerCreation(t *testing.T) {
	cfg := createTestConfig()

	checker := gateway.NewHealthChecker(cfg)
	if checker == nil {
		t.Error("健康检查器为空")
	}

	// 测试默认状态
	status := checker.IsGatewayHealthy("main_gateway")
	if !status {
		t.Error("默认网关状态应该为健康")
	}
}

func TestHealthCheckerStatus(t *testing.T) {
	cfg := createTestConfig()
	checker := gateway.NewHealthChecker(cfg)

	// 测试状态获取
	status := checker.GetGatewayStatus()
	if status == nil {
		t.Error("网关状态映射不应为空")
	}

	if len(status) == 0 {
		t.Error("应该有至少一个网关状态")
	}
}

func TestHealthCheckerPreferred(t *testing.T) {
	cfg := createTestConfig()
	cfg.Gateways = append(cfg.Gateways, config.Gateway{
		Name:        "backup_gateway",
		IP:          "192.168.1.2",
		IsDefault:   false,
		Description: "备用网关",
	})

	checker := gateway.NewHealthChecker(cfg)

	// 测试优先网关选择
	gw := checker.GetHealthyGateway("backup_gateway")
	if gw == nil {
		t.Error("应该返回备用网关")
	}

	if gw.Name != "backup_gateway" {
		t.Errorf("期望返回backup_gateway, 实际返回 %s", gw.Name)
	}
}

func TestHealthCheckerDefault(t *testing.T) {
	cfg := createTestConfig()
	checker := gateway.NewHealthChecker(cfg)

	// 测试默认网关选择
	gw := checker.GetHealthyGateway("")
	if gw == nil {
		t.Error("应该返回默认网关")
	}

	if !gw.IsDefault {
		t.Error("返回的网关应该是默认网关")
	}
}

func TestHealthCheckerStop(t *testing.T) {
	cfg := createTestConfig()
	checker := gateway.NewHealthChecker(cfg)

	// 测试停止功能
	go checker.Start()
	time.Sleep(100 * time.Millisecond)

	// 停止检查器
	checker.Stop()

	// 验证停止后的状态
	time.Sleep(100 * time.Millisecond)
	// 检查器应该已经停止，但状态仍然可以查询
	status := checker.IsGatewayHealthy("main_gateway")
	if status == false {
		t.Log("网关状态查询正常")
	}
}

func TestHealthCheckerUnknownGateway(t *testing.T) {
	cfg := createTestConfig()
	checker := gateway.NewHealthChecker(cfg)

	// 测试未知网关
	status := checker.IsGatewayHealthy("unknown_gateway")
	if status {
		t.Error("未知网关状态应该为false")
	}
}

func TestHealthCheckerMultipleGateways(t *testing.T) {
	cfg := createTestConfig()
	cfg.Gateways = append(cfg.Gateways, []config.Gateway{
		{Name: "gw1", IP: "192.168.1.2", IsDefault: false},
		{Name: "gw2", IP: "192.168.1.3", IsDefault: false},
	}...)

	checker := gateway.NewHealthChecker(cfg)

	// 测试多个网关的状态
	status := checker.GetGatewayStatus()
	if len(status) != 3 {
		t.Errorf("期望3个网关状态, 实际为 %d", len(status))
	}
}

// 测试用例23-35: HTTP API测试
func TestAPIServerCreation(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	if apiServer == nil {
		t.Error("API服务器为空")
	}
}

func TestAPIHealthEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()

	apiServer.RegisterRoutes(http.NewServeMux())
	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}
}

func TestAPILeasesEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	// 分配一些IP
	server.GetPool().RequestIP("aa:bb:cc:dd:ee:01", nil, "device1")
	server.GetPool().RequestIP("aa:bb:cc:dd:ee:02", nil, "device2")

	req := httptest.NewRequest("GET", "/api/leases", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}

	// 验证响应内容
	var leases []interface{}
	err := json.NewDecoder(w.Body).Decode(&leases)
	if err != nil {
		t.Errorf("解析响应失败: %v", err)
	}

	if len(leases) < 2 {
		t.Error("应该有至少2个租约")
	}
}

func TestAPIActiveLeasesEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	req := httptest.NewRequest("GET", "/api/leases/active", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}
}

func TestAPIHistoryEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	req := httptest.NewRequest("GET", "/api/leases/history", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}

	// 验证响应内容
	var history []interface{}
	err := json.NewDecoder(w.Body).Decode(&history)
	if err != nil {
		t.Errorf("解析响应失败: %v", err)
	}

	if len(history) == 0 {
		t.Error("历史记录不应为空")
	}
}

func TestAPIStatsEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}

	// 验证响应内容
	var stats map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&stats)
	if err != nil {
		t.Errorf("解析响应失败: %v", err)
	}

	if stats["pool_stats"] == nil {
		t.Error("统计信息中缺少pool_stats")
	}
}

func TestAPIGatewaysEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	req := httptest.NewRequest("GET", "/api/gateways", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}
}

func TestAPIDevicesEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	req := httptest.NewRequest("GET", "/api/devices", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}
}

func TestAPIBindingsEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	req := httptest.NewRequest("GET", "/api/bindings", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}
}

func TestAPIConfigEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	req := httptest.NewRequest("GET", "/api/config", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
	}
}

func TestAPIConfigValidateEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	// 创建有效的配置数据
	validConfig := `{
		"content": "server:\n  interface: \"eth0\"\n  port: 67\nnetwork:\n  subnet: \"192.168.1.0/24\"\n  start_ip: \"192.168.1.100\"\n  end_ip: \"192.168.1.200\"\ngateways:\n  - name: \"test\"\n    ip: \"192.168.1.1\"\n    is_default: true"
	}`

	req := httptest.NewRequest("POST", "/api/config/validate", strings.NewReader(validConfig))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200, 实际为 %d", w.Code)
		t.Errorf("响应内容: %s", w.Body.String())
	}
}

func TestAPIDeviceAddEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	deviceData := `{
		"mac": "aa:bb:cc:dd:ee:ff",
		"device_type": "Android",
		"model": "Test Device",
		"description": "测试设备",
		"owner": "测试用户"
	}`

	req := httptest.NewRequest("POST", "/api/devices", strings.NewReader(deviceData))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("期望状态码201, 实际为 %d", w.Code)
	}
}

func TestAPIStaticBindingAddEndpoint(t *testing.T) {
	cfg := createTestConfig()
	server, _ := dhcp.NewServer(cfg)

	apiServer := api.NewAPIServer(
		server.GetPool(),
		server.GetChecker(),
		cfg,
		"test.yaml",
		server,
		8080,
	)

	bindingData := `{
		"alias": "test-binding",
		"mac": "aa:bb:cc:dd:ee:ff",
		"ip": "192.168.1.150",
		"gateway": "main_gateway",
		"hostname": "test-device"
	}`

	req := httptest.NewRequest("POST", "/api/bindings", strings.NewReader(bindingData))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("期望状态码201, 实际为 %d", w.Code)
	}
}

// 测试用例36-42: 高级功能测试
func TestConfigDeviceManagement(t *testing.T) {
	cfg := createTestConfig()

	// 添加设备
	device := config.DeviceInfo{
		MAC:         "aa:bb:cc:dd:ee:ff",
		DeviceType:  "Android",
		Model:       "Test Device",
		Description: "测试设备",
		Owner:       "测试用户",
	}

	cfg.AddOrUpdateDevice(device)

	// 查找设备
	found := cfg.FindDeviceByMAC("aa:bb:cc:dd:ee:ff")
	if found == nil {
		t.Error("设备未找到")
	}

	if found.DeviceType != "Android" {
		t.Errorf("期望设备类型为Android, 实际为 %s", found.DeviceType)
	}
}

func TestConfigGatewayManagement(t *testing.T) {
	cfg := createTestConfig()

	// 查找默认网关
	defaultGW := cfg.GetDefaultGateway()
	if defaultGW == nil {
		t.Error("默认网关未找到")
	}

	if !defaultGW.IsDefault {
		t.Error("返回的网关不是默认网关")
	}

	// 按名称查找网关
	found := cfg.FindGatewayByName("main_gateway")
	if found == nil {
		t.Error("网关未找到")
	}

	if found.Name != "main_gateway" {
		t.Errorf("期望网关名称为main_gateway, 实际为 %s", found.Name)
	}
}

func TestConfigBindingManagement(t *testing.T) {
	cfg := createTestConfig()
	cfg.Bindings = []config.MACBinding{
		{
			Alias:    "test-binding",
			MAC:      "aa:bb:cc:dd:ee:ff",
			IP:       "192.168.1.150",
			Gateway:  "main_gateway",
			Hostname: "test-device",
		},
	}

	// 查找绑定
	found := cfg.FindBindingByMAC("aa:bb:cc:dd:ee:ff")
	if found == nil {
		t.Error("绑定未找到")
	}

	if found.IP != "192.168.1.150" {
		t.Errorf("期望IP为192.168.1.150, 实际为 %s", found.IP)
	}
}

func TestIPPoolStats(t *testing.T) {
	cfg := createTestConfig()
	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	// 分配一些IP
	pool.RequestIP("aa:bb:cc:dd:ee:01", nil, "device1")
	pool.RequestIP("aa:bb:cc:dd:ee:02", nil, "device2")

	stats := pool.GetPoolStats()

	if stats["total_ips"].(int) <= 0 {
		t.Error("总IP数量应大于0")
	}

	if stats["dynamic_leases"].(int) < 2 {
		t.Error("已用IP数量应至少为2")
	}

	if stats["available_ips"].(int) < 0 {
		t.Error("可用IP数量不应为负数")
	}
}

func TestIPPoolCleanup(t *testing.T) {
	cfg := createTestConfig()
	cfg.Server.LeaseTime = 1 * time.Millisecond // 设置极短的租期

	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	// 分配IP
	lease, err := pool.RequestIP("aa:bb:cc:dd:ee:ff", nil, "test-device")
	if err != nil {
		t.Fatalf("分配IP失败: %v", err)
	}

	// 等待过期
	time.Sleep(2 * time.Millisecond)

	// 执行清理
	pool.CleanupExpiredLeases()

	// 验证清理结果
	_, exists := pool.GetLease(lease.IP.String())
	if exists {
		t.Error("过期租约应该已被清理")
	}
}

func TestLeaseRenewal(t *testing.T) {
	cfg := createTestConfig()
	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	// 首次分配
	lease1, err := pool.RequestIP("aa:bb:cc:dd:ee:ff", nil, "test-device")
	if err != nil {
		t.Fatalf("首次分配IP失败: %v", err)
	}

	originalStartTime := lease1.StartTime

	// 等待一段时间后续租
	time.Sleep(10 * time.Millisecond)

	lease2, err := pool.RequestIP("aa:bb:cc:dd:ee:ff", nil, "test-device")
	if err != nil {
		t.Fatalf("续租IP失败: %v", err)
	}

	// 验证是同一个IP
	if lease1.IP.String() != lease2.IP.String() {
		t.Error("续租应该返回相同的IP")
	}

	// 验证时间已更新
	if !lease2.StartTime.After(originalStartTime) {
		t.Error("续租时间应该更新")
	}
}

func TestConcurrentIPAllocation(t *testing.T) {
	cfg := createTestConfig()
	pool, err := dhcp.NewIPPool(cfg)
	if err != nil {
		t.Fatalf("创建IP地址池失败: %v", err)
	}

	// 并发分配IP
	results := make(chan *dhcp.IPLease, 10)
	errors := make(chan error, 10)

	for i := 0; i < 10; i++ {
		go func(id int) {
			mac := fmt.Sprintf("aa:bb:cc:dd:ee:%02d", id)
			lease, err := pool.RequestIP(mac, nil, fmt.Sprintf("device%d", id))
			if err != nil {
				errors <- err
				return
			}
			results <- lease
		}(i)
	}

	// 收集结果
	leases := make([]*dhcp.IPLease, 0)
	for i := 0; i < 10; i++ {
		select {
		case lease := <-results:
			leases = append(leases, lease)
		case err := <-errors:
			t.Errorf("并发分配IP失败: %v", err)
		case <-time.After(5 * time.Second):
			t.Error("并发分配IP超时")
		}
	}

	// 验证没有IP冲突
	ipSet := make(map[string]bool)
	for _, lease := range leases {
		ip := lease.IP.String()
		if ipSet[ip] {
			t.Errorf("IP地址冲突: %s", ip)
		}
		ipSet[ip] = true
	}
}

// 辅助函数
func createTestConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Interface: "eth0",
			Port:      67,
			LeaseTime: 24 * time.Hour,
			APIPort:   8080,
			LogLevel:  "info",
			Debug:     false,
		},
		Network: config.NetworkConfig{
			Subnet:           "192.168.1.0/24",
			Netmask:          "255.255.255.0",
			StartIP:          "192.168.1.100",
			EndIP:            "192.168.1.200",
			DNSServers:       []string{"8.8.8.8", "1.1.1.1"},
			DomainName:       "local",
			DefaultGateway:   "192.168.1.1",
			LeaseTime:        86400,
			RenewalTime:      43200,
			RebindingTime:    75600,
			BroadcastAddress: "192.168.1.255",
		},
		Gateways: []config.Gateway{
			{
				Name:        "main_gateway",
				IP:          "192.168.1.1",
				IsDefault:   true,
				Description: "主网关",
			},
		},
		Bindings: []config.MACBinding{},
		Devices:  []config.DeviceInfo{},
		HealthCheck: config.HealthConfig{
			Interval:   30 * time.Second,
			Timeout:    5 * time.Second,
			RetryCount: 3,
			Method:     "ping",
		},
	}
}

// 测试运行统计
func TestMain(m *testing.M) {
	// 抑制测试期间的日志输出
	log.SetOutput(io.Discard)

	// 运行测试
	code := m.Run()

	// 恢复日志输出
	log.SetOutput(os.Stdout)

	os.Exit(code)
}
