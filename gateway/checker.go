package gateway

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"dhcp-server/config"
)

// HealthChecker 网关健康检查器
type HealthChecker struct {
	config     *config.Config
	status     map[string]bool // 网关状态缓存
	statusLock sync.RWMutex
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewHealthChecker 创建健康检查器
func NewHealthChecker(cfg *config.Config) *HealthChecker {
	ctx, cancel := context.WithCancel(context.Background())

	hc := &HealthChecker{
		config: cfg,
		status: make(map[string]bool),
		ctx:    ctx,
		cancel: cancel,
	}

	// 初始化所有网关为健康状态
	for _, gw := range cfg.Gateways {
		hc.status[gw.Name] = true
	}

	return hc
}

// Start 启动健康检查
func (hc *HealthChecker) Start() {
	log.Println("启动网关健康检查服务...")

	// 立即执行一次检查
	hc.checkAllGateways()

	// 定时检查
	ticker := time.NewTicker(hc.config.HealthCheck.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-hc.ctx.Done():
			log.Println("网关健康检查服务已停止")
			return
		case <-ticker.C:
			hc.checkAllGateways()
		}
	}
}

// Stop 停止健康检查
func (hc *HealthChecker) Stop() {
	log.Println("正在停止网关健康检查服务...")
	hc.cancel()
}

// IsGatewayHealthy 检查网关是否健康
func (hc *HealthChecker) IsGatewayHealthy(gatewayName string) bool {
	hc.statusLock.RLock()
	defer hc.statusLock.RUnlock()

	if status, exists := hc.status[gatewayName]; exists {
		return status
	}
	return false
}

// GetHealthyGateway 获取健康的网关，优先返回指定网关，不健康则返回默认网关
func (hc *HealthChecker) GetHealthyGateway(preferredGateway string) *config.Gateway {
	// 如果首选网关健康，返回首选网关
	if preferredGateway != "" && hc.IsGatewayHealthy(preferredGateway) {
		return hc.config.FindGatewayByName(preferredGateway)
	}

	// 否则返回健康的默认网关
	defaultGW := hc.config.GetDefaultGateway()
	if defaultGW != nil && hc.IsGatewayHealthy(defaultGW.Name) {
		return defaultGW
	}

	// 如果默认网关也不健康，返回任何一个健康的网关
	for _, gw := range hc.config.Gateways {
		if hc.IsGatewayHealthy(gw.Name) {
			return &gw
		}
	}

	// 如果所有网关都不健康，返回默认网关（降级处理）
	log.Printf("警告: 所有网关都不健康，返回默认网关: %s", defaultGW.IP)
	return defaultGW
}

// checkAllGateways 检查所有网关
func (hc *HealthChecker) checkAllGateways() {
	log.Println("开始检查所有网关健康状态...")

	var wg sync.WaitGroup
	results := make(chan gatewayResult, len(hc.config.Gateways))

	// 并发检查所有网关
	for _, gw := range hc.config.Gateways {
		wg.Add(1)
		go func(gateway config.Gateway) {
			defer wg.Done()
			healthy := hc.checkSingleGateway(gateway.IP)
			results <- gatewayResult{
				name:    gateway.Name,
				healthy: healthy,
			}
		}(gw)
	}

	// 等待所有检查完成
	go func() {
		wg.Wait()
		close(results)
	}()

	// 更新状态
	hc.statusLock.Lock()
	defer hc.statusLock.Unlock()

	for result := range results {
		oldStatus := hc.status[result.name]
		hc.status[result.name] = result.healthy

		if oldStatus != result.healthy {
			status := "健康"
			if !result.healthy {
				status = "不健康"
			}
			log.Printf("网关 %s 状态变更为: %s", result.name, status)
		}
	}

	hc.logCurrentStatus()
}

type gatewayResult struct {
	name    string
	healthy bool
}

// checkSingleGateway 检查单个网关
func (hc *HealthChecker) checkSingleGateway(gatewayIP string) bool {
	retryCount := hc.config.HealthCheck.RetryCount
	if retryCount < 1 {
		retryCount = 1
	}

	for i := 0; i < retryCount; i++ {
		if i > 0 {
			time.Sleep(time.Second) // 重试间隔
		}

		switch hc.config.HealthCheck.Method {
		case "ping":
			if hc.pingCheck(gatewayIP) {
				return true
			}
		case "tcp":
			if hc.tcpCheck(gatewayIP, hc.config.HealthCheck.TCPPort) {
				return true
			}
		case "http":
			if hc.httpCheck(gatewayIP, hc.config.HealthCheck.HTTPPath) {
				return true
			}
		default:
			// 默认使用ping检查
			if hc.pingCheck(gatewayIP) {
				return true
			}
		}
	}

	return false
}

// pingCheck ping检查
func (hc *HealthChecker) pingCheck(ip string) bool {
	ctx, cancel := context.WithTimeout(hc.ctx, hc.config.HealthCheck.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "3000", ip)
	err := cmd.Run()
	return err == nil
}

// tcpCheck TCP端口检查
func (hc *HealthChecker) tcpCheck(ip string, port int) bool {
	address := fmt.Sprintf("%s:%d", ip, port)

	conn, err := net.DialTimeout("tcp", address, hc.config.HealthCheck.Timeout)
	if err != nil {
		return false
	}
	defer conn.Close()

	return true
}

// httpCheck HTTP检查
func (hc *HealthChecker) httpCheck(ip string, path string) bool {
	client := &http.Client{
		Timeout: hc.config.HealthCheck.Timeout,
	}

	url := fmt.Sprintf("http://%s%s", ip, path)
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// logCurrentStatus 打印当前状态
func (hc *HealthChecker) logCurrentStatus() {
	log.Println("当前网关健康状态:")
	for _, gw := range hc.config.Gateways {
		status := "不健康"
		if hc.status[gw.Name] {
			status = "健康"
		}
		log.Printf("  %s (%s): %s", gw.Name, gw.IP, status)
	}
}

// GetGatewayStatus 获取所有网关状态（用于API或监控）
func (hc *HealthChecker) GetGatewayStatus() map[string]bool {
	hc.statusLock.RLock()
	defer hc.statusLock.RUnlock()

	status := make(map[string]bool)
	for k, v := range hc.status {
		status[k] = v
	}
	return status
}
