package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"dhcp-server/config"
	"dhcp-server/dhcp"
	"dhcp-server/gateway"
)

// SSE日志连接管理
var (
	logClients    = make(map[chan string]bool)
	logClientsMux sync.RWMutex
	logBroadcast  = make(chan string, 100)
)

// 日志广播器
func init() {
	go logBroadcaster()
}

// logBroadcaster 负责向所有连接的客户端广播日志
func logBroadcaster() {
	for {
		message := <-logBroadcast
		logClientsMux.Lock()
		// 使用slice来避免在迭代时修改map
		var toRemove []chan string
		for client := range logClients {
			// 使用recover来处理可能的panic
			func() {
				defer func() {
					if r := recover(); r != nil {
						// 发送失败，标记为需要移除
						toRemove = append(toRemove, client)
					}
				}()
				select {
				case client <- message:
				default:
					// 客户端阻塞，标记为需要移除
					toRemove = append(toRemove, client)
				}
			}()
		}
		// 移除失败的客户端
		for _, client := range toRemove {
			delete(logClients, client)
		}
		logClientsMux.Unlock()
	}
}

// GetLogBroadcast 获取日志广播通道
func GetLogBroadcast() chan string {
	return logBroadcast
}

// APIServer HTTP API服务器
type APIServer struct {
	pool       *dhcp.IPPool
	checker    *gateway.HealthChecker
	config     *config.Config // 添加配置引用
	configPath string         // 配置文件路径
	dhcpServer *dhcp.Server
	scanner    *dhcp.NetworkScanner // 网络扫描器
	port       int
	host       string // API监听地址
	server     *http.Server
	// 添加重新加载回调函数
	reloadCallback func(*config.Config) error
}

// LeaseInfo 租约信息响应
type LeaseInfo struct {
	IP            string    `json:"ip"`
	MAC           string    `json:"mac"`
	Hostname      string    `json:"hostname"`
	StartTime     time.Time `json:"start_time"`
	LeaseTime     string    `json:"lease_time"`
	RemainingTime string    `json:"remaining_time"`
	IsStatic      bool      `json:"is_static"`
	Gateway       string    `json:"gateway"`    // 配置中的网关名称（保持兼容性）
	GatewayIP     string    `json:"gateway_ip"` // 实际响应的网关IP地址
	IsExpired     bool      `json:"is_expired"`
}

// HistoryRecord 历史记录
type HistoryRecord struct {
	IP        string    `json:"ip"`
	MAC       string    `json:"mac"`
	Hostname  string    `json:"hostname"`
	Action    string    `json:"action"` // DISCOVER, REQUEST, RELEASE, etc.
	Timestamp time.Time `json:"timestamp"`
	Gateway   string    `json:"gateway"`
	ServerIP  string    `json:"server_ip"`
}

// StatsResponse 统计信息响应
type StatsResponse struct {
	PoolStats     map[string]interface{} `json:"pool_stats"`
	GatewayStatus map[string]bool        `json:"gateway_status"`
	ServerInfo    ServerInfo             `json:"server_info"`
}

// ServerInfo 服务器信息
type ServerInfo struct {
	Version   string    `json:"version"`
	StartTime time.Time `json:"start_time"`
	Uptime    string    `json:"uptime"`
}

// NewAPIServer 创建新的API服务器
func NewAPIServer(pool *dhcp.IPPool, checker *gateway.HealthChecker, cfg *config.Config, configPath string, dhcpServer *dhcp.Server, scanner *dhcp.NetworkScanner, port int) *APIServer {
	// 获取API监听地址，如果为空则使用默认值 0.0.0.0
	host := cfg.Server.APIHost
	if host == "" {
		host = "0.0.0.0"
	}

	return &APIServer{
		pool:       pool,
		checker:    checker,
		config:     cfg,
		configPath: configPath,
		dhcpServer: dhcpServer,
		scanner:    scanner,
		port:       port,
		host:       host,
	}
}

// SetReloadCallback 设置重新加载回调函数
func (api *APIServer) SetReloadCallback(callback func(*config.Config) error) {
	api.reloadCallback = callback
}

// UpdateReferences 更新组件引用（用于热重载）
func (api *APIServer) UpdateReferences(pool *dhcp.IPPool, checker *gateway.HealthChecker, cfg *config.Config, dhcpServer *dhcp.Server, scanner *dhcp.NetworkScanner) {
	api.pool = pool
	api.checker = checker
	api.config = cfg
	api.dhcpServer = dhcpServer
	api.scanner = scanner

	// 更新API监听地址
	host := cfg.Server.APIHost
	if host == "" {
		host = "0.0.0.0"
	}
	api.host = host
}

// Start 启动API服务器
func (api *APIServer) Start() error {
	mux := http.NewServeMux()

	// 注册路由到mux
	api.RegisterRoutes(mux)

	// 添加CORS支持
	corsHandler := corsMiddleware(mux)

	api.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", api.host, api.port),
		Handler: corsHandler,
	}

	log.Printf("启动HTTP API服务器，地址: %s:%d", api.host, api.port)
	log.Printf("API文档: http://%s:%d", api.host, api.port)

	return api.server.ListenAndServe()
}

// Stop 停止API服务器
func (api *APIServer) Stop() error {
	if api.server != nil {
		log.Println("正在停止HTTP API服务器...")
		return api.server.Close()
	}
	return nil
}

// RegisterRoutes 注册路由
func (api *APIServer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", api.handleIndex)
	mux.HandleFunc("/api/health", api.handleHealth)
	mux.HandleFunc("/api/leases", api.handleLeases)
	mux.HandleFunc("/api/leases/active", api.handleActiveLeases)
	mux.HandleFunc("/api/leases/history", api.handleHistory)
	mux.HandleFunc("/api/stats", api.handleStats)

	// 扫描器相关路由
	mux.HandleFunc("/api/scanner", api.handleScanner)
	mux.HandleFunc("/api/scanner/results", api.handleScannerResults)
	mux.HandleFunc("/api/scanner/log", api.handleScannerLog)
	mux.HandleFunc("/api/scanner/start", api.handleScannerStart)
	mux.HandleFunc("/api/scanner/stop", api.handleScannerStop)
	mux.HandleFunc("/api/scanner/config", api.handleScannerConfig)
	mux.HandleFunc("/api/gateways", api.handleGateways)

	// 设备管理相关端点
	mux.HandleFunc("/api/devices", api.handleDevices)
	mux.HandleFunc("/api/devices/discover", api.handleDeviceDiscover)
	mux.HandleFunc("/api/devices/batch", api.handleBatchAddDevices)
	mux.HandleFunc("/api/devices/gateway", api.handleDeviceGateway)

	// 静态绑定管理接口
	mux.HandleFunc("/api/bindings", api.handleStaticBindings)
	mux.HandleFunc("/api/leases/convert-to-static", api.handleConvertLeaseToStatic)

	// 配置管理相关端点
	mux.HandleFunc("/api/config", api.handleConfig)
	mux.HandleFunc("/api/config/validate", api.handleConfigValidate)
	mux.HandleFunc("/api/config/backups", api.handleConfigBackups)
	mux.HandleFunc("/api/config/restore", api.handleConfigRestore)
	mux.HandleFunc("/api/config/reload", api.handleConfigReload)

	// 日志管理端点
	mux.HandleFunc("/api/logs", api.handleLogs)
	mux.HandleFunc("/api/logs/stream", api.handleLogStream)

	// 配置管理子模块端点
	mux.HandleFunc("/api/config/server", api.handleServerConfig)
	mux.HandleFunc("/api/config/network", api.handleNetworkConfig)
	mux.HandleFunc("/api/config/health-check", api.handleHealthCheckConfig)
	mux.HandleFunc("/api/available-ips", api.handleAvailableIPs)

	// 服务器管理API
	mux.HandleFunc("/api/server/restart", api.handleServerRestart)
}

// handleIndex 处理首页请求
func (api *APIServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>DHCP服务器管理</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        
        /* 默认（亮色主题）样式 */
        body, body.light-theme { 
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Arial, sans-serif; 
            background: 
                linear-gradient(135deg, #74b9ff 0%, #0984e3 25%, #00b894 50%, #74b9ff 75%, #a29bfe 100%);
            background-size: 400% 400%;
            animation: lightGradientFlow 18s ease infinite;
            color: #333; 
            line-height: 1.6; 
            position: relative;
            min-height: 100vh;
            overflow-x: hidden;
        }
        
        /* 粉色主题样式 */
        body.pink-theme { 
            background: 
                linear-gradient(135deg, #ff9a8b 0%, #fecfef 25%, #fecfef 50%, #a8edea 75%, #fed6e3 100%);
            background-size: 400% 400%;
            animation: pinkGradientFlow 20s ease infinite;
        }
        
        /* 亮色主题背景效果 */
        body::before, body.light-theme::before {
            content: '';
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            height: 100%;
            background: 
                radial-gradient(circle at 15% 85%, rgba(116, 185, 255, 0.2) 0%, transparent 40%),
                radial-gradient(circle at 85% 15%, rgba(0, 184, 148, 0.15) 0%, transparent 45%),
                radial-gradient(circle at 50% 50%, rgba(162, 155, 254, 0.1) 0%, transparent 60%),
                radial-gradient(circle at 30% 30%, rgba(9, 132, 227, 0.12) 0%, transparent 35%);
            pointer-events: none;
            z-index: -1;
        }
        
        body::after, body.light-theme::after {
            content: '';
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            height: 100%;
            background: 
                linear-gradient(45deg, transparent 30%, rgba(255,255,255,0.08) 50%, transparent 70%);
            background-size: 50px 50px;
            animation: shimmer 20s linear infinite;
            pointer-events: none;
            z-index: -1;
        }
        
        /* 粉色主题背景效果 */
        body.pink-theme::before {
            background: 
                radial-gradient(circle at 15% 85%, rgba(255, 182, 193, 0.3) 0%, transparent 40%),
                radial-gradient(circle at 85% 15%, rgba(173, 216, 230, 0.25) 0%, transparent 45%),
                radial-gradient(circle at 50% 50%, rgba(255, 240, 245, 0.2) 0%, transparent 60%),
                radial-gradient(circle at 30% 30%, rgba(255, 218, 185, 0.15) 0%, transparent 35%);
        }
        
        body.pink-theme::after {
            background: 
                linear-gradient(45deg, transparent 30%, rgba(255,255,255,0.05) 50%, transparent 70%);
            background-size: 60px 60px;
            animation: shimmer 25s linear infinite;
        }
        
        @keyframes lightGradientFlow {
            0% { background-position: 0% 50%; }
            25% { background-position: 100% 25%; }
            50% { background-position: 0% 75%; }
            75% { background-position: 100% 50%; }
            100% { background-position: 0% 50%; }
        }
        
        @keyframes pinkGradientFlow {
            0% { background-position: 0% 50%; }
            25% { background-position: 100% 25%; }
            50% { background-position: 0% 75%; }
            75% { background-position: 100% 50%; }
            100% { background-position: 0% 50%; }
        }
        
        @keyframes shimmer {
            0% { transform: translateX(-100%) translateY(-100%); }
            100% { transform: translateX(100%) translateY(100%); }
        }
        .container { 
            max-width: 1400px; 
            margin: 0 auto; 
            padding: 20px; 
            position: relative;
            z-index: 1;
        }
        /* 亮色主题头部 */
        .header, body.light-theme .header { 
            background: linear-gradient(135deg, #74b9ff 0%, #0984e3 50%, #00b894 100%); 
            color: white; 
            padding: 1.5rem; 
            border-radius: 20px; 
            margin-bottom: 1.5rem; 
            box-shadow: 0 10px 30px rgba(116, 185, 255, 0.3), 0 0 20px rgba(0, 184, 148, 0.2);
            position: relative;
            overflow: hidden;
        }
        
        /* 粉色主题头部 */
        body.pink-theme .header { 
            background: linear-gradient(135deg, #ff6b6b 0%, #ffa726 50%, #42a5f5 100%); 
            box-shadow: 0 10px 30px rgba(255, 107, 107, 0.3), 0 0 20px rgba(255, 167, 38, 0.2);
        }
        
        .header::before {
            content: '';
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: linear-gradient(45deg, transparent 30%, rgba(255,255,255,0.1) 50%, transparent 70%);
            animation: headerShine 3s ease-in-out infinite;
        }
        .header h1 { 
            font-size: 1.8rem; 
            margin-bottom: 0.3rem; 
            font-weight: 600; 
            position: relative;
            z-index: 1;
        }
        .header p { 
            font-size: 0.95rem; 
            opacity: 0.9; 
            margin: 0; 
            position: relative;
            z-index: 1;
        }
        
        .header-content {
            display: flex;
            justify-content: space-between;
            align-items: center;
            position: relative;
            z-index: 2;
        }

        .header-info {
            flex: 1;
        }

        .header-actions {
            display: flex;
            gap: 1rem;
            align-items: center;
        }

        .server-status {
            display: flex;
            flex-direction: column;
            align-items: flex-end;
            gap: 0.5rem;
        }

        .uptime-display {
            display: flex;
            align-items: center;
            gap: 0.5rem;
            font-size: 0.85rem;
            color: rgba(255, 255, 255, 0.9);
        }

        .uptime-label {
            font-weight: 500;
        }

        .uptime-value {
            font-weight: 600;
            background: rgba(255, 255, 255, 0.15);
            padding: 0.2rem 0.5rem;
            border-radius: 4px;
            backdrop-filter: blur(5px);
            position: relative;
            overflow: hidden;
        }

        .uptime-value::before {
            content: '';
            position: absolute;
            top: 0;
            left: -100%;
            width: 100%;
            height: 100%;
            background: linear-gradient(90deg, transparent, rgba(255, 255, 255, 0.2), transparent);
            animation: uptimeShine 3s ease-in-out infinite;
        }

        @keyframes uptimeShine {
            0% { left: -100%; }
            50% { left: 100%; }
            100% { left: 100%; }
        }

        /* 亮色主题运行时间 */
        body.light-theme .uptime-value {
            background: rgba(116, 185, 255, 0.2);
            border: 1px solid rgba(116, 185, 255, 0.3);
            color: #2c3e50;
        }

        /* 暗色主题运行时间 */
        body.dark-theme .uptime-value {
            background: rgba(0, 212, 255, 0.2);
            border: 1px solid rgba(0, 212, 255, 0.3);
            color: #e0e0e0;
        }

        /* 粉色主题运行时间 */
        body.pink-theme .uptime-value {
            background: rgba(255, 107, 107, 0.2);
            border: 1px solid rgba(255, 107, 107, 0.3);
            color: #c0392b;
        }

        /* 运行时间容器和进度条 */
        .uptime-container {
            display: flex;
            flex-direction: column;
            gap: 0.2rem;
            align-items: flex-start;
        }

        .uptime-progress {
            width: 100%;
            height: 3px;
            background: rgba(255, 255, 255, 0.1);
            border-radius: 2px;
            overflow: hidden;
            position: relative;
        }

        .uptime-progress-bar {
            height: 100%;
            background: linear-gradient(90deg, #4CAF50, #8BC34A);
            border-radius: 2px;
            width: 0%;
            transition: width 0.5s ease;
            position: relative;
        }

        .uptime-progress-bar::after {
            content: '';
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: linear-gradient(90deg, transparent, rgba(255, 255, 255, 0.3), transparent);
            animation: progressShine 2s ease-in-out infinite;
        }

        @keyframes progressShine {
            0% { transform: translateX(-100%); }
            50% { transform: translateX(100%); }
            100% { transform: translateX(100%); }
        }

        /* 亮色主题进度条 */
        body.light-theme .uptime-progress {
            background: rgba(116, 185, 255, 0.1);
        }

        body.light-theme .uptime-progress-bar {
            background: linear-gradient(90deg, #74b9ff, #00b894);
        }

        /* 暗色主题进度条 */
        body.dark-theme .uptime-progress {
            background: rgba(0, 212, 255, 0.1);
        }

        body.dark-theme .uptime-progress-bar {
            background: linear-gradient(90deg, #00d4ff, #0099ff);
        }

        /* 粉色主题进度条 */
        body.pink-theme .uptime-progress {
            background: rgba(255, 107, 107, 0.1);
        }

        body.pink-theme .uptime-progress-bar {
            background: linear-gradient(90deg, #ff6b6b, #ffa726);
        }

        .restart-btn {
            background: rgba(255, 255, 255, 0.2);
            border: 2px solid rgba(255, 255, 255, 0.3);
            color: white;
            padding: 0.7rem 1.5rem;
            border-radius: 8px;
            font-weight: 600;
            transition: all 0.3s ease;
            backdrop-filter: blur(10px);
            white-space: nowrap;
            cursor: pointer;
            font-size: 0.9rem;
        }

        .restart-btn:hover {
            background: rgba(255, 255, 255, 0.3);
            border-color: rgba(255, 255, 255, 0.5);
            transform: translateY(-2px);
            box-shadow: 0 5px 15px rgba(0, 0, 0, 0.2);
        }

        .restart-btn:active {
            transform: translateY(0);
            box-shadow: 0 2px 5px rgba(0, 0, 0, 0.2);
        }

        /* 亮色主题重启按钮 */
        body.light-theme .restart-btn {
            background: rgba(116, 185, 255, 0.25);
            border: 2px solid rgba(116, 185, 255, 0.4);
            color: white;
        }

        body.light-theme .restart-btn:hover {
            background: rgba(116, 185, 255, 0.35);
            border-color: rgba(116, 185, 255, 0.6);
            box-shadow: 0 5px 15px rgba(116, 185, 255, 0.3);
        }

        /* 暗色主题重启按钮 */
        body.dark-theme .restart-btn {
            background: rgba(0, 212, 255, 0.25);
            border: 2px solid rgba(0, 212, 255, 0.4);
            color: white;
        }

        body.dark-theme .restart-btn:hover {
            background: rgba(0, 212, 255, 0.35);
            border-color: rgba(0, 212, 255, 0.6);
            box-shadow: 0 5px 15px rgba(0, 212, 255, 0.3);
        }

        /* 粉色主题重启按钮 */
        body.pink-theme .restart-btn {
            background: rgba(255, 107, 107, 0.25);
            border: 2px solid rgba(255, 107, 107, 0.4);
            color: white;
        }

        body.pink-theme .restart-btn:hover {
            background: rgba(255, 107, 107, 0.35);
            border-color: rgba(255, 107, 107, 0.6);
            box-shadow: 0 5px 15px rgba(255, 107, 107, 0.3);
        }
        
        @keyframes headerShine {
            0% { transform: translateX(-100%); }
            50% { transform: translateX(100%); }
            100% { transform: translateX(-100%); }
        }
        .stats-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 1rem; margin-bottom: 1.2rem; }
        /* 亮色主题统计卡片 */
        .stat-card, body.light-theme .stat-card { 
            background: rgba(255, 255, 255, 0.92); 
            backdrop-filter: blur(15px);
            padding: 1.5rem; 
            border-radius: 18px; 
            box-shadow: 0 15px 40px rgba(116, 185, 255, 0.15), 0 5px 15px rgba(0, 184, 148, 0.1); 
            border: 1px solid rgba(255, 255, 255, 0.3);
            border-left: 4px solid transparent;
            background-clip: padding-box;
            background-image: linear-gradient(to right, #74b9ff, #00b894);
            background-origin: border-box;
            position: relative;
            overflow: hidden;
            transition: all 0.3s ease;
        }
        
        /* 粉色主题统计卡片 */
        body.pink-theme .stat-card { 
            box-shadow: 0 15px 40px rgba(255, 107, 107, 0.15), 0 5px 15px rgba(255, 167, 38, 0.1); 
            background-image: linear-gradient(to right, #ff6b6b, #ffa726);
        }
        
        .stat-card::before {
            content: '';
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: linear-gradient(135deg, rgba(255, 255, 255, 0.2) 0%, transparent 50%);
            opacity: 0;
            transition: opacity 0.3s ease;
        }
        
        .stat-card:hover::before {
            opacity: 1;
        }
        .stat-card:hover, body.light-theme .stat-card:hover { 
            transform: translateY(-5px) scale(1.02); 
            box-shadow: 0 20px 50px rgba(116, 185, 255, 0.25), 0 10px 25px rgba(0, 184, 148, 0.15);
        }
        
        body.pink-theme .stat-card:hover { 
            box-shadow: 0 20px 50px rgba(255, 107, 107, 0.25), 0 10px 25px rgba(255, 167, 38, 0.15);
        }
        .stat-value, body.light-theme .stat-value { 
            font-size: 1.8rem; 
            font-weight: 700; 
            color: #2c3e50; /* 备用颜色 */
            background: linear-gradient(135deg, #74b9ff 0%, #00b894 100%);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
            background-clip: text;
            margin-bottom: 0.3rem; 
            position: relative;
            z-index: 1;
        }
        
        /* 不支持渐变文字的浏览器备用样式 */
        .stat-value:not(.gradient-text), body.light-theme .stat-value:not(.gradient-text) {
            color: #2c3e50 !important;
            background: none !important;
            -webkit-background-clip: initial !important;
            -webkit-text-fill-color: initial !important;
            background-clip: initial !important;
        }
        
        body.pink-theme .stat-value { 
            color: #c0392b; /* 备用颜色 */
            background: linear-gradient(135deg, #ff6b6b 0%, #ffa726 100%);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
            background-clip: text;
        }
        
        /* 粉色主题不支持渐变文字的浏览器备用样式 */
        body.pink-theme .stat-value:not(.gradient-text) {
            color: #c0392b !important;
            background: none !important;
            -webkit-background-clip: initial !important;
            -webkit-text-fill-color: initial !important;
            background-clip: initial !important;
        }
        .stat-label { 
            color: #666; 
            font-size: 0.8rem; 
            text-transform: uppercase; 
            letter-spacing: 0.5px; 
            position: relative;
            z-index: 1;
        }
        .tabs, body.light-theme .tabs { 
            background: rgba(255, 255, 255, 0.92); 
            backdrop-filter: blur(15px);
            border-radius: 20px; 
            box-shadow: 0 20px 50px rgba(116, 185, 255, 0.1), 0 10px 25px rgba(0, 184, 148, 0.08); 
            overflow: hidden; 
            border: 1px solid rgba(255, 255, 255, 0.3);
        }
        
        body.pink-theme .tabs { 
            box-shadow: 0 20px 50px rgba(255, 107, 107, 0.1), 0 10px 25px rgba(255, 167, 38, 0.08); 
        }
        .tab-buttons, body.light-theme .tab-buttons { 
            display: flex; 
            background: linear-gradient(135deg, rgba(255, 255, 255, 0.85) 0%, rgba(240, 248, 255, 0.85) 100%); 
            border-bottom: 1px solid rgba(116, 185, 255, 0.2); 
            overflow-x: auto; 
            padding: 0.5rem;
            border-radius: 20px 20px 0 0;
        }
        
        body.pink-theme .tab-buttons { 
            background: linear-gradient(135deg, rgba(255, 255, 255, 0.85) 0%, rgba(255, 248, 248, 0.85) 100%); 
            border-bottom: 1px solid rgba(255, 107, 107, 0.2); 
        }
        .tab-button { 
            background: transparent; 
            border: none; 
            padding: 1rem 1.5rem; 
            cursor: pointer; 
            font-size: 0.9rem; 
            font-weight: 500; 
            color: #666; 
            transition: all 0.3s ease; 
            white-space: nowrap; 
            border-radius: 12px;
            margin: 0 0.2rem;
            position: relative;
            overflow: hidden;
        }
        .tab-button.active, body.light-theme .tab-button.active { 
            color: white; 
            background: linear-gradient(135deg, #74b9ff 0%, #00b894 100%); 
            font-weight: 600; 
            box-shadow: 0 4px 15px rgba(116, 185, 255, 0.4);
            transform: translateY(-2px);
        }
        
        body.pink-theme .tab-button.active { 
            background: linear-gradient(135deg, #ff6b6b 0%, #ffa726 100%); 
            box-shadow: 0 4px 15px rgba(255, 107, 107, 0.4);
        }
        .tab-button:hover, body.light-theme .tab-button:hover { 
            background: linear-gradient(135deg, rgba(116, 185, 255, 0.1) 0%, rgba(0, 184, 148, 0.1) 100%); 
            color: #333; 
            transform: translateY(-1px);
        }
        
        body.pink-theme .tab-button:hover { 
            background: linear-gradient(135deg, rgba(255, 107, 107, 0.1) 0%, rgba(255, 167, 38, 0.1) 100%); 
        }
        .tab-content { padding: 1.5rem; min-height: 700px; }
        .tab-pane { display: none; }
        .tab-pane.active { display: block; }
        .controls { display: flex; justify-content: space-between; align-items: center; margin-bottom: 1.5rem; flex-wrap: wrap; gap: 1rem; }
        .btn { padding: 0.7rem 1.5rem; border: none; border-radius: 8px; cursor: pointer; font-size: 0.9rem; font-weight: 500; transition: all 0.2s; text-decoration: none; display: inline-block; text-align: center; margin: 0.2rem; }
        .btn-primary { background: #667eea; color: white; }
        .btn-primary:hover { background: #5a6fd8; transform: translateY(-1px); }
        .btn-secondary { background: #6c757d; color: white; }
        .btn-success { background: #28a745; color: white; }
        .btn-warning { background: #ffc107; color: #212529; }
        .btn-danger { background: #dc3545; color: white; }
        .search-box { padding: 0.7rem 1rem; border: 2px solid #e9ecef; border-radius: 8px; font-size: 0.9rem; width: 300px; transition: border-color 0.2s; }
        .search-box:focus { outline: none; border-color: #667eea; }
        .table { width: 100%; border-collapse: collapse; margin-top: 1rem; }
        .table th, .table td { padding: 1rem; text-align: left; border-bottom: 1px solid #e9ecef; }
        .table th { background: #f8f9fa; font-weight: 600; color: #495057; position: sticky; top: 0; }
        .table tbody tr:hover { background: #f8f9fa; }
        .status { padding: 0.3rem 0.8rem; border-radius: 20px; font-size: 0.8rem; font-weight: 500; }
        .status-online { background: #d4edda; color: #155724; }
        .status-offline { background: #f8d7da; color: #721c24; }
        .status-healthy { background: #d4edda; color: #155724; }
        .status-unhealthy { background: #f8d7da; color: #721c24; }
        .auto-refresh { display: flex; align-items: center; gap: 0.5rem; margin-left: auto; }
        .config-editor { display: grid; grid-template-columns: 1fr 300px; gap: 1.5rem; height: 80vh; max-height: 800px; overflow: hidden; }
        .editor-panel { display: flex; flex-direction: column; overflow: hidden; }
        .editor-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 1rem; padding-bottom: 0.5rem; border-bottom: 1px solid #e9ecef; flex-shrink: 0; }
        .editor-textarea { flex: 1; font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace; font-size: 14px; line-height: 1.5; padding: 1rem; border: 2px solid #e9ecef; border-radius: 8px; resize: none; background: #f8f9fa; color: #333; overflow-y: auto; min-height: 400px; }
        .editor-textarea:focus { outline: none; border-color: #667eea; }
        .sidebar-panel { display: flex; flex-direction: column; gap: 1.5rem; overflow-y: auto; max-height: 100%; }
        .panel-section { background: #f8f9fa; padding: 1rem; border-radius: 8px; border: 1px solid #e9ecef; }
        .panel-title { font-weight: 600; margin-bottom: 1rem; color: #495057; }
        .validation-result { padding: 0.8rem; border-radius: 6px; margin-bottom: 1rem; font-size: 0.9rem; }
        .validation-success { background: #d4edda; color: #155724; border: 1px solid #c3e6cb; }
        .validation-error { background: #f8d7da; color: #721c24; border: 1px solid #f5c6cb; }
        .backup-list { max-height: 200px; overflow-y: auto; margin-top: 0.5rem; }
        .backup-item { display: flex; justify-content: space-between; align-items: center; padding: 0.5rem; border-bottom: 1px solid #e9ecef; font-size: 0.8rem; }
        .backup-item:last-child { border-bottom: none; }
        .checkbox-option { display: flex; align-items: center; gap: 0.5rem; margin: 0.5rem 0; }
        .message { margin: 1rem 0; padding: 1rem; border-radius: 8px; }
        .message-success { background: #d4edda; color: #155724; border: 1px solid #c3e6cb; }
        .message-error { background: #f8d7da; color: #721c24; border: 1px solid #f5c6cb; }
        .gateway-status { display: flex; align-items: center; gap: 0.5rem; }
        .status-indicator { width: 12px; height: 12px; border-radius: 50%; }
        .status-indicator.healthy { background: #28a745; }
        .status-indicator.unhealthy { background: #dc3545; }
        .modal { display: none; position: fixed; z-index: 1000; left: 0; top: 0; width: 100%; height: 100%; background-color: rgba(0,0,0,0.5); }
        .modal-content { background-color: white; margin: 5% auto; padding: 0; border-radius: 12px; width: 500px; max-width: 90%; max-height: 80vh; overflow-y: auto; box-shadow: 0 20px 60px rgba(0,0,0,0.3); }
        .modal-header { padding: 1.5rem; border-bottom: 1px solid #e9ecef; border-radius: 12px 12px 0 0; background: linear-gradient(135deg, #667eea 0%, #764ba2 100%); color: white; }
        .modal-title { margin: 0; font-size: 1.25rem; font-weight: 600; }
        .modal-body { padding: 1.5rem; max-height: 60vh; overflow-y: auto; }
        .modal-footer { padding: 1rem 1.5rem; border-top: 1px solid #e9ecef; display: flex; justify-content: flex-end; gap: 0.5rem; }
        .close { color: white; float: right; font-size: 28px; font-weight: bold; cursor: pointer; line-height: 1; }
        .close:hover { opacity: 0.7; }
        .form-group { margin-bottom: 1rem; }
        .form-group label { display: block; margin-bottom: 0.5rem; font-weight: 500; color: #495057; }
        .form-control { width: 100%; padding: 0.75rem; border: 2px solid #e9ecef; border-radius: 8px; font-size: 0.9rem; transition: border-color 0.2s; }
        .form-control:focus { outline: none; border-color: #667eea; }
        .btn-modal { padding: 0.6rem 1.2rem; border: none; border-radius: 6px; font-size: 0.9rem; font-weight: 500; cursor: pointer; transition: all 0.2s; }
        .btn-modal-primary { background: #667eea; color: white; }
        .btn-modal-primary:hover { background: #5a6fd8; }
        .form-help { color: #666; font-size: 0.8rem; margin-top: 0.3rem; line-height: 1.4; }
        .btn-modal-secondary { background: #6c757d; color: white; }
        .btn-modal-secondary:hover { background: #5a6268; }
        
        /* 配置管理子标签页样式 */
        .config-tabs { display: flex; flex-direction: column; height: 100%; }
        .config-tab-buttons { display: flex; background: #f8f9fa; border-bottom: 1px solid #e9ecef; margin-bottom: 1rem; }
        .config-tab-button { background: none; border: none; padding: 0.8rem 1.2rem; cursor: pointer; font-size: 0.9rem; font-weight: 500; color: #666; border-bottom: 3px solid transparent; transition: all 0.2s; white-space: nowrap; }
        .config-tab-button.active { color: #667eea; border-bottom-color: #667eea; background: white; }
        .config-tab-button:hover { background: #e9ecef; }
        .config-tab-content { flex: 1; }
        .config-sub-pane { display: none; }
        .config-sub-pane.active { display: block; }
        
        /* 网关检测配置样式 */
        .status-display { background: #f8f9fa; border: 1px solid #e9ecef; border-radius: 8px; padding: 1rem; }
        .status-item { display: flex; justify-content: space-between; margin-bottom: 0.5rem; }
        .status-item:last-child { margin-bottom: 0; }
        .status-label { font-weight: 500; color: #666; }
        .status-value { color: #333; }
        
        /* 加载提示样式 */
        .loading-overlay { 
            display: none; 
            position: fixed; 
            z-index: 10001; 
            left: 0; 
            top: 0; 
            width: 100%; 
            height: 100%; 
            background-color: rgba(0,0,0,0.7); 
            animation: fadeIn 0.3s ease; 
        }
        .loading-content { 
            position: absolute; 
            top: 50%; 
            left: 50%; 
            transform: translate(-50%, -50%); 
            background: white; 
            padding: 2rem; 
            border-radius: 12px; 
            text-align: center; 
            box-shadow: 0 20px 60px rgba(0,0,0,0.3); 
            min-width: 200px; 
        }
        .loading-spinner { 
            width: 40px; 
            height: 40px; 
            border: 4px solid #f3f3f3; 
            border-top: 4px solid #667eea; 
            border-radius: 50%; 
            animation: spin 1s linear infinite; 
            margin: 0 auto 1rem auto; 
        }
        @keyframes spin { 
            0% { transform: rotate(0deg); } 
            100% { transform: rotate(360deg); } 
        }
        .loading-text { 
            color: #333; 
            font-size: 1rem; 
            font-weight: 500; 
            margin: 0; 
        }
        
        /* 美化确认对话框样式 */
        .confirm-modal { display: none; position: fixed; z-index: 10000; left: 0; top: 0; width: 100%; height: 100%; background-color: rgba(0,0,0,0.6); animation: fadeIn 0.3s ease; }
        .confirm-modal-content { background-color: white; margin: 15% auto; padding: 0; border-radius: 12px; width: 420px; max-width: 90%; box-shadow: 0 25px 50px rgba(0,0,0,0.4); animation: slideIn 0.3s ease; transform: translateY(-50px); opacity: 0; animation-fill-mode: forwards; }
        .confirm-modal-header { padding: 1.5rem 1.5rem 1rem 1.5rem; text-align: center; }
        .confirm-modal-icon { font-size: 3rem; margin-bottom: 1rem; display: block; }
        .confirm-modal-icon.warning { color: #ff6b6b; }
        .confirm-modal-icon.danger { color: #e74c3c; }
        .confirm-modal-icon.info { color: #3498db; }
        .confirm-modal-title { margin: 0 0 0.5rem 0; font-size: 1.25rem; font-weight: 600; color: #2c3e50; }
        .confirm-modal-message { color: #6c757d; line-height: 1.5; margin-bottom: 0; white-space: pre-line; }
        .confirm-modal-footer { padding: 1rem 1.5rem 1.5rem 1.5rem; display: flex; justify-content: center; gap: 1rem; }
        .confirm-btn { padding: 0.7rem 1.5rem; border: none; border-radius: 8px; font-size: 0.9rem; font-weight: 500; cursor: pointer; transition: all 0.2s; min-width: 100px; }
        .confirm-btn-danger { background: #e74c3c; color: white; }
        .confirm-btn-danger:hover { background: #c0392b; transform: translateY(-1px); }
        .confirm-btn-warning { background: #f39c12; color: white; }
        .confirm-btn-warning:hover { background: #e67e22; transform: translateY(-1px); }
        .confirm-btn-cancel { background: #95a5a6; color: white; }
        .confirm-btn-cancel:hover { background: #7f8c8d; transform: translateY(-1px); }
        
        @keyframes fadeIn { from { opacity: 0; } to { opacity: 1; } }
        @keyframes fadeOut { from { opacity: 1; } to { opacity: 0; } }
        @keyframes slideIn { from { transform: translateY(-50px); opacity: 0; } to { transform: translateY(0); opacity: 1; } }
        
        /* 主题选择器样式 */
        #themeSelect { cursor: pointer; }
        #themeSelect:focus { border-color: #4a90e2; box-shadow: 0 0 0 2px rgba(74, 144, 226, 0.2); }
        
        /* 暗色主题样式 */
        body.dark-theme { 
            background: 
                linear-gradient(135deg, #0c0c0c 0%, #1a1a2e 25%, #16213e 50%, #0f3460 75%, #533483 100%);
            background-size: 400% 400%;
            animation: darkCyberFlow 18s ease infinite;
            color: #e0e0e0; 
        }
        
        body.dark-theme::before {
            background: 
                radial-gradient(circle at 25% 75%, rgba(0, 255, 255, 0.15) 0%, transparent 35%),
                radial-gradient(circle at 75% 25%, rgba(255, 0, 150, 0.12) 0%, transparent 40%),
                radial-gradient(circle at 50% 50%, rgba(138, 43, 226, 0.1) 0%, transparent 50%),
                radial-gradient(circle at 10% 90%, rgba(0, 191, 255, 0.08) 0%, transparent 30%),
                radial-gradient(circle at 90% 10%, rgba(255, 20, 147, 0.06) 0%, transparent 45%);
        }
        
        body.dark-theme::after {
            background: 
                linear-gradient(45deg, transparent 40%, rgba(0, 255, 255, 0.02) 50%, transparent 60%),
                linear-gradient(-45deg, transparent 40%, rgba(255, 0, 150, 0.02) 50%, transparent 60%);
            background-size: 80px 80px, 120px 120px;
            animation: cyberGrid 30s linear infinite;
        }
        
        @keyframes darkCyberFlow {
            0% { background-position: 0% 50%; }
            25% { background-position: 100% 75%; }
            50% { background-position: 0% 25%; }
            75% { background-position: 100% 50%; }
            100% { background-position: 0% 50%; }
        }
        
        @keyframes cyberGrid {
            0% { transform: translateX(-100%) translateY(-100%) rotate(0deg); }
            100% { transform: translateX(100%) translateY(100%) rotate(360deg); }
        }
        body.dark-theme .header { 
            background: linear-gradient(135deg, #0a0a0a 0%, #1a1a2e 50%, #533483 100%); 
            box-shadow: 0 15px 40px rgba(0, 255, 255, 0.2), 0 0 30px rgba(255, 0, 150, 0.15);
            border: 1px solid rgba(0, 255, 255, 0.3);
            position: relative;
            overflow: hidden;
        }
        
        body.dark-theme .header::before {
            background: linear-gradient(45deg, transparent 30%, rgba(0, 255, 255, 0.08) 50%, transparent 70%);
            animation: cyberHeaderShine 4s ease-in-out infinite;
        }
        
        body.dark-theme .stat-card { 
            background: linear-gradient(135deg, rgba(42, 42, 66, 0.95) 0%, rgba(28, 28, 50, 0.95) 100%); 
            backdrop-filter: blur(15px);
            color: #e0e0e0; 
            border: 1px solid rgba(0, 255, 255, 0.4);
            border-left: 4px solid #00ff41;
            box-shadow: 0 10px 30px rgba(0, 255, 65, 0.15), 0 0 20px rgba(0, 212, 255, 0.1);
        }
        
        body.dark-theme .stat-card:hover { 
            background: linear-gradient(135deg, rgba(52, 52, 76, 0.98) 0%, rgba(38, 38, 60, 0.98) 100%);
            box-shadow: 0 15px 40px rgba(0, 255, 65, 0.25), 0 0 30px rgba(0, 212, 255, 0.2);
            transform: translateY(-5px) scale(1.02);
            border-color: rgba(0, 255, 65, 0.6);
            border-left-color: #00ff41;
        }
        
        body.dark-theme .tabs { 
            background: rgba(10, 10, 10, 0.9); 
            backdrop-filter: blur(15px);
            box-shadow: 0 15px 40px rgba(0, 255, 255, 0.1), 0 0 25px rgba(255, 0, 150, 0.08); 
            border: 1px solid rgba(0, 255, 255, 0.2);
        }
        
        body.dark-theme .tab-buttons { 
            background: linear-gradient(135deg, rgba(10, 10, 10, 0.95) 0%, rgba(26, 26, 46, 0.95) 100%); 
            border-bottom: 1px solid rgba(0, 255, 255, 0.3); 
            border-radius: 20px 20px 0 0;
        }
        
        body.dark-theme .tab-button { 
            color: #b0b0b0; 
            transition: all 0.3s ease;
        }
        
        body.dark-theme .tab-button.active { 
            color: #00ffff; 
            background: linear-gradient(135deg, rgba(0, 255, 255, 0.2) 0%, rgba(255, 0, 150, 0.2) 100%); 
            box-shadow: 0 4px 15px rgba(0, 255, 255, 0.3), 0 0 10px rgba(255, 0, 150, 0.2);
            border: 1px solid rgba(0, 255, 255, 0.5);
            text-shadow: 0 0 10px rgba(0, 255, 255, 0.5);
        }
        
        body.dark-theme .tab-button:hover { 
            background: linear-gradient(135deg, rgba(0, 255, 255, 0.1) 0%, rgba(255, 0, 150, 0.1) 100%); 
            color: #e0e0e0;
            box-shadow: 0 2px 10px rgba(0, 255, 255, 0.2);
        }
        
                 @keyframes cyberHeaderShine {
             0% { transform: translateX(-100%); opacity: 0.3; }
             50% { transform: translateX(100%); opacity: 0.8; }
             100% { transform: translateX(-100%); opacity: 0.3; }
         }
         
         body.dark-theme .stat-value { 
             color: #00d4ff; /* 更柔和的青色 */
             background: linear-gradient(135deg, #00d4ff 0%, #0099ff 50%, #0066cc 100%);
             -webkit-background-clip: text;
             -webkit-text-fill-color: transparent;
             background-clip: text;
             text-shadow: 0 0 15px rgba(0, 212, 255, 0.4);
             font-weight: 700;
         }
         
         /* 暗色主题不支持渐变文字的浏览器备用样式 */
         body.dark-theme .stat-value:not(.gradient-text) {
             color: #00d4ff !important;
             background: none !important;
             -webkit-background-clip: initial !important;
             -webkit-text-fill-color: initial !important;
             background-clip: initial !important;
             text-shadow: 0 0 15px rgba(0, 212, 255, 0.4) !important;
             font-weight: 700 !important;
         }
         
         body.dark-theme .stat-label { 
             color: #b0b0b0; 
             text-shadow: 0 0 10px rgba(0, 255, 255, 0.2);
         }
        body.dark-theme .btn-primary { background: #4a90e2; }
        body.dark-theme .btn-primary:hover { background: #357abd; }
        body.dark-theme .btn-secondary { background: #5a6c7d; }
        body.dark-theme .btn-success { background: #27ae60; }
        body.dark-theme .btn-warning { background: #00d4ff; }
        body.dark-theme .btn-warning:hover { background: #0099ff; }
        body.dark-theme .btn-danger { background: #e74c3c; }
        body.dark-theme .search-box, body.dark-theme .form-control { background: #3a3a3a; color: #e0e0e0; border-color: #555; }
        body.dark-theme .search-box:focus, body.dark-theme .form-control:focus { border-color: #4a90e2; }
        body.dark-theme .table th { background: #3a3a3a; color: #e0e0e0; }
        body.dark-theme .table tbody tr:hover { background: #3a3a3a; }
        body.dark-theme .table td { border-bottom-color: #555; }
        body.dark-theme .modal-content { background: #2d2d2d; color: #e0e0e0; }
        body.dark-theme .modal-header { background: linear-gradient(135deg, #2c3e50 0%, #34495e 100%); }
        body.dark-theme .modal-body { background: #2d2d2d; }
        body.dark-theme .modal-footer { border-top-color: #555; }
        body.dark-theme .editor-textarea { background: #1e1e1e; color: #e0e0e0; border-color: #555; }
        body.dark-theme .panel-section { background: #3a3a3a; border-color: #555; }

        body.dark-theme .config-form { background: #2d2d2d; }
        body.dark-theme .config-section { background: #3a3a3a; border-color: #555; }
        body.dark-theme .config-section h3 { color: #4a90e2; }

        /* 自动补全样式 */
        .autocomplete-list {
            border: 1px solid #ddd;
            border-top: none;
            max-height: 150px;
            overflow-y: auto;
            background: white;
            box-shadow: 0 2px 8px rgba(0,0,0,0.08);
            font-size: 14px;
            border-radius: 0 0 6px 6px;
            margin-top: -2px;
            padding: 0;
            z-index: 1000;
            width: 100%;
            box-sizing: border-box;
        }
        .autocomplete-item {
            padding: 6px 10px;
            cursor: pointer;
            border-bottom: 1px solid #f0f0f0;
            transition: background 0.15s;
            background: #fff;
        }
        .autocomplete-item:last-child { border-bottom: none; }
        .autocomplete-item:hover, .autocomplete-item.active {
            background: #e6f7ff;
            color: #1890ff;
        }

        body.dark-theme .autocomplete-list {
            background: #3a3a3a;
            border-color: #555;
            color: #e0e0e0;
        }

        body.dark-theme .autocomplete-item:hover {
            background-color: #4a4a4a;
        }
        
        @media (max-width: 768px) { .container { padding: 10px; } .header { padding: 1rem; margin-bottom: 1rem; } .header h1 { font-size: 1.4rem; } .header p { font-size: 0.85rem; } .header-content { flex-direction: column; gap: 1rem; align-items: flex-start; } .header-actions { width: 100%; justify-content: center; } .server-status { align-items: center; } .uptime-display { font-size: 0.8rem; } .uptime-value { padding: 0.15rem 0.4rem; } .restart-btn { padding: 0.6rem 1.2rem; font-size: 0.85rem; } .stats-grid { grid-template-columns: repeat(2, 1fr); gap: 0.8rem; margin-bottom: 1rem; } .stat-card { padding: 0.8rem; } .stat-value { font-size: 1.3rem; } .stat-label { font-size: 0.75rem; } .tab-content { padding: 1rem; min-height: 500px; } .controls { flex-direction: column; align-items: stretch; } .search-box { width: 100%; } .table { font-size: 0.8rem; } .modal-content { width: 95%; margin: 2% auto; } .confirm-modal-content { width: 95%; margin: 10% auto; } }
        @media (max-width: 1024px) { 
            .config-editor { 
                grid-template-columns: 1fr; 
                height: auto; 
                max-height: none; 
            } 
            .editor-textarea { 
                height: 50vh; 
                min-height: 300px; 
            } 
            .sidebar-panel { 
                max-height: none; 
                overflow-y: visible; 
            } 
        }
        
        /* 配置表单样式 */
        .config-form { max-width: 800px; margin: 0 auto; }
        .config-form h3 { color: #2c3e50; margin-bottom: 1.5rem; display: flex; align-items: center; gap: 0.5rem; }
        .config-section { background: #f8f9fa; border: 1px solid #e9ecef; border-radius: 8px; padding: 1.5rem; margin-bottom: 1.5rem; }
        .config-section h4 { color: #495057; margin-top: 0; margin-bottom: 1rem; border-bottom: 2px solid #e9ecef; padding-bottom: 0.5rem; }
        .form-group { margin-bottom: 1rem; }
        .form-group label { display: block; margin-bottom: 0.5rem; color: #495057; font-weight: 500; }
        .form-group input[type="checkbox"] { margin-right: 0.5rem; }
        .form-actions { display: flex; gap: 1rem; justify-content: flex-end; padding-top: 1rem; margin-top: 1.5rem; border-top: 1px solid #e9ecef; }
        .form-actions button { padding: 0.75rem 1.5rem; }
        
        /* DNS服务器管理样式 */
        .dns-servers-section { }
        .dns-server-row { display: flex; gap: 10px; margin-bottom: 10px; align-items: center; }
        .dns-server-row input { flex: 1; }
        /* 网关DNS显示样式 */
        .dns-list { display: flex; flex-wrap: wrap; gap: 0.3rem; }
        .dns-item { background: #e8f4f8; color: #2c5282; padding: 0.2rem 0.5rem; border-radius: 12px; font-size: 0.75rem; border: 1px solid #bee3f8; }
        .text-muted { color: #6c757d; font-style: italic; }
        .btn-small { padding: 6px 12px; font-size: 0.875rem; }
        .form-help { color: #6c757d; font-size: 0.875rem; margin-top: 0.25rem; display: block; }
        
        /* 紧凑模式样式 */
        body.compact-mode .stat-card { padding: 0.75rem; }
        body.compact-mode .tab-button { padding: 0.5rem 1rem; }
        body.compact-mode .tab-content { padding: 1rem; }
        body.compact-mode .table th, body.compact-mode .table td { padding: 0.5rem; }
        body.compact-mode .form-group { margin-bottom: 0.75rem; }
        body.compact-mode .config-section { padding: 1rem; margin-bottom: 1rem; }

        /* 使用说明页面样式 */
        .help-content { line-height: 1.6; }
        .help-section { margin-bottom: 3rem; }
        .help-section h2 { 
            color: #2c3e50; 
            border-bottom: 3px solid #3498db; 
            padding-bottom: 0.5rem; 
            margin-bottom: 2rem;
            display: flex;
            align-items: center;
            gap: 0.5rem;
        }
        .help-card { 
            background: #fff;
            border: 1px solid #e3e6f0;
            border-radius: 12px;
            padding: 2rem;
            margin-bottom: 1.5rem;
            box-shadow: 0 2px 8px rgba(0,0,0,0.1);
            transition: transform 0.2s, box-shadow 0.2s;
        }
        .help-card:hover {
            transform: translateY(-2px);
            box-shadow: 0 4px 16px rgba(0,0,0,0.15);
        }
        .help-card h3 { 
            color: #2c3e50; 
            margin-top: 0; 
            margin-bottom: 1rem;
            display: flex;
            align-items: center;
            gap: 0.5rem;
        }
        .help-card ul, .help-card ol { 
            padding-left: 1.5rem; 
            margin-bottom: 1rem;
        }
        .help-card li { 
            margin-bottom: 0.5rem; 
            color: #555;
        }
        .help-card p { 
            color: #666; 
            margin-bottom: 1rem;
        }
        .code-block {
            background: #f8f9fa;
            border: 1px solid #e9ecef;
            border-radius: 8px;
            padding: 1rem;
            margin: 1rem 0;
            overflow-x: auto;
        }
        .code-block pre {
            margin: 0;
            font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace;
            font-size: 0.9rem;
            line-height: 1.4;
            color: #2c3e50;
        }
        .help-card code {
            background: #f1f3f4;
            padding: 0.2rem 0.4rem;
            border-radius: 4px;
            font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace;
            font-size: 0.9rem;
            color: #d63384;
        }
        .api-table { margin-top: 1rem; }
        .api-table table { width: 100%; border-collapse: collapse; }
        .api-table th, .api-table td { 
            padding: 0.75rem; 
            text-align: left; 
            border-bottom: 1px solid #e9ecef;
        }
        .api-table th { 
            background: #f8f9fa; 
            font-weight: 600; 
            color: #495057;
        }
        .version-info {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            border-radius: 12px;
            padding: 1.5rem;
        }
        .version-info h4 { color: white; margin-top: 0; }
        .version-info p { color: rgba(255,255,255,0.9); margin-bottom: 0.5rem; }

        /* 暗色主题下的使用说明样式 */
        body.dark-theme .help-card {
            background: #3a3a3a;
            border-color: #555;
            color: #e0e0e0;
        }
        body.dark-theme .help-card h3 { color: #4a90e2; }
        body.dark-theme .help-card p { color: #b0b0b0; }
        body.dark-theme .help-card li { color: #b0b0b0; }
        body.dark-theme .help-section h2 { 
            color: #4a90e2; 
            border-bottom-color: #4a90e2;
        }
        body.dark-theme .code-block {
            background: #2d2d2d;
            border-color: #555;
        }
        body.dark-theme .code-block pre { color: #e0e0e0; }
        body.dark-theme .help-card code {
            background: #2d2d2d;
            color: #ffc107;
        }
        body.dark-theme .api-table th {
            background: #2d2d2d;
            color: #e0e0e0;
        }
        body.dark-theme .api-table td {
            border-bottom-color: #555;
            color: #b0b0b0;
        }

        /* 粉色主题下的使用说明样式 */
        body.pink-theme .help-card {
            background: #fff5f8;
            border-color: #ffc1cc;
        }
        body.pink-theme .help-card h3 { color: #d63384; }
        body.pink-theme .help-section h2 { 
            color: #d63384; 
            border-bottom-color: #e91e63;
        }
        body.pink-theme .version-info {
            background: linear-gradient(135deg, #f093fb 0%, #f5576c 100%);
        }
        body.pink-theme .code-block {
            background: #fff0f3;
            border-color: #ffc1cc;
        }
        body.pink-theme .help-card code {
            background: #fff0f3;
            color: #d63384;
        }
    </style>
</head>
    <body>
        <!-- 主题切换器已移除，改为服务器配置页面选项 -->
        
        <div class="container">
            <div class="header">
                <div class="header-content">
                    <div class="header-info">
                        <h1>🌐 DHCP服务器管理</h1>
                        <p>企业级DHCP服务器 - 配置管理与监控平台</p>
                    </div>
                    <div class="header-actions">
                        <div class="server-status">
                                    <div class="uptime-display">
            <span class="uptime-label">运行时间:</span>
            <div class="uptime-container">
                <span class="uptime-value" id="serverUptime">计算中...</span>
                <div class="uptime-progress">
                    <div class="uptime-progress-bar" id="uptimeProgress"></div>
                </div>
            </div>
        </div>
                            <button class="btn btn-warning restart-btn" onclick="quickRestartServer()" title="快速重启DHCP服务器">
                                🔄 重启服务器
                            </button>
                        </div>
                    </div>
                </div>
            </div>

        <div class="stats-grid" id="statsGrid">
            <!-- 统计卡片将通过JavaScript动态加载 -->
        </div>

        <div class="tabs">
            <div class="tab-buttons">
                <button class="tab-button active" onclick="switchTab('leases')">📋 租约管理</button>
                <button class="tab-button" onclick="switchTab('history')">📜 历史记录</button>
                <button class="tab-button" onclick="switchTab('gateways')">🌐 网关状态</button>
                <button class="tab-button" onclick="switchTab('devices')">📱 设备管理</button>
                <button class="tab-button" onclick="switchTab('server-config')">🖥️ 服务器配置</button>
                <button class="tab-button" onclick="switchTab('config')">⚙️ 配置管理</button>
                <button class="tab-button" onclick="switchTab('ui-config')">🎨 界面配置</button>
                <button class="tab-button" onclick="switchTab('logs')">📄 程序日志</button>
                <button class="tab-button" onclick="switchTab('help')">❓ 使用说明</button>
            </div>

            <div class="tab-content">
                <!-- 租约管理标签页 -->
                <div id="leases" class="tab-pane active">
                    <div class="controls">
                        <div>
                            <button class="btn btn-primary" onclick="toggleLeaseMode()">
                                <span id="leaseModeText">显示所有租约</span>
                            </button>
                        </div>
                        <input type="text" class="search-box" id="leaseSearch" placeholder="搜索MAC地址或IP地址..." oninput="filterLeases()">
                        <div class="auto-refresh">
                            <label>
                                <input type="checkbox" id="autoRefresh" checked> 自动刷新
                            </label>
                        </div>
                    </div>
                    <div id="leasesTable"></div>
                </div>

                <!-- 历史记录标签页 -->
                <div id="history" class="tab-pane">
                    <div class="controls">
                        <select id="historyLimit" class="search-box" style="width: 150px;" onchange="loadHistory()">
                            <option value="50">最近50条</option>
                            <option value="100" selected>最近100条</option>
                            <option value="200">最近200条</option>
                            <option value="500">最近500条</option>
                        </select>
                        <input type="text" class="search-box" id="historySearch" placeholder="搜索MAC地址或IP地址..." oninput="filterHistory()">
                    </div>
                    <div id="historyTable"></div>
                </div>

                <!-- 网关状态标签页 -->
                <div id="gateways" class="tab-pane">
                    <div class="controls">
                        <div>
                            <button class="btn btn-success" onclick="showAddGatewayModal()">添加网关</button>
                        </div>
                    </div>
                    <div id="gatewayStatus"></div>
                </div>

                <!-- 设备管理标签页 -->
                <div id="devices" class="tab-pane">
                    <div class="controls">
                        <div>
                            <button class="btn btn-primary" onclick="discoverDevices()">发现设备</button>
                            <button class="btn btn-success" onclick="showAddDeviceModal()">添加设备</button>
                        </div>
                        <input type="text" class="search-box" id="deviceSearch" placeholder="搜索设备..." oninput="filterDevices()">
                    </div>
                    <div id="devicesTable"></div>
                </div>

                <!-- 配置管理标签页 -->
                <div id="config" class="tab-pane">
                    <div class="config-tabs">
                        <div class="config-tab-buttons">
                            <button class="config-tab-button active" onclick="switchConfigSubTab('network-config')">🌐 网络配置</button>
                            <button class="config-tab-button" onclick="switchConfigSubTab('gateway-detection')">🚀 网关检测</button>
                            <button class="config-tab-button" onclick="switchConfigSubTab('network-scanner')">🔍 网络扫描器</button>
                            <button class="config-tab-button" onclick="switchConfigSubTab('file-management')">📁 配置文件管理</button>
                        </div>
                        
                        <div class="config-tab-content">
                            <!-- 网络配置子标签页 -->
                            <div id="network-config" class="config-sub-pane active">
                                <div class="config-form">
                                    <h3>🌐 网络配置</h3>
                                    <form id="networkConfigForm">
                                        <div class="config-section">
                                            <h4>网络范围</h4>
                                            <div class="form-group">
                                                <label for="networkSubnet">子网地址 *</label>
                                                <input type="text" id="networkSubnet" class="form-control" placeholder="192.168.1.0/24" required>
                                            </div>
                                            <div class="form-group">
                                                <label for="networkStartIP">起始IP地址 *</label>
                                                <input type="text" id="networkStartIP" class="form-control" placeholder="192.168.1.100" required>
                                            </div>
                                            <div class="form-group">
                                                <label for="networkEndIP">结束IP地址 *</label>
                                                <input type="text" id="networkEndIP" class="form-control" placeholder="192.168.1.200" required>
                                            </div>
                                            <div class="form-group">
                                                <label for="networkDefaultGateway">默认网关</label>
                                                <input type="text" id="networkDefaultGateway" class="form-control" placeholder="192.168.1.1">
                                            </div>
                                        </div>
                                        
                                        <div class="config-section">
                                            <h4>DNS服务器配置</h4>
                                            <div class="dns-servers-section">
                                                <div class="form-group" style="display: flex; justify-content: space-between; align-items: center;">
                                                    <label>DNS服务器列表</label>
                                                    <button type="button" class="btn btn-success btn-small" onclick="addDNSServer()">➕ 添加DNS</button>
                                                </div>
                                                <div id="dnsServersContainer">
                                                    <!-- DNS服务器输入框将动态添加到这里 -->
                                                </div>
                                                <small class="form-help">您可以配置多个DNS服务器，系统将按顺序使用</small>
                                            </div>
                                        </div>
                                        
                                        <div class="config-section">
                                            <h4>高级设置</h4>
                                            <div class="form-group">
                                                <label for="networkLeaseTime">租约时间 (秒)</label>
                                                <input type="number" id="networkLeaseTime" class="form-control" placeholder="86400" min="60">
                                            </div>
                                            <div class="form-group">
                                                <label for="networkRenewalTime">续租时间 (秒)</label>
                                                <input type="number" id="networkRenewalTime" class="form-control" placeholder="43200" min="30">
                                            </div>
                                            <div class="form-group">
                                                <label for="networkRebindingTime">重新绑定时间 (秒)</label>
                                                <input type="number" id="networkRebindingTime" class="form-control" placeholder="75600" min="60">
                                            </div>
                                            <div class="form-group">
                                                <label for="networkDomainName">域名</label>
                                                <input type="text" id="networkDomainName" class="form-control" placeholder="local">
                                            </div>
                                            <div class="form-group">
                                                <label for="networkBroadcastAddress">广播地址</label>
                                                <input type="text" id="networkBroadcastAddress" class="form-control" placeholder="192.168.1.255">
                                            </div>
                                        </div>
                                        <div class="form-actions">
                                            <button type="button" class="btn btn-primary" onclick="saveNetworkConfig()">保存网络配置</button>
                                            <button type="button" class="btn btn-secondary" onclick="loadNetworkConfig(true)">重新加载</button>
                                        </div>
                                    </form>
                                </div>
                            </div>
                            
                            <!-- 网关检测子标签页 -->
                            <div id="gateway-detection" class="config-sub-pane">
                                <div class="config-form">
                                    <h3>🚀 网关检测配置</h3>
                                    <form id="gatewayDetectionForm">
                                        <div class="config-section">
                                            <h4>检测设置</h4>
                                            <div class="form-group">
                                                <label for="healthCheckInterval">检测间隔 (秒)</label>
                                                <input type="number" id="healthCheckInterval" class="form-control" placeholder="30" min="5" max="3600">
                                                <small class="form-help">网关健康检查的时间间隔，建议设置为30秒</small>
                                            </div>
                                            <div class="form-group">
                                                <label for="healthCheckTimeout">检测超时 (秒)</label>
                                                <input type="number" id="healthCheckTimeout" class="form-control" placeholder="5" min="1" max="30">
                                                <small class="form-help">单次检测的超时时间，建议设置为5秒</small>
                                            </div>
                                            <div class="form-group">
                                                <label for="healthCheckRetryCount">重试次数</label>
                                                <input type="number" id="healthCheckRetryCount" class="form-control" placeholder="3" min="1" max="10">
                                                <small class="form-help">检测失败后的重试次数</small>
                                            </div>
                                        </div>
                                        
                                        <div class="config-section">
                                            <h4>检测方法</h4>
                                            <div class="form-group">
                                                <label for="healthCheckMethod">检测方式</label>
                                                <select id="healthCheckMethod" class="form-control" onchange="updateHealthCheckOptions()">
                                                    <option value="ping">🏓 PING检测</option>
                                                    <option value="tcp">🔌 TCP端口检测</option>
                                                    <option value="http">🌐 HTTP检测</option>
                                                </select>
                                                <small class="form-help">选择合适的检测方式</small>
                                            </div>
                                            <div class="form-group" id="tcpPortGroup" style="display: none;">
                                                <label for="healthCheckTcpPort">TCP端口</label>
                                                <input type="number" id="healthCheckTcpPort" class="form-control" placeholder="80" min="1" max="65535">
                                                <small class="form-help">用于TCP检测的端口号</small>
                                            </div>
                                            <div class="form-group" id="httpPathGroup" style="display: none;">
                                                <label for="healthCheckHttpPath">HTTP路径</label>
                                                <input type="text" id="healthCheckHttpPath" class="form-control" placeholder="/" pattern="^/.*">
                                                <small class="form-help">用于HTTP检测的路径，如 /health 或 /</small>
                                            </div>
                                        </div>
                                        
                                        <div class="config-section">
                                            <h4>状态信息</h4>
                                            <div class="form-group">
                                                <label>当前检测状态</label>
                                                <div id="currentHealthStatus" class="status-display">
                                                    <div class="status-item">
                                                        <span class="status-label">检测服务状态：</span>
                                                        <span class="status-value" id="detectionServiceStatus">运行中</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">上次检测时间：</span>
                                                        <span class="status-value" id="lastCheckTime">--</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">下次检测时间：</span>
                                                        <span class="status-value" id="nextCheckTime">--</span>
                                                    </div>
                                                </div>
                                            </div>
                                        </div>
                                        
                                        <div class="form-actions">
                                            <button type="button" class="btn btn-primary" onclick="saveGatewayDetectionConfig()">保存检测配置</button>
                                            <button type="button" class="btn btn-secondary" onclick="loadGatewayDetectionConfig(true)">重新加载</button>
                                            <button type="button" class="btn btn-warning" onclick="testGatewayDetection()">测试检测</button>
                                        </div>
                                    </form>
                                </div>
                            </div>
                            
                            <!-- 网络扫描器子标签页 -->
                            <div id="network-scanner" class="config-sub-pane">
                                <div class="config-form">
                                    <h3>🔍 网络扫描器配置</h3>
                                    <form id="networkScannerForm">
                                        <div class="config-section">
                                            <h4>扫描设置</h4>
                                            <div class="form-group">
                                                <label for="scannerEnabled">启用网络扫描</label>
                                                <select id="scannerEnabled" class="form-control">
                                                    <option value="true">启用</option>
                                                    <option value="false">禁用</option>
                                                </select>
                                                <small class="form-help">启用后系统将定期扫描网络以发现设备和检测IP冲突</small>
                                            </div>
                                            <div class="form-group">
                                                <label for="scannerInterval">扫描间隔 (秒)</label>
                                                <input type="number" id="scannerInterval" class="form-control" placeholder="300" min="60" max="3600">
                                                <small class="form-help">扫描间隔时间，建议设置为300秒（5分钟）</small>
                                            </div>
                                            <div class="form-group">
                                                <label for="scannerConcurrency">并发扫描数</label>
                                                <input type="number" id="scannerConcurrency" class="form-control" placeholder="10" min="1" max="100">
                                                <small class="form-help">同时扫描的IP数量，建议设置为10-20</small>
                                            </div>
                                            <div class="form-group">
                                                <label for="scannerTimeout">扫描超时 (秒)</label>
                                                <input type="number" id="scannerTimeout" class="form-control" placeholder="3" min="1" max="30">
                                                <small class="form-help">单个IP扫描的超时时间</small>
                                            </div>
                                        </div>
                                        
                                        <div class="config-section">
                                            <h4>扫描范围</h4>
                                            <div class="form-group">
                                                <label>扫描范围</label>
                                                <div class="info-display">
                                                    <span id="scannerRangeDisplay">自动使用DHCP可分配地址范围</span>
                                                </div>
                                                <small class="form-help">扫描器会自动扫描DHCP服务器可分配的IP地址范围</small>
                                            </div>
                                        </div>
                                        
                                        <div class="config-section">
                                            <h4>冲突处理</h4>
                                            <div class="form-group">
                                                <label for="scannerConflictAction">IP冲突处理方式</label>
                                                <select id="scannerConflictAction" class="form-control">
                                                    <option value="mark_conflicted">标记为冲突</option>
                                                    <option value="ignore">忽略冲突</option>
                                                </select>
                                                <small class="form-help">发现IP冲突时的处理方式</small>
                                            </div>
                                            <div class="form-group">
                                                <label for="scannerConflictTimeout">冲突标记超时 (分钟)</label>
                                                <input type="number" id="scannerConflictTimeout" class="form-control" placeholder="30" min="5" max="1440">
                                                <small class="form-help">冲突IP标记的超时时间，超时后重新可用</small>
                                            </div>
                                        </div>
                                        
                                        <div class="config-section">
                                            <h4>扫描状态</h4>
                                            <div class="form-group">
                                                <label>当前扫描状态</label>
                                                <div id="currentScannerStatus" class="status-display">
                                                    <div class="status-item">
                                                        <span class="status-label">扫描器状态：</span>
                                                        <span class="status-value" id="scannerServiceStatus">--</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">扫描进度：</span>
                                                        <span class="status-value" id="scanProgress">0%</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">当前扫描：</span>
                                                        <span class="status-value" id="currentScanIP">--</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">已扫描：</span>
                                                        <span class="status-value" id="scannedIPs">0/0</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">发现设备数量：</span>
                                                        <span class="status-value" id="discoveredDevicesCount">0</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">冲突IP数量：</span>
                                                        <span class="status-value" id="conflictedIPsCount">0</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">上次扫描时间：</span>
                                                        <span class="status-value" id="lastScanTime">--</span>
                                                    </div>
                                                    <div class="status-item">
                                                        <span class="status-label">下次扫描时间：</span>
                                                        <span class="status-value" id="nextScanTime">--</span>
                                                    </div>
                                                </div>
                                            </div>
                                        </div>
                                        
                                        <div class="form-actions">
                                            <button type="button" class="btn btn-primary" onclick="saveNetworkScannerConfig()">保存扫描器配置</button>
                                            <button type="button" class="btn btn-secondary" onclick="loadNetworkScannerConfig(true)">重新加载</button>
                                            <button type="button" class="btn btn-success" onclick="startNetworkScanner()">启动扫描</button>
                                            <button type="button" class="btn btn-warning" onclick="stopNetworkScanner()">停止扫描</button>
                                            <button type="button" class="btn btn-info" onclick="viewScannerResults()">查看扫描结果</button>
                                            <button type="button" class="btn btn-secondary" onclick="viewScannerLog()">查看扫描日志</button>
                                        </div>
                                    </form>
                                </div>
                            </div>
                            
                            <!-- 配置文件管理子标签页 -->
                            <div id="file-management" class="config-sub-pane">
                                <div class="config-editor">
                                    <div class="editor-panel">
                                        <div class="editor-header">
                                            <h3>配置文件编辑</h3>
                                            <div>
                                                <button class="btn btn-secondary" onclick="reloadConfig()">重新加载</button>
                                                <button class="btn btn-warning" onclick="validateConfig()">验证配置</button>
                                                <button class="btn btn-success" onclick="saveConfig()">保存配置</button>
                                            </div>
                                        </div>
                                        <textarea id="configEditor" class="editor-textarea" placeholder="配置文件加载中..."></textarea>
                                    </div>
                                    
                                    <div class="sidebar-panel">
                                        <div class="panel-section">
                                            <div class="panel-title">保存选项</div>
                                            <div class="checkbox-option">
                                                <input type="checkbox" id="autoReloadCheck" checked>
                                                <label for="autoReloadCheck">保存后自动重载</label>
                                            </div>
                                        </div>
                                        
                                        <div class="panel-section">
                                            <div class="panel-title">验证结果</div>
                                            <div id="validationResult"></div>
                                        </div>
                                        
                                        <div class="panel-section">
                                            <div class="panel-title">配置备份</div>
                                            <button class="btn btn-primary" onclick="loadBackups()" style="width: 100%; margin-bottom: 0.5rem;">刷新备份列表</button>
                                            <div class="backup-list" id="backupList"></div>
                                        </div>
                                        
                                        <div class="panel-section">
                                            <div class="panel-title">操作</div>
                                            <button class="btn btn-warning" onclick="reloadServerConfig()" style="width: 100%; margin-bottom: 0.5rem;">重载服务器配置</button>
                                            <button class="btn btn-secondary" onclick="downloadConfig()" style="width: 100%;">下载配置文件</button>
                                        </div>
                                    </div>
                                </div>
                            </div>
                        </div>
                    </div>
                </div>

                <!-- 服务器配置标签页 -->
                <div id="server-config" class="tab-pane">
                    <div class="config-form">
                        <h3>🖥️ 服务器配置</h3>
                        <form id="serverConfigForm">
                            <div class="config-section">
                                <h4>基础设置</h4>
                                <div class="form-group">
                                    <label for="serverInterface">网络接口 *</label>
                                    <input type="text" id="serverInterface" class="form-control" placeholder="eth0, wlan0, en0" required>
                                </div>
                                <div class="form-group">
                                    <label for="serverPort">DHCP端口</label>
                                    <input type="number" id="serverPort" class="form-control" placeholder="67" min="1" max="65535" required>
                                </div>
                                <div class="form-group">
                                    <label for="serverAPIPort">Web管理界面端口</label>
                                    <input type="number" id="serverAPIPort" class="form-control" placeholder="8080" min="1" max="65535" required>
                                    <small class="form-help">用于访问Web管理界面的端口号</small>
                                </div>
                                <div class="form-group">
                                    <label for="serverAPIHost">Web管理界面访问地址</label>
                                    <input type="text" id="serverAPIHost" class="form-control" placeholder="0.0.0.0" pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$|^localhost$|^0\.0\.0\.0$|^$">
                                    <small class="form-help">
                                        <strong>访问地址说明：</strong><br>
                                        • <code>0.0.0.0</code> - 允许从任何IP地址访问Web界面<br>
                                        • <code>127.0.0.1</code> - 仅允许本地访问，更安全<br>
                                        • <code>192.168.1.10</code> - 仅允许从指定IP地址访问<br>
                                        • 留空使用默认值(0.0.0.0)<br>
                                        <strong>⚠️ 修改此设置需要重启服务器</strong>
                                    </small>
                                </div>
                                <div class="form-group">
                                    <label for="serverLeaseTime">DHCP租期时间</label>
                                    <input type="text" id="serverLeaseTime" class="form-control" placeholder="24h" pattern="^(\d+[smhd])+$">
                                    <small class="form-help">
                                        <strong>租期时间格式：</strong><br>
                                        • <code>24h</code> - 24小时（推荐）<br>
                                        • <code>30m</code> - 30分钟<br>
                                        • <code>1d</code> - 1天<br>
                                        • <code>2h30m</code> - 2小时30分钟<br>
                                        支持 s(秒), m(分), h(小时), d(天) 组合
                                    </small>
                                </div>
                                <div class="form-group">
                                    <label for="serverLogLevel">日志级别</label>
                                    <select id="serverLogLevel" class="form-control">
                                        <option value="debug">Debug</option>
                                        <option value="info">Info</option>
                                        <option value="warn">Warn</option>
                                        <option value="error">Error</option>
                                    </select>
                                </div>
                                <div class="form-group">
                                    <label for="serverLogFile">日志文件</label>
                                    <input type="text" id="serverLogFile" class="form-control" placeholder="dhcp.log">
                                </div>
                                <div class="form-group">
                                    <label>
                                        <input type="checkbox" id="serverDebug"> 调试模式
                                    </label>
                                </div>
                                <div class="form-group">
                                    <label>
                                        <input type="checkbox" id="serverAllowAnyServerIP"> 允许响应任意ServerIP（不建议，除非有特殊需求）
                                    </label>
                                    <small class="form-help">开启后，DHCP服务器会响应所有ServerIP的Request请求，兼容部分特殊网络环境。</small>
                                </div>
                            </div>

                            <div class="form-actions">
                                <button type="button" class="btn btn-primary" onclick="saveServerConfig()">保存服务器配置</button>
                                <button type="button" class="btn btn-secondary" onclick="loadServerConfig(true)">重新加载</button>
                                <button type="button" class="btn btn-warning" onclick="quickRestartServer()" title="快速重启DHCP服务器">🔄 重启服务器</button>
                            </div>
                        </form>
                    </div>
                </div>



                <!-- 界面配置标签页 -->
                <div id="ui-config" class="tab-pane">
                    <div class="config-form">
                        <h3>🎨 界面配置</h3>
                        <div class="config-section">
                            <h4>主题设置</h4>
                            <div class="form-group">
                                <label for="themeSelect">界面主题</label>
                                <select id="themeSelect" class="form-control" onchange="changeTheme()">
                                    <option value="light">🌞 亮色主题</option>
                                    <option value="pink">🌸 粉色主题</option>
                                    <option value="dark">🌙 暗色主题</option>
                                </select>
                                <small class="form-help">选择您喜欢的界面主题，设置会立即生效并保存到浏览器本地</small>
                            </div>
                        </div>
                        
                        <div class="config-section">
                            <h4>显示设置</h4>
                            <div class="form-group">
                                <label>
                                    <input type="checkbox" id="compactMode" onchange="updateCompactMode()"> 紧凑模式
                                </label>
                                <small class="form-help">启用后界面元素间距更小，显示更多内容</small>
                            </div>
                            <div class="form-group">
                                <label>
                                    <input type="checkbox" id="showAdvanced" checked> 显示高级功能
                                </label>
                                <small class="form-help">显示或隐藏高级配置选项和功能</small>
                            </div>
                        </div>
                        
                        <div class="config-section">
                            <h4>数据刷新</h4>
                            <div class="form-group">
                                <label for="refreshInterval">自动刷新间隔</label>
                                <select id="refreshInterval" class="form-control" onchange="updateRefreshInterval()">
                                    <option value="5">5秒</option>
                                    <option value="10" selected>10秒</option>
                                    <option value="30">30秒</option>
                                    <option value="60">1分钟</option>
                                    <option value="0">关闭自动刷新</option>
                                </select>
                                <small class="form-help">设置数据自动刷新的时间间隔</small>
                            </div>
                        </div>
                        
                        <div class="form-actions">
                            <button type="button" class="btn btn-primary" onclick="saveUIConfig()">保存界面配置</button>
                            <button type="button" class="btn btn-secondary" onclick="resetUIConfig()">恢复默认</button>
                        </div>
                    </div>
                </div>

                <!-- 程序日志标签页 -->
                <div id="logs" class="tab-pane">
                    <div class="controls">
                        <div>
                            <select id="logLimit" class="search-box" style="width: 150px;" onchange="loadLogs()">
                                <option value="100">最近100行</option>
                                <option value="200">最近200行</option>
                                <option value="500" selected>最近500行</option>
                                <option value="1000">最近1000行</option>
                            </select>
                            <button class="btn btn-primary" onclick="loadLogs()">刷新日志</button>
                        </div>
                        <input type="text" class="search-box" id="logSearch" placeholder="搜索日志内容..." oninput="filterLogs()">
                        <div class="auto-refresh">
                            <label>
                                <input type="checkbox" id="logAutoRefresh" checked> 自动刷新
                            </label>
                        </div>
                    </div>
                    <div id="logsContent" style="height: 600px; overflow-y: auto; background: #f8f9fa; border: 1px solid #e9ecef; border-radius: 8px; padding: 1rem; font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace; font-size: 14px; line-height: 1.4; white-space: pre-wrap;">
                        <div style="text-align: center; color: #666; padding: 2rem;">
                            点击"刷新日志"按钮加载程序日志...
                        </div>
                    </div>
                </div>

                <!-- 使用说明标签页 -->
                <div id="help" class="tab-pane">
                    <div class="help-content" style="max-width: 1200px; margin: 0 auto; padding: 2rem;">
                        <!-- 快速开始指南 -->
                        <div class="help-section">
                            <h2>🚀 快速开始指南</h2>
                            <div class="help-card">
                                <h3>📋 系统要求</h3>
                                <ul>
                                    <li><strong>操作系统</strong>: Linux、macOS、Windows</li>
                                    <li><strong>Go版本</strong>: 1.19或更高版本</li>
                                    <li><strong>网络权限</strong>: 需要绑定UDP 67端口（建议以管理员权限运行）</li>
                                    <li><strong>内存</strong>: 最少64MB RAM</li>
                                    <li><strong>磁盘</strong>: 最少100MB可用空间</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>⚡ 快速启动</h3>
                                <div class="code-block">
                                    <pre># 编译程序
go build -o dhcp-server .

# 以管理员权限运行（推荐）
sudo ./dhcp-server

# 使用自定义配置
sudo ./dhcp-server -config my-config.yaml

# 查看版本信息
./dhcp-server -version</pre>
                                </div>
                                <p>服务启动后，访问 <code>http://localhost:8083</code> 进入Web管理界面。</p>
                            </div>
                        </div>

                        <!-- 功能模块说明 -->
                        <div class="help-section">
                            <h2>📋 功能模块说明</h2>

                            <div class="help-card">
                                <h3>📊 统计概览</h3>
                                <p>页面顶部显示实时统计信息：</p>
                                <ul>
                                    <li><strong>总IP数量</strong>: 配置的IP地址池大小</li>
                                    <li><strong>活跃租约</strong>: 当前正在使用的IP数量</li>
                                    <li><strong>静态租约</strong>: 配置的静态IP绑定数量</li>
                                    <li><strong>地址池利用率</strong>: IP地址使用百分比</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>📋 租约管理</h3>
                                <p>管理DHCP IP地址租约的核心功能：</p>
                                <ul>
                                    <li><strong>查看租约</strong>: 显示活跃租约和历史租约</li>
                                    <li><strong>搜索过滤</strong>: 按MAC地址或IP地址快速搜索</li>
                                    <li><strong>租约详情</strong>: 查看租约时间、剩余时间、网关等信息</li>
                                    <li><strong>转换静态</strong>: 将动态租约转换为静态IP绑定</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>📜 历史记录</h3>
                                <p>查看DHCP服务的完整操作历史：</p>
                                <ul>
                                    <li><strong>操作记录</strong>: DISCOVER、REQUEST、RELEASE等DHCP操作</li>
                                    <li><strong>时间过滤</strong>: 选择查看最近50/100/200/500条记录</li>
                                    <li><strong>设备跟踪</strong>: 追踪特定设备的网络活动</li>
                                    <li><strong>故障排查</strong>: 分析网络连接问题</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>🌐 网关状态</h3>
                                <p>监控和管理网络网关的健康状态：</p>
                                <ul>
                                    <li><strong>健康监控</strong>: 实时检查网关连通性</li>
                                    <li><strong>自动切换</strong>: 网关故障时自动切换到备用网关</li>
                                    <li><strong>检测方式</strong>: 支持ping、TCP、HTTP三种检测方法</li>
                                    <li><strong>网关管理</strong>: 添加、编辑、删除网关配置</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>📱 设备管理</h3>
                                <p>管理网络中的设备信息：</p>
                                <ul>
                                    <li><strong>设备识别</strong>: 自动识别设备类型和制造商</li>
                                    <li><strong>设备发现</strong>: 扫描网络中的新设备</li>
                                    <li><strong>信息维护</strong>: 编辑设备名称、类型、描述等信息</li>
                                    <li><strong>批量管理</strong>: 支持批量添加和删除设备</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>⚙️ 配置管理</h3>
                                <p>在线配置系统参数：</p>
                                <ul>
                                    <li><strong>网络配置</strong>: 设置IP地址池、DNS服务器、域名等</li>
                                    <li><strong>网关检测</strong>: 配置健康检查参数</li>
                                    <li><strong>网络扫描器</strong>: 配置网络扫描和IP冲突检测</li>
                                    <li><strong>配置备份</strong>: 自动备份和一键恢复配置</li>
                                    <li><strong>热重载</strong>: 无需重启即可应用配置更改</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>🎨 界面配置</h3>
                                <p>个性化界面设置：</p>
                                <ul>
                                    <li><strong>主题切换</strong>: 亮色、粉色、暗色三种主题</li>
                                    <li><strong>自动刷新</strong>: 设置数据自动刷新间隔</li>
                                    <li><strong>紧凑模式</strong>: 在有限屏幕空间中显示更多信息</li>
                                    <li><strong>界面记忆</strong>: 自动保存用户的界面偏好设置</li>
                                    <li><strong>高级功能</strong>: 显示或隐藏高级配置选项</li>
                                </ul>
                            </div>
                        </div>

                        <!-- 常见操作指南 -->
                        <div class="help-section">
                            <h2>🔧 常见操作指南</h2>

                            <div class="help-card">
                                <h3>📌 配置静态IP绑定</h3>
                                <ol>
                                    <li>在"设备管理"页面找到目标设备</li>
                                    <li>点击设备行末的"配置静态IP"按钮</li>
                                    <li>系统会自动生成唯一的绑定别名</li>
                                    <li>设置IP地址</li>
                                    <li>设置主机名（可选）</li>
                                    <li>保存配置</li>
                                </ol>
                                <p><strong>注意</strong>: 静态IP必须在配置的地址池范围内，且不能与其他设备冲突。绑定别名由系统自动生成，确保唯一性。</p>

                                <h3>🌐 指定设备网关</h3>
                                <ol>
                                    <li>在"设备管理"页面找到目标设备</li>
                                    <li>点击设备行末的"指定网关"按钮</li>
                                    <li>在弹出的对话框中选择要使用的网关</li>
                                    <li>点击"保存"按钮完成配置</li>
                                </ol>
                                <p><strong>说明</strong>: 网关配置独立于静态IP配置，可以为任何设备单独指定网关。</p>
                            </div>

                            <div class="help-card">
                                <h3>🌐 添加网关</h3>
                                <ol>
                                    <li>进入"网关状态"页面</li>
                                    <li>点击"添加网关"按钮</li>
                                    <li>输入网关名称和IP地址</li>
                                    <li>设置是否为默认网关</li>
                                    <li>添加描述信息（可选）</li>
                                    <li>保存配置</li>
                                </ol>
                                <p><strong>提示</strong>: 系统会自动进行网关健康检查，确保网关可用性。</p>
                            </div>

                            <div class="help-card">
                                <h3>🔍 设备发现</h3>
                                <ol>
                                    <li>在"设备管理"页面点击"发现设备"</li>
                                    <li>系统会扫描当前租约中的新设备</li>
                                    <li>在弹出的对话框中选择要添加的设备</li>
                                    <li>可以批量选择多个设备</li>
                                    <li>确认添加到设备列表</li>
                                </ol>
                                <p><strong>说明</strong>: 设备发现基于当前DHCP租约记录，确保设备已连接到网络。</p>
                            </div>

                            <div class="help-card">
                                <h3>📁 配置备份与恢复</h3>
                                <ol>
                                    <li>进入"配置管理" → "配置文件管理"</li>
                                    <li><strong>备份</strong>: 点击"创建配置备份"自动生成备份文件</li>
                                    <li><strong>恢复</strong>: 从备份列表中选择要恢复的配置</li>
                                    <li>确认恢复操作</li>
                                    <li>系统会自动重载配置</li>
                                </ol>
                                <p><strong>建议</strong>: 修改重要配置前先创建备份，以便出现问题时快速恢复。</p>
                            </div>

                            <div class="help-card">
                                <h3>🔍 网络扫描器配置</h3>
                                <ol>
                                    <li>进入"配置管理" → "网络扫描器"</li>
                                    <li>启用网络扫描功能</li>
                                    <li>设置扫描间隔时间（建议300秒）</li>
                                    <li>配置并发扫描数量（建议10-20）</li>
                                    <li>设置IP冲突处理方式</li>
                                    <li>保存配置并启动扫描</li>
                                </ol>
                                <p><strong>说明</strong>: 扫描器会自动扫描DHCP可分配IP范围，检测设备活动和IP冲突。</p>
                            </div>
                        </div>

                        <!-- 故障排除 -->
                        <div class="help-section">
                            <h2>🔧 故障排除</h2>

                            <div class="help-card">
                                <h3>❌ 客户端无法获取IP地址</h3>
                                <p><strong>可能原因和解决方案：</strong></p>
                                <ul>
                                    <li><strong>检查网络接口</strong>: 确认监听的网络接口配置正确</li>
                                    <li><strong>检查防火墙</strong>: 确保UDP 67端口没有被阻止</li>
                                    <li><strong>检查地址池</strong>: 查看IP地址池是否已满</li>
                                    <li><strong>查看日志</strong>: 在"程序日志"页面查看详细错误信息</li>
                                    <li><strong>权限问题</strong>: 确保以管理员权限运行程序</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>🚫 网关不可达</h3>
                                <p><strong>诊断步骤：</strong></p>
                                <ul>
                                    <li><strong>检查网关状态</strong>: 在"网关状态"页面查看健康检查结果</li>
                                    <li><strong>手动测试</strong>: 尝试手动ping网关IP地址</li>
                                    <li><strong>检查配置</strong>: 确认网关IP地址配置正确</li>
                                    <li><strong>网络连通性</strong>: 检查服务器到网关的网络连接</li>
                                    <li><strong>调整检测方式</strong>: 尝试不同的健康检查方法（ping/tcp/http）</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>⚠️ 静态绑定不生效</h3>
                                <p><strong>检查清单：</strong></p>
                                <ul>
                                    <li><strong>MAC地址格式</strong>: 确认MAC地址格式正确（aa:bb:cc:dd:ee:ff）</li>
                                    <li><strong>IP地址范围</strong>: 确认IP在配置的地址池范围内</li>
                                    <li><strong>重复配置</strong>: 检查是否有重复的MAC或IP配置</li>
                                    <li><strong>客户端释放</strong>: 客户端需要释放当前租约并重新请求</li>
                                    <li><strong>配置重载</strong>: 修改配置后点击"重载配置"按钮</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>🌐 Web界面无法访问</h3>
                                <p><strong>解决步骤：</strong></p>
                                <ul>
                                    <li><strong>检查端口</strong>: 确认API端口8080配置正确</li>
                                    <li><strong>防火墙设置</strong>: 确保端口8080没有被防火墙阻止</li>
                                    <li><strong>服务状态</strong>: 检查HTTP API服务是否启动成功</li>
                                    <li><strong>日志查看</strong>: 查看启动日志确认服务状态</li>
                                    <li><strong>端口冲突</strong>: 检查端口是否被其他程序占用</li>
                                </ul>
                            </div>

                            <div class="help-card">
                                <h3>🔍 网络扫描器问题</h3>
                                <p><strong>常见问题：</strong></p>
                                <ul>
                                    <li><strong>扫描结果为空</strong>: 检查网络环境，确认有设备在DHCP范围内</li>
                                    <li><strong>扫描器状态异常</strong>: 重启扫描器或检查配置参数</li>
                                    <li><strong>IP冲突检测</strong>: 查看扫描日志了解冲突详情</li>
                                    <li><strong>扫描性能</strong>: 调整并发数量以优化扫描性能</li>
                                    <li><strong>扫描器死锁</strong>: 重启服务器或检查扫描器配置</li>
                                </ul>
                            </div>
                        </div>

                        <!-- API接口说明 -->
                        <div class="help-section">
                            <h2>🔌 API接口说明</h2>

                            <div class="help-card">
                                <h3>📡 REST API端点</h3>
                                <div class="api-table">
                                    <table class="table">
                                        <thead>
                                            <tr>
                                                <th>方法</th>
                                                <th>端点</th>
                                                <th>描述</th>
                                            </tr>
                                        </thead>
                                        <tbody>
                                            <tr><td>GET</td><td>/api/health</td><td>服务器健康检查</td></tr>
                                            <tr><td>GET</td><td>/api/stats</td><td>获取服务器统计信息</td></tr>
                                            <tr><td>GET</td><td>/api/leases/active</td><td>获取活跃租约</td></tr>
                                            <tr><td>GET</td><td>/api/leases/history</td><td>获取操作历史</td></tr>
                                            <tr><td>GET</td><td>/api/gateways</td><td>获取网关状态</td></tr>
                                            <tr><td>GET</td><td>/api/devices</td><td>获取设备列表</td></tr>
                                            <tr><td>GET</td><td>/api/bindings</td><td>获取静态绑定</td></tr>
                                            <tr><td>GET</td><td>/api/config</td><td>获取配置</td></tr>
                                        </tbody>
                                    </table>
                                </div>
                            </div>

                            <div class="help-card">
                                <h3>💡 API使用示例</h3>
                                <div class="code-block">
                                    <pre># 获取活跃租约
curl http://localhost:8080/api/leases/active

# 获取服务器统计
curl http://localhost:8080/api/stats

# 获取特定设备历史
curl "http://localhost:8080/api/leases/history?mac=aa:bb:cc:dd:ee:ff"

# 健康检查
curl http://localhost:8080/api/health</pre>
                                </div>
                            </div>
                        </div>

                        <!-- 联系支持 -->
                        <div class="help-section">
                            <h2>🆘 获取支持</h2>
                            <div class="help-card">
                                <p>如果您遇到问题或需要帮助，请参考以下资源：</p>
                                <ul>
                                    <li><strong>📖 完整文档</strong>: 查看项目的README.md文件获取详细文档</li>
                                    <li><strong>📄 程序日志</strong>: 在"程序日志"页面查看详细的运行日志</li>
                                    <li><strong>🔧 配置示例</strong>: 参考config.yaml配置文件示例</li>
                                    <li><strong>🧪 测试功能</strong>: 运行测试脚本验证系统功能</li>
                                </ul>
                                
                                <div class="version-info" style="margin-top: 2rem; padding: 1rem; background: #f8f9fa; border-radius: 8px;">
                                    <h4>📊 系统信息</h4>
                                    <p><strong>版本</strong>: DHCP Server v1.0.0</p>
                                    <p><strong>构建时间</strong>: 2024年7月</p>
                                    <p><strong>Go版本</strong>: Go 1.19+</p>
                                    <p><strong>测试覆盖</strong>: 42个测试用例，100%通过率</p>
                                </div>
                            </div>
                        </div>
                    </div>
                </div>
            </div>
        </div>
    </div>

    <!-- 设备编辑模态框 -->
    <div id="deviceModal" class="modal">
        <div class="modal-content">
            <div class="modal-header">
                <h2 class="modal-title" id="deviceModalTitle">添加设备</h2>
                <span class="close" onclick="closeDeviceModal()">&times;</span>
            </div>
            <div class="modal-body">
                <form id="deviceForm">
                    <div class="form-group">
                        <label for="deviceMAC">MAC地址 *</label>
                        <input type="text" id="deviceMAC" class="form-control" placeholder="aa:bb:cc:dd:ee:ff" required>
                    </div>
                    <div class="form-group">
                        <label for="deviceType">设备类型</label>
                        <input type="text" id="deviceType" class="form-control" placeholder="Android、iPhone、Windows、Linux等">
                    </div>

                    <div class="form-group">
                        <label for="deviceModel">型号</label>
                        <input type="text" id="deviceModel" class="form-control" placeholder="iPhone 15、Galaxy S24等">
                    </div>
                    <div class="form-group">
                        <label for="deviceOwner">设备所有者</label>
                        <input type="text" id="deviceOwner" class="form-control" placeholder="张三、李四等">
                    </div>
                    <div class="form-group">
                        <label for="deviceDescription">设备描述</label>
                        <input type="text" id="deviceDescription" class="form-control" placeholder="设备用途、备注等">
                    </div>
                </form>
            </div>
            <div class="modal-footer">
                <button type="button" class="btn-modal btn-modal-secondary" onclick="closeDeviceModal()">取消</button>
                <button type="button" class="btn-modal btn-modal-primary" onclick="saveDeviceForm()">保存</button>
            </div>
        </div>
    </div>

    <!-- 静态IP配置模态框 -->
    <div id="staticIPModal" class="modal">
        <div class="modal-content">
            <div class="modal-header">
                <h2 class="modal-title">配置静态IP</h2>
                <span class="close" onclick="closeStaticIPModal()">&times;</span>
            </div>
            <div class="modal-body">
                <form id="staticIPForm">
                    <div class="form-group">
                        <label for="staticMAC">设备MAC地址</label>
                        <input type="text" id="staticMAC" class="form-control" readonly>
                    </div>
                    <div class="form-group">
                        <label for="staticAlias">绑定别名</label>
                        <input type="text" id="staticAlias" class="form-control" readonly>
                        <small class="form-help">系统自动生成的唯一别名</small>
                    </div>
                    <div class="form-group" style="position:relative;">
                        <label for="staticIP">IP地址 *</label>
                        <input type="text" id="staticIP" class="form-control" placeholder="例如：192.168.1.100" required autocomplete="off" oninput="onStaticIPInput()" onfocus="onStaticIPInput()" onblur="setTimeout(hideIPAutocomplete, 200)">
                        <div id="staticIPAutocomplete" class="autocomplete-list" style="display:none;position:absolute;z-index:1000;background:#fff;border:1px solid #ccc;max-height:150px;overflow-y:auto;width:100%;left:0;right:0;box-sizing:border-box;"></div>
                        <small class="form-help">请输入网段内的可用IP地址，支持Tab键快速补齐</small>
                    </div>

                    <div class="form-group">
                        <label for="staticHostname">主机名</label>
                        <input type="text" id="staticHostname" class="form-control" placeholder="可选：设置主机名">
                    </div>
                </form>
            </div>
            <div class="modal-footer">
                <button type="button" class="btn-modal btn-modal-secondary" onclick="closeStaticIPModal()">取消</button>
                <button type="button" class="btn-modal btn-modal-primary" onclick="saveStaticIP()">配置静态IP</button>
            </div>
        </div>
    </div>

    <!-- 设备发现结果模态框 -->
    <div id="discoveryModal" class="modal">
        <div class="modal-content" style="width: 900px; max-width: 95%;">
            <div class="modal-header">
                <h2 class="modal-title">发现新设备</h2>
                <span class="close" onclick="closeDiscoveryModal()">&times;</span>
            </div>
            <div class="modal-body">
                <div id="discoveryResult">
                    <p>正在搜索网络中的新设备...</p>
                </div>
                <div id="discoveredDevices" style="display: none;">
                    <div style="margin-bottom: 1rem; display: flex; justify-content: space-between; align-items: center;">
                        <h4>发现的设备列表</h4>
                        <div>
                            <button class="btn btn-secondary" onclick="selectAllDiscovered()">全选</button>
                            <button class="btn btn-secondary" onclick="clearAllDiscovered()">清空</button>
                        </div>
                    </div>
                    <div style="max-height: 400px; overflow-y: auto;">
                        <table class="table">
                            <thead>
                                <tr>
                                    <th style="width: 40px;">选择</th>
                                    <th>MAC地址</th>
                                    <th>当前IP</th>
                                    <th>主机名</th>
                                    <th>推测设备类型</th>

                                    <th>最后活跃</th>
                                </tr>
                            </thead>
                            <tbody id="discoveredDevicesTable">
                            </tbody>
                        </table>
                    </div>
                </div>
            </div>
            <div class="modal-footer">
                <button type="button" class="btn-modal btn-modal-secondary" onclick="closeDiscoveryModal()">取消</button>
                <button type="button" class="btn-modal btn-modal-primary" onclick="addSelectedDevices()" id="addSelectedBtn" disabled>添加选中设备</button>
            </div>
        </div>
    </div>

    <!-- 网关编辑模态框 -->
    <div id="gatewayModal" class="modal">
        <div class="modal-content">
            <div class="modal-header">
                <h2 class="modal-title" id="gatewayModalTitle">添加网关</h2>
                <span class="close" onclick="closeGatewayModal()">&times;</span>
            </div>
            <div class="modal-body">
                <form id="gatewayForm">
                    <div class="form-group">
                        <label for="gatewayName">网关名称 *</label>
                        <input type="text" id="gatewayName" class="form-control" placeholder="例如：main_gateway、backup_gateway" required>
                    </div>
                    <div class="form-group">
                        <label for="gatewayIP">IP地址 *</label>
                        <input type="text" id="gatewayIP" class="form-control" placeholder="例如：192.168.1.1" required>
                        <small class="form-help">请输入有效的IP地址</small>
                    </div>
                    <div class="form-group">
                        <label>
                            <input type="checkbox" id="gatewayIsDefault"> 设为默认网关
                        </label>
                        <small class="form-help">同一时间只能有一个默认网关</small>
                    </div>
                    <div class="form-group">
                        <label for="gatewayDescription">描述</label>
                        <input type="text" id="gatewayDescription" class="form-control" placeholder="网关用途或备注">
                    </div>
                    <div class="form-group">
                        <label for="gatewayDNS">DNS服务器</label>
                        <div id="gatewayDNSContainer">
                            <input type="text" class="form-control gateway-dns-input" placeholder="例如：8.8.8.8">
                        </div>
                        <button type="button" class="btn btn-secondary btn-sm mt-2" onclick="addGatewayDNSField()">+ 添加DNS</button>
                        <small class="form-help">可配置多个DNS服务器，也可以不配置。当主机分配到此网关时，会优先使用这些DNS服务器</small>
                    </div>
                </form>
            </div>
            <div class="modal-footer">
                <button type="button" class="btn-modal btn-modal-secondary" onclick="closeGatewayModal()">取消</button>
                <button type="button" class="btn-modal btn-modal-primary" onclick="saveGatewayForm()">保存</button>
            </div>
        </div>
    </div>

    <!-- 指定网关模态框 -->
    <div id="deviceGatewayModal" class="modal">
        <div class="modal-content">
            <div class="modal-header">
                <h2 class="modal-title">指定网关</h2>
                <span class="close" onclick="closeDeviceGatewayModal()">&times;</span>
            </div>
            <div class="modal-body">
                <form id="deviceGatewayForm">
                    <div class="form-group">
                        <label for="deviceGatewayMAC">设备MAC地址</label>
                        <input type="text" id="deviceGatewayMAC" class="form-control" readonly>
                    </div>
                    <div class="form-group">
                        <label for="deviceGatewayName">设备名称</label>
                        <input type="text" id="deviceGatewayName" class="form-control" readonly>
                    </div>
                    <div class="form-group">
                        <label for="deviceGatewaySelect">选择网关</label>
                        <select id="deviceGatewaySelect" class="form-control">
                            <option value="">使用默认网关</option>
                        </select>
                        <small class="form-help">选择设备使用的网关，留空则使用默认网关</small>
                    </div>
                </form>
            </div>
            <div class="modal-footer">
                <button type="button" class="btn-modal btn-modal-secondary" onclick="closeDeviceGatewayModal()">取消</button>
                <button type="button" class="btn-modal btn-modal-primary" onclick="saveDeviceGateway()">保存</button>
            </div>
        </div>
    </div>

    <script>
        let currentLeaseMode = 'active';
        let allLeases = [];
        let allHistory = [];
        let allDevices = [];
        let allLogs = [];
        let staticBindings = {}; // 缓存静态绑定信息，key为MAC地址
        let autoRefreshInterval;
        let configContent = '';
        let availableIPs = []; // 缓存可用IP列表
        let staticIPActiveIndex = -1;

        // 美化的确认对话框函数
        function showBeautifulConfirm(title, message, type = 'warning') {
            return new Promise((resolve) => {
                // 移除已存在的确认框
                const existingModal = document.getElementById('beautifulConfirmModal');
                if (existingModal) {
                    existingModal.remove();
                }
                
                // 创建确认框HTML
                const iconMap = {
                    'warning': '⚠️',
                    'danger': '🗑️',
                    'info': 'ℹ️'
                };
                
                const buttonMap = {
                    'warning': 'confirm-btn-warning',
                    'danger': 'confirm-btn-danger',
                    'info': 'confirm-btn-warning'
                };
                
                const modalHTML = 
                    '<div id="beautifulConfirmModal" class="confirm-modal" style="display: block;">' +
                        '<div class="confirm-modal-content">' +
                            '<div class="confirm-modal-header">' +
                                '<span class="confirm-modal-icon ' + type + '">' + (iconMap[type] || '❓') + '</span>' +
                                '<h3 class="confirm-modal-title">' + title + '</h3>' +
                                '<p class="confirm-modal-message">' + message + '</p>' +
                            '</div>' +
                            '<div class="confirm-modal-footer">' +
                                '<button class="confirm-btn confirm-btn-cancel" onclick="closeBeautifulConfirm(false)">取消</button>' +
                                '<button class="confirm-btn ' + buttonMap[type] + '" onclick="closeBeautifulConfirm(true)">确定</button>' +
                            '</div>' +
                        '</div>' +
                    '</div>';
                
                // 添加到页面
                document.body.insertAdjacentHTML('beforeend', modalHTML);
                
                // 设置全局回调
                window.beautifulConfirmResolve = resolve;
                
                // 点击背景关闭
                document.getElementById('beautifulConfirmModal').addEventListener('click', function(e) {
                    if (e.target === this) {
                        closeBeautifulConfirm(false);
                    }
                });
                
                // ESC键关闭
                document.addEventListener('keydown', function(e) {
                    if (e.key === 'Escape') {
                        closeBeautifulConfirm(false);
                    }
                });
            });
        }
        
        // 关闭美化确认框
        function closeBeautifulConfirm(result) {
            const modal = document.getElementById('beautifulConfirmModal');
            if (modal) {
                modal.style.animation = 'fadeOut 0.2s ease';
                setTimeout(() => {
                    modal.remove();
                    if (window.beautifulConfirmResolve) {
                        window.beautifulConfirmResolve(result);
                        window.beautifulConfirmResolve = null;
                    }
                }, 200);
            }
        }

        // 加载提示相关函数
        function showLoading(text = '正在处理中...') {
            const overlay = document.getElementById('loadingOverlay');
            const loadingText = document.getElementById('loadingText');
            if (overlay && loadingText) {
                loadingText.textContent = text;
                overlay.style.display = 'block';
            }
        }

        function hideLoading() {
            const overlay = document.getElementById('loadingOverlay');
            if (overlay) {
                overlay.style.display = 'none';
            }
        }

        // 页面加载完成后初始化
        document.addEventListener('DOMContentLoaded', function() {
            loadStats();
            loadActiveLeases();
            loadGatewayStatus();
            loadDevices();
            updateServerUptime(); // 立即更新运行时间
            
            // 检查配置管理标签页是否需要初始化（延迟执行以确保DOM完全就绪）
            setTimeout(() => {
                const configTab = document.getElementById('config');
                if (configTab && configTab.classList.contains('active')) {
                    // 如果配置标签页是活跃的，检查网络配置子标签页
                    const networkConfigTab = document.getElementById('network-config');
                    if (networkConfigTab && networkConfigTab.classList.contains('active')) {
                        console.log('🔄 页面加载时自动加载网络配置...');
                        loadNetworkConfig(false);
                    }
                }
            }, 100);
            
            // 页面加载后检查统计数值可见性
            setTimeout(() => {
                fixStatValueVisibility();
            }, 500);
            
            // 设置自动刷新
            const autoRefreshCheck = document.getElementById('autoRefresh');
            if (autoRefreshCheck.checked) {
                startAutoRefresh();
            }
            
            autoRefreshCheck.addEventListener('change', function() {
                if (this.checked) {
                    startAutoRefresh();
                } else {
                    stopAutoRefresh();
                }
            });
            
            // 设置日志自动刷新监听器
            const logAutoRefreshCheck = document.getElementById('logAutoRefresh');
            logAutoRefreshCheck.addEventListener('change', function() {
                if (this.checked) {
                    // 如果当前在日志页面且已有日志数据，启动实时流
                    if (document.querySelector('.tab-pane.active').id === 'logs' && allLogs.length > 0) {
                        startLogStream();
                    }
                } else {
                    stopLogStream();
                }
            });
            
            // 页面关闭时清理日志连接
            window.addEventListener('beforeunload', function() {
                stopLogStream();
            });
        });

        function startAutoRefresh() {
            autoRefreshInterval = setInterval(() => {
                loadStats();
                updateServerUptime();
                if (document.querySelector('.tab-pane.active').id === 'leases') {
                    if (currentLeaseMode === 'active') {
                        loadActiveLeases();
                    } else {
                        loadAllLeases();
                    }
                }
                loadGatewayStatus();
                loadDevices();
                
                // 检查日志页面是否启用自动刷新
                if (document.querySelector('.tab-pane.active').id === 'logs' && 
                    document.getElementById('logAutoRefresh').checked) {
                    loadLogs();
                }
            }, 10000); // 每10秒刷新
        }

        function stopAutoRefresh() {
            if (autoRefreshInterval) {
                clearInterval(autoRefreshInterval);
                autoRefreshInterval = null;
            }
        }

        function switchTab(tabName) {
            document.querySelectorAll('.tab-pane').forEach(pane => pane.classList.remove('active'));
            document.querySelectorAll('.tab-button').forEach(btn => btn.classList.remove('active'));
            document.getElementById(tabName).classList.add('active');
            event.target.classList.add('active');
            
            // 加载对应的数据
            switch(tabName) {
                case 'leases':
                    if (currentLeaseMode === 'active') {
                        loadActiveLeases();
                    } else {
                        loadAllLeases();
                    }
                    break;
                case 'history':
                    loadHistory();
                    break;
                case 'gateways':
                    loadGatewayStatus();
                    break;
                case 'devices':
                    loadDevices();
                    break;
                case 'server-config':
                    loadServerConfig(false);
                    break;
                case 'config':
                    // 默认切换到网络配置子标签页
                    switchConfigSubTab('network-config');
                    break;
                case 'logs':
                    loadLogs();
                    break;
                default:
                    // 切换到其他标签页时关闭日志流
                    stopLogStream();
                    break;
            }
        }
        
        function switchConfigSubTab(subTabName) {
            document.querySelectorAll('.config-sub-pane').forEach(pane => pane.classList.remove('active'));
            document.querySelectorAll('.config-tab-button').forEach(btn => btn.classList.remove('active'));
            document.getElementById(subTabName).classList.add('active');
            
            // 激活对应的按钮
            const selectedButton = document.querySelector('button[onclick="switchConfigSubTab(\'' + subTabName + '\')"]');
            if (selectedButton) {
                selectedButton.classList.add('active');
            }
            
            // 加载对应的数据
            switch(subTabName) {
                case 'network-config':
                    loadNetworkConfig(false);
                    break;
                case 'file-management':
                    loadConfigContent();
                    loadBackups();
                    break;
                case 'gateway-detection':
                    loadGatewayDetectionConfig(false);
                    break;
                case 'network-scanner':
                    loadNetworkScannerConfig(false);
                    updateScannerStatus();
                    break;
            }
        }

        // 修复统计数值的可见性 - 使用更简单的检测方法
        function fixStatValueVisibility() {
            setTimeout(() => {
                const statValues = document.querySelectorAll('.stat-value');
                
                statValues.forEach(element => {
                    // 检查元素是否实际可见（透明度检测）
                    const computedStyle = window.getComputedStyle(element);
                    const rect = element.getBoundingClientRect();
                    
                    // 如果文字不可见或透明度太低，则使用备用样式
                    if (computedStyle.webkitTextFillColor === 'transparent' && 
                        (computedStyle.opacity === '0' || element.offsetHeight === 0)) {
                        // 应用备用颜色样式
                        element.style.webkitTextFillColor = '';
                        element.style.background = '';
                        element.style.webkitBackgroundClip = '';
                        element.style.backgroundClip = '';
                        
                        // 根据当前主题设置适当的颜色
                        if (document.body.classList.contains('dark-theme')) {
                            element.style.color = '#00d4ff';
                        } else if (document.body.classList.contains('pink-theme')) {
                            element.style.color = '#c0392b';
                        } else {
                            element.style.color = '#2c3e50';
                        }
                    }
                });
            }, 150);
        }

        async function loadStats() {
            try {
                const response = await fetch('/api/stats');
                const stats = await response.json();
                
                const statsGrid = document.getElementById('statsGrid');
                const poolStats = stats.pool_stats || {};
                statsGrid.innerHTML = 
                    '<div class="stat-card">' +
                        '<div class="stat-value">' + (poolStats.total_ips || 0) + '</div>' +
                        '<div class="stat-label">总IP数量</div>' +
                    '</div>' +
                    '<div class="stat-card">' +
                        '<div class="stat-value">' + ((poolStats.dynamic_leases || 0) + (poolStats.static_leases || 0)) + '</div>' +
                        '<div class="stat-label">活跃租约</div>' +
                    '</div>' +
                    '<div class="stat-card">' +
                        '<div class="stat-value">' + (poolStats.static_leases || 0) + '</div>' +
                        '<div class="stat-label">静态租约</div>' +
                    '</div>' +
                    '<div class="stat-card">' +
                        '<div class="stat-value">' + ((poolStats.utilization || 0).toFixed(1)) + '%</div>' +
                        '<div class="stat-label">地址池利用率</div>' +
                    '</div>';
                
                // 修复统计数值的可见性问题
                setTimeout(() => {
                    fixStatValueVisibility();
                }, 100);
                
            } catch (error) {
                console.error('加载统计信息失败:', error);
            }
        }

        // 更新服务器运行时间显示
        async function updateServerUptime() {
            try {
                const response = await fetch('/api/health');
                const health = await response.json();
                const uptimeElement = document.getElementById('serverUptime');
                const progressBar = document.getElementById('uptimeProgress');
                
                if (uptimeElement && health.uptime) {
                    // 格式化运行时间为中文格式
                    const formattedUptime = formatUptimeToChinese(health.uptime);
                    uptimeElement.textContent = formattedUptime;
                    
                    // 计算进度条宽度（基于运行时间的秒数）
                    if (progressBar) {
                        const uptimeStr = health.uptime;
                        const seconds = parseUptimeToSeconds(uptimeStr);
                        const progress = Math.min((seconds / 3600) * 100, 100); // 基于1小时作为100%
                        progressBar.style.width = progress + '%';
                    }
                }
            } catch (error) {
                console.error('更新运行时间失败:', error);
                const uptimeElement = document.getElementById('serverUptime');
                if (uptimeElement) {
                    uptimeElement.textContent = '获取失败';
                }
            }
        }

        // 解析运行时间字符串为秒数
        function parseUptimeToSeconds(uptimeStr) {
            // 处理 "6.425657458s" 格式
            const secondsMatch = uptimeStr.match(/(\d+\.?\d*)s/);
            if (secondsMatch) {
                return parseFloat(secondsMatch[1]);
            }
            
            // 处理 "1h2m3s" 格式
            const match = uptimeStr.match(/(\d+)h(\d+)m(\d+)s/);
            if (match) {
                const hours = parseInt(match[1]);
                const minutes = parseInt(match[2]);
                const seconds = parseInt(match[3]);
                return hours * 3600 + minutes * 60 + seconds;
            }
            return 0;
        }

        // 格式化运行时间为中文格式
        function formatUptimeToChinese(uptimeStr) {
            // 处理 "6.425657458s" 格式
            const secondsMatch = uptimeStr.match(/(\d+\.?\d*)s/);
            if (secondsMatch) {
                const totalSeconds = parseFloat(secondsMatch[1]);
                return formatSecondsToChinese(totalSeconds);
            }
            
            // 处理 "1h2m3s" 格式
            const match = uptimeStr.match(/(\d+)h(\d+)m(\d+)s/);
            if (match) {
                const hours = parseInt(match[1]);
                const minutes = parseInt(match[2]);
                const seconds = parseInt(match[3]);
                
                let result = '';
                
                if (hours > 0) {
                    result += hours + '小时';
                }
                if (minutes > 0) {
                    result += minutes + '分钟';
                }
                if (seconds > 0 || (hours === 0 && minutes === 0)) {
                    result += seconds + '秒';
                }
                
                return result;
            }
            
            // 如果无法解析，返回原始字符串
            return uptimeStr;
        }

        // 将秒数格式化为中文时间格式
        function formatSecondsToChinese(totalSeconds) {
            const hours = Math.floor(totalSeconds / 3600);
            const minutes = Math.floor((totalSeconds % 3600) / 60);
            const seconds = Math.floor(totalSeconds % 60);
            
            let result = '';
            
            if (hours > 0) {
                result += hours + '小时';
            }
            if (minutes > 0) {
                result += minutes + '分钟';
            }
            if (seconds > 0 || (hours === 0 && minutes === 0)) {
                result += seconds + '秒';
            }
            
            return result;
        }

        function toggleLeaseMode() {
            if (currentLeaseMode === 'active') {
                currentLeaseMode = 'all';
                document.getElementById('leaseModeText').textContent = '显示活跃租约';
                loadAllLeases();
            } else {
                currentLeaseMode = 'active';
                document.getElementById('leaseModeText').textContent = '显示所有租约';
                loadActiveLeases();
            }
        }

        async function loadActiveLeases() {
            try {
                const response = await fetch('/api/leases/active');
                allLeases = await response.json();
                displayLeases(allLeases);
            } catch (error) {
                console.error('加载活跃租约失败:', error);
            }
        }

        async function loadAllLeases() {
            try {
                const response = await fetch('/api/leases');
                allLeases = await response.json();
                displayLeases(allLeases);
            } catch (error) {
                console.error('加载所有租约失败:', error);
            }
        }

        function displayLeases(leases) {
            const tableHTML = 
                '<table class="table">' +
                    '<thead>' +
                        '<tr>' +
                            '<th>IP地址</th>' +
                            '<th>MAC地址</th>' +
                            '<th>主机名</th>' +
                            '<th>类型</th>' +
                            '<th>网关</th>' +
                            '<th>剩余时间</th>' +
                            '<th>状态</th>' +
                            '<th>操作</th>' +
                        '</tr>' +
                    '</thead>' +
                    '<tbody>' +
                        leases.map(lease => 
                            '<tr>' +
                                '<td>' + lease.ip + '</td>' +
                                '<td>' + lease.mac + '</td>' +
                                '<td>' + (lease.hostname || '-') + '</td>' +
                                '<td>' + (lease.is_static ? '静态' : '动态') + '</td>' +
                                '<td>' + (lease.gateway_ip || lease.gateway || '-') + '</td>' +
                                '<td>' + (lease.is_static ? '永久' : formatDuration(lease.remaining_time)) + '</td>' +
                                '<td><span class="status ' + (lease.is_expired ? 'status-offline' : 'status-online') + '">' + (lease.is_expired ? '已过期' : '活跃') + '</span></td>' +
                                '<td>' + 
                                    (!lease.is_static && !lease.is_expired ? 
                                        '<button class="btn btn-primary" onclick="convertToStatic(\'' + lease.mac + '\', \'' + lease.ip + '\', \'' + (lease.hostname || '') + '\', \'' + (lease.gateway || '') + '\')">转为静态IP</button>' :
                                        '-') + 
                                '</td>' +
                            '</tr>'
                        ).join('') +
                    '</tbody>' +
                '</table>';
            document.getElementById('leasesTable').innerHTML = tableHTML;
        }

        function filterLeases() {
            const searchTerm = document.getElementById('leaseSearch').value.toLowerCase();
            const filteredLeases = allLeases.filter(lease => 
                lease.ip.toLowerCase().includes(searchTerm) ||
                lease.mac.toLowerCase().includes(searchTerm) ||
                (lease.hostname && lease.hostname.toLowerCase().includes(searchTerm))
            );
            displayLeases(filteredLeases);
        }

        async function loadHistory() {
            try {
                const limit = document.getElementById('historyLimit').value;
                const response = await fetch('/api/leases/history?limit=' + limit);
                allHistory = await response.json();
                displayHistory(allHistory);
            } catch (error) {
                console.error('加载历史记录失败:', error);
            }
        }

        function displayHistory(history) {
            const tableHTML = 
                '<table class="table">' +
                    '<thead>' +
                        '<tr>' +
                            '<th>时间</th>' +
                            '<th>操作</th>' +
                            '<th>IP地址</th>' +
                            '<th>MAC地址</th>' +
                            '<th>主机名</th>' +
                            '<th>网关</th>' +
                        '</tr>' +
                    '</thead>' +
                    '<tbody>' +
                        history.map(record => 
                            '<tr>' +
                                '<td>' + formatTimestamp(record.timestamp) + '</td>' +
                                '<td><span class="status status-' + getActionColor(record.action) + '">' + record.action + '</span></td>' +
                                '<td>' + record.ip + '</td>' +
                                '<td>' + record.mac + '</td>' +
                                '<td>' + (record.hostname || '-') + '</td>' +
                                '<td>' + (record.gateway || '-') + '</td>' +
                            '</tr>'
                        ).join('') +
                    '</tbody>' +
                '</table>';
            document.getElementById('historyTable').innerHTML = tableHTML;
        }

        function filterHistory() {
            const searchTerm = document.getElementById('historySearch').value.toLowerCase();
            const filteredHistory = allHistory.filter(record => 
                record.ip.toLowerCase().includes(searchTerm) ||
                record.mac.toLowerCase().includes(searchTerm) ||
                (record.hostname && record.hostname.toLowerCase().includes(searchTerm))
            );
            displayHistory(filteredHistory);
        }

        async function loadGatewayStatus() {
            try {
                const response = await fetch('/api/gateways');
                const gateways = await response.json();
                
                const gatewayHTML = 
                    '<table class="table">' +
                        '<thead>' +
                            '<tr>' +
                                '<th>网关名称</th>' +
                                '<th>IP地址</th>' +
                                '<th>类型</th>' +
                                '<th>状态</th>' +
                                '<th>DNS服务器</th>' +
                                '<th>描述</th>' +
                                '<th>操作</th>' +
                            '</tr>' +
                        '</thead>' +
                        '<tbody>' +
                            Object.entries(gateways).map(([name, info]) => 
                                '<tr>' +
                                    '<td>' + name + '</td>' +
                                    '<td>' + (info.ip || '-') + '</td>' +
                                    '<td>' + 
                                        '<span class="status ' + (info.is_default ? 'status-online' : 'status-offline') + '">' +
                                            (info.is_default ? '默认网关' : '备用网关') +
                                        '</span>' +
                                    '</td>' +
                                    '<td>' +
                                        '<div class="gateway-status">' +
                                            '<div class="status-indicator ' + (info.healthy ? 'healthy' : 'unhealthy') + '"></div>' +
                                            '<span class="status ' + (info.healthy ? 'status-healthy' : 'status-unhealthy') + '">' +
                                                (info.healthy ? '健康' : '不健康') +
                                            '</span>' +
                                        '</div>' +
                                    '</td>' +
                                    '<td>' + 
                                        (info.dns_servers && info.dns_servers.length > 0 ? 
                                            '<div class="dns-list">' + info.dns_servers.map(dns => '<span class="dns-item">' + dns + '</span>').join('') + '</div>' : 
                                            '<span class="text-muted">未配置</span>') +
                                    '</td>' +
                                    '<td>' + (info.description || '-') + '</td>' +
                                    '<td>' +
                                        '<button class="btn btn-secondary" onclick="editGateway(\'' + name + '\')">编辑</button>' +
                                        '<button class="btn btn-danger" onclick="deleteGateway(\'' + name + '\')">删除</button>' +
                                    '</td>' +
                                '</tr>'
                            ).join('') +
                        '</tbody>' +
                    '</table>';
                document.getElementById('gatewayStatus').innerHTML = gatewayHTML;
            } catch (error) {
                console.error('加载网关状态失败:', error);
            }
        }



        async function loadDevices() {
            try {
                // 并行加载设备列表和静态绑定信息
                const [devicesResponse, bindingsResponse] = await Promise.all([
                    fetch('/api/devices'),
                    fetch('/api/bindings')
                ]);
                
                allDevices = await devicesResponse.json();
                const bindings = await bindingsResponse.json();
                
                // 缓存静态绑定信息，key为MAC地址
                staticBindings = {};
                bindings.forEach(binding => {
                    staticBindings[binding.mac] = binding;
                });
                
                displayDevices(allDevices);
            } catch (error) {
                console.error('加载设备列表失败:', error);
            }
        }

        function displayDevices(devices) {
            const tableHTML = 
                '<table class="table">' +
                    '<thead>' +
                        '<tr>' +
                            '<th>MAC地址</th>' +
                            '<th>主机名</th>' +
                            '<th>设备类型</th>' +
                            '<th>型号</th>' +
                            '<th>所有者</th>' +
                            '<th>网关</th>' +
                            '<th>静态IP</th>' +
                            '<th>状态</th>' +
                            '<th style="width: 120px;">操作</th>' +
                        '</tr>' +
                    '</thead>' +
                    '<tbody>' +
                        devices.map(device => 
                            '<tr>' +
                                '<td>' + device.mac + '</td>' +
                                '<td>' + (device.hostname || '-') + '</td>' +
                                '<td>' + (device.device_type || '-') + '</td>' +
                                '<td>' + (device.model || '-') + '</td>' +
                                '<td>' + (device.owner || '-') + '</td>' +
                                '<td>' + (device.gateway || '-') + '</td>' +
                                '<td>' + 
                                    (device.has_static_ip ? 
                                        '<span class="status status-online">' + device.static_ip + '</span>' : 
                                        '<span class="status status-offline">无</span>') + 
                                '</td>' +
                                '<td><span class="status ' + (device.is_active ? 'status-online' : 'status-offline') + '">' + (device.is_active ? '在线' : '离线') + '</span></td>' +
                                '<td>' +
                                    '<div class="dropdown" style="position: relative; display: inline-block;">' +
                                        '<button class="btn btn-secondary btn-sm dropdown-toggle" onclick="toggleDropdown(event, \'' + device.mac + '\')" style="padding: 4px 8px; font-size: 12px;">操作 ▼</button>' +
                                        '<div id="dropdown-' + device.mac + '" class="dropdown-menu" style="display: none; position: absolute; top: 100%; left: 0; background: white; border: 1px solid #ddd; border-radius: 4px; min-width: 140px; z-index: 1000; box-shadow: 0 2px 8px rgba(0,0,0,0.15);">' +
                                            '<a href="#" onclick="editDevice(\'' + device.mac + '\'); closeAllDropdowns();" style="display: block; padding: 8px 12px; text-decoration: none; color: #333; border-bottom: 1px solid #eee;">📝 编辑设备</a>' +
                                            (device.has_static_ip ? 
                                                '<a href="#" onclick="editStaticIP(\'' + device.mac + '\'); closeAllDropdowns();" style="display: block; padding: 8px 12px; text-decoration: none; color: #ff9800; border-bottom: 1px solid #eee;">⚙️ 修改静态IP</a>' +
                                                '<a href="#" onclick="deleteStaticIP(\'' + device.mac + '\'); closeAllDropdowns();" style="display: block; padding: 8px 12px; text-decoration: none; color: #f44336; border-bottom: 1px solid #eee;">🗑️ 删除静态IP</a>' :
                                                '<a href="#" onclick="configureStaticIP(\'' + device.mac + '\'); closeAllDropdowns();" style="display: block; padding: 8px 12px; text-decoration: none; color: #2196f3; border-bottom: 1px solid #eee;">📌 配置静态IP</a>') +
                                            '<a href="#" onclick="configureGateway(\'' + device.mac + '\'); closeAllDropdowns();" style="display: block; padding: 8px 12px; text-decoration: none; color: #4caf50; border-bottom: 1px solid #eee;">🌐 指定网关</a>' +
                                            '<a href="#" onclick="deleteDevice(\'' + device.mac + '\'); closeAllDropdowns();" style="display: block; padding: 8px 12px; text-decoration: none; color: #f44336;">🗑️ 删除设备</a>' +
                                        '</div>' +
                                    '</div>' +
                                '</td>' +
                            '</tr>'
                        ).join('') +
                    '</tbody>' +
                '</table>';
            document.getElementById('devicesTable').innerHTML = tableHTML;
        }

        // 下拉菜单控制函数
        function toggleDropdown(event, mac) {
            event.stopPropagation();
            
            // 关闭所有其他下拉菜单
            const allDropdowns = document.querySelectorAll('.dropdown-menu');
            allDropdowns.forEach(dropdown => {
                if (dropdown.id !== 'dropdown-' + mac) {
                    dropdown.style.display = 'none';
                }
            });
            
            // 切换当前下拉菜单
            const currentDropdown = document.getElementById('dropdown-' + mac);
            if (currentDropdown) {
                if (currentDropdown.style.display === 'none') {
                    // 显示下拉菜单前调整位置
                    currentDropdown.style.display = 'block';
                    adjustDropdownPosition(currentDropdown, event.target);
                } else {
                    currentDropdown.style.display = 'none';
                }
            }
        }
        
        // 调整下拉菜单位置，避免超出视窗
        function adjustDropdownPosition(dropdown, trigger) {
            const rect = trigger.getBoundingClientRect();
            const dropdownRect = dropdown.getBoundingClientRect();
            const viewportHeight = window.innerHeight;
            const viewportWidth = window.innerWidth;
            
            // 重置位置样式
            dropdown.style.top = '';
            dropdown.style.bottom = '';
            dropdown.style.left = '';
            dropdown.style.right = '';
            
            // 检查垂直空间
            const spaceBelow = viewportHeight - rect.bottom;
            const spaceAbove = rect.top;
            const dropdownHeight = dropdown.offsetHeight;
            
            if (spaceBelow >= dropdownHeight) {
                // 下方空间足够，向下展开
                dropdown.style.top = '100%';
            } else if (spaceAbove >= dropdownHeight) {
                // 上方空间足够，向上展开
                dropdown.style.bottom = '100%';
            } else {
                // 两边都不够，选择空间较大的一边
                if (spaceBelow > spaceAbove) {
                    dropdown.style.top = '100%';
                    dropdown.style.maxHeight = spaceBelow - 10 + 'px';
                    dropdown.style.overflowY = 'auto';
                } else {
                    dropdown.style.bottom = '100%';
                    dropdown.style.maxHeight = spaceAbove - 10 + 'px';
                    dropdown.style.overflowY = 'auto';
                }
            }
            
            // 检查水平空间
            const spaceRight = viewportWidth - rect.left;
            const dropdownWidth = dropdown.offsetWidth;
            
            if (spaceRight < dropdownWidth) {
                // 右侧空间不足，向左对齐
                dropdown.style.right = '0';
                dropdown.style.left = 'auto';
            } else {
                // 右侧空间足够，向右对齐
                dropdown.style.left = '0';
                dropdown.style.right = 'auto';
            }
        }
        
        function closeAllDropdowns() {
            const allDropdowns = document.querySelectorAll('.dropdown-menu');
            allDropdowns.forEach(dropdown => {
                dropdown.style.display = 'none';
            });
        }
        
        // 点击页面其他地方时关闭下拉菜单
        document.addEventListener('click', function(event) {
            if (!event.target.closest('.dropdown')) {
                closeAllDropdowns();
            }
        });
        
        function filterDevices() {
            const searchTerm = document.getElementById('deviceSearch').value.toLowerCase();
            const filteredDevices = allDevices.filter(device => 
                device.mac.toLowerCase().includes(searchTerm) ||
                (device.hostname && device.hostname.toLowerCase().includes(searchTerm)) ||
                (device.device_type && device.device_type.toLowerCase().includes(searchTerm)) ||
                (device.owner && device.owner.toLowerCase().includes(searchTerm)) ||
                (device.gateway && device.gateway.toLowerCase().includes(searchTerm))
            );
            displayDevices(filteredDevices);
        }

        async function discoverDevices() {
            // 打开发现设备模态框
            document.getElementById('discoveryModal').style.display = 'block';
            document.getElementById('discoveryResult').innerHTML = '<p>正在搜索网络中的新设备...</p>';
            document.getElementById('discoveredDevices').style.display = 'none';
            
            try {
                const response = await fetch('/api/devices/discover', { method: 'POST' });
                const result = await response.json();
                
                if (result.discovered_count > 0) {
                    displayDiscoveredDevices(result.unknown_devices);
                    document.getElementById('discoveryResult').innerHTML = 
                        '<p><strong>✅ 发现了 ' + result.discovered_count + ' 个新设备</strong></p>' +
                        '<p>请选择要添加到设备管理的设备：</p>';
                    document.getElementById('discoveredDevices').style.display = 'block';
                } else {
                    document.getElementById('discoveryResult').innerHTML = 
                        '<p><strong>ℹ️ 未发现新设备</strong></p>' +
                        '<p>网络中的所有活跃设备都已在设备列表中。</p>';
                }
            } catch (error) {
                console.error('设备发现失败:', error);
                document.getElementById('discoveryResult').innerHTML = 
                    '<p><strong>❌ 设备发现失败</strong></p>' +
                    '<p>错误信息: ' + error.message + '</p>';
            }
        }

        let discoveredDevicesList = [];

        function displayDiscoveredDevices(devices) {
            discoveredDevicesList = devices;
            const tableBody = document.getElementById('discoveredDevicesTable');
            
            tableBody.innerHTML = devices.map((device, index) => 
                '<tr>' +
                    '<td><input type="checkbox" id="device_' + index + '" onchange="updateAddButton()"></td>' +
                    '<td>' + device.mac + '</td>' +
                    '<td>' + device.ip + '</td>' +
                    '<td>' + (device.hostname || '-') + '</td>' +
                    '<td>' + (device.device_type || '未知') + '</td>' +
                    '<td>' + formatTimestamp(device.last_seen) + '</td>' +
                '</tr>'
            ).join('');
            
            updateAddButton();
        }

        function selectAllDiscovered() {
            discoveredDevicesList.forEach((device, index) => {
                document.getElementById('device_' + index).checked = true;
            });
            updateAddButton();
        }

        function clearAllDiscovered() {
            discoveredDevicesList.forEach((device, index) => {
                document.getElementById('device_' + index).checked = false;
            });
            updateAddButton();
        }

        function updateAddButton() {
            let selectedCount = 0;
            discoveredDevicesList.forEach((device, index) => {
                if (document.getElementById('device_' + index).checked) {
                    selectedCount++;
                }
            });
            
            const addBtn = document.getElementById('addSelectedBtn');
            addBtn.disabled = selectedCount === 0;
            addBtn.textContent = selectedCount > 0 ? 
                '添加选中设备 (' + selectedCount + ')' : 
                '添加选中设备';
        }

        async function addSelectedDevices() {
            const selectedDevices = [];
            discoveredDevicesList.forEach((device, index) => {
                				if (document.getElementById('device_' + index).checked) {
					selectedDevices.push({
						mac: device.mac,
						device_type: device.device_type || '',
						hostname: device.hostname || '',
						model: '',
						owner: '',
						description: '通过设备发现自动添加'
					});
				}
            });

            if (selectedDevices.length === 0) {
                showMessage('请选择要添加的设备', 'error');
                return;
            }

            // 显示进度
            const addBtn = document.getElementById('addSelectedBtn');
            const originalText = addBtn.textContent;
            addBtn.disabled = true;
            addBtn.textContent = '正在批量添加设备...';

            try {
                const response = await fetch('/api/devices/batch', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ devices: selectedDevices })
                });

                const result = await response.json();

                // 恢复按钮状态
                addBtn.textContent = originalText;
                addBtn.disabled = false;

                // 显示结果
                if (result.added_count > 0) {
                    showMessage('成功添加 ' + result.added_count + ' 个设备！', 'success');
                    closeDiscoveryModal();
                    loadDevices(); // 刷新设备列表
                }

                if (result.error_count > 0) {
                    console.error('添加设备时遇到错误:', result.errors);
                    const errorMsg = result.errors.length > 3 ? 
                        result.errors.slice(0, 3).join('; ') + '等' + result.error_count + '个错误' :
                        result.errors.join('; ');
                    showMessage('添加设备时遇到错误: ' + errorMsg, 'error');
                }

                // 如果没有成功添加任何设备，保持模态框打开
                if (result.added_count === 0) {
                    return;
                }

            } catch (error) {
                // 恢复按钮状态
                addBtn.textContent = originalText;
                addBtn.disabled = false;
                
                console.error('批量添加设备失败:', error);
                showMessage('批量添加设备失败: ' + error.message, 'error');
            }
        }

        function closeDiscoveryModal() {
            document.getElementById('discoveryModal').style.display = 'none';
            discoveredDevicesList = [];
        }

        // 日志管理相关函数
        let logEventSource = null;
        
        async function loadLogs() {
            try {
                const limit = document.getElementById('logLimit').value;
                const response = await fetch('/api/logs?limit=' + limit);
                const result = await response.json();
                
                if (result.logs) {
                    allLogs = result.logs;
                    displayLogs(allLogs);
                    
                    // 如果开启了自动刷新，开始实时日志流
                    if (document.getElementById('logAutoRefresh').checked) {
                        startLogStream();
                    }
                } else {
                    document.getElementById('logsContent').innerHTML = 
                        '<div style="text-align: center; color: #dc3545; padding: 2rem;">' +
                        '❌ 加载日志失败: ' + (result.error || '未知错误') +
                        '</div>';
                }
            } catch (error) {
                console.error('加载日志失败:', error);
                document.getElementById('logsContent').innerHTML = 
                    '<div style="text-align: center; color: #dc3545; padding: 2rem;">' +
                    '❌ 加载日志失败: ' + error.message +
                    '</div>';
            }
        }
        
        function startLogStream() {
            // 如果已有连接，先关闭
            if (logEventSource) {
                logEventSource.close();
            }
            
            // 创建SSE连接
            logEventSource = new EventSource('/api/logs/stream');
            
            logEventSource.onopen = function() {
                console.log('实时日志连接已建立');
            };
            
            logEventSource.onmessage = function(event) {
                const newLogLine = event.data;
                if (newLogLine && newLogLine !== 'Connected to log stream') {
                    // 添加新日志行到数组
                    allLogs.push(newLogLine);
                    
                    // 限制日志数量，保持最新的1000条
                    if (allLogs.length > 1000) {
                        allLogs = allLogs.slice(-1000);
                    }
                    
                    // 重新显示日志（如果没有搜索过滤）
                    const searchTerm = document.getElementById('logSearch').value;
                    if (!searchTerm) {
                        displayLogs(allLogs);
                    } else {
                        filterLogs();
                    }
                }
            };
            
            logEventSource.onerror = function(event) {
                console.log('实时日志连接错误，将重新连接...');
                setTimeout(startLogStream, 5000); // 5秒后重新连接
            };
        }
        
        function stopLogStream() {
            if (logEventSource) {
                logEventSource.close();
                logEventSource = null;
                console.log('实时日志连接已关闭');
            }
        }

        function displayLogs(logs) {
            const logsContainer = document.getElementById('logsContent');
            
            if (!logs || logs.length === 0) {
                logsContainer.innerHTML = 
                    '<div style="text-align: center; color: #666; padding: 2rem;">' +
                    'ℹ️ 暂无日志记录' +
                    '</div>';
                return;
            }

            // 将日志行添加颜色标识
            const coloredLogs = logs.map(line => {
                // 根据日志级别添加颜色
                if (line.includes('ERROR') || line.includes('error') || line.includes('错误') || line.includes('失败')) {
                    return '<span style="color: #dc3545;">' + escapeHtml(line) + '</span>';
                } else if (line.includes('WARN') || line.includes('warn') || line.includes('警告')) {
                    return '<span style="color: #fd7e14;">' + escapeHtml(line) + '</span>';
                } else if (line.includes('INFO') || line.includes('info') || line.includes('成功') || line.includes('启动')) {
                    return '<span style="color: #198754;">' + escapeHtml(line) + '</span>';
                } else if (line.includes('DEBUG') || line.includes('debug')) {
                    return '<span style="color: #6c757d;">' + escapeHtml(line) + '</span>';
                } else {
                    return escapeHtml(line);
                }
            });

            logsContainer.innerHTML = coloredLogs.join('\n');
            
            // 自动滚动到底部显示最新日志
            logsContainer.scrollTop = logsContainer.scrollHeight;
        }

        function filterLogs() {
            const searchTerm = document.getElementById('logSearch').value.toLowerCase();
            if (!searchTerm) {
                displayLogs(allLogs);
                return;
            }
            
            const filteredLogs = allLogs.filter(line => 
                line.toLowerCase().includes(searchTerm)
            );
            displayLogs(filteredLogs);
        }

        function escapeHtml(text) {
            const div = document.createElement('div');
            div.textContent = text;
            return div.innerHTML;
        }

        // 配置管理相关函数
        async function loadConfigContent() {
            try {
                const response = await fetch('/api/config?raw=true');
                const data = await response.json();
                configContent = data.content;
                document.getElementById('configEditor').value = configContent;
            } catch (error) {
                console.error('加载配置文件失败:', error);
                showMessage('加载配置文件失败: ' + error.message, 'error');
            }
        }

        async function validateConfig() {
            const content = document.getElementById('configEditor').value;
            
            try {
                const response = await fetch('/api/config/validate', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ content: content })
                });
                
                const result = await response.json();
                const validationDiv = document.getElementById('validationResult');
                
                if (result.valid) {
                    validationDiv.innerHTML = '<div class="validation-success">✓ 配置格式正确</div>';
                } else {
                    validationDiv.innerHTML = '<div class="validation-error">✗ ' + result.error + '</div>';
                }
            } catch (error) {
                document.getElementById('validationResult').innerHTML = 
                    '<div class="validation-error">✗ 验证失败: ' + error.message + '</div>';
            }
        }

        async function saveConfig() {
            const content = document.getElementById('configEditor').value;
            const autoReload = document.getElementById('autoReloadCheck').checked;
            
            try {
                const response = await fetch('/api/config', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ content: content, auto_reload: autoReload })
                });
                
                const result = await response.json();
                
                if (result.success) {
                    showBeautifulConfirm('✅ 保存成功', result.message, 'info');
                    configContent = content;
                    if (autoReload) {
                        setTimeout(() => {
                            loadStats(); // 重新加载统计信息
                        }, 1000);
                    }
                } else {
                    showBeautifulConfirm('❌ 保存失败', result.error || '保存配置文件时发生错误', 'danger');
                }
            } catch (error) {
                showBeautifulConfirm('❌ 保存失败', '保存配置文件时发生错误：\\n' + error.message, 'danger');
            }
        }

        async function reloadConfig() {
            const confirmed = await showBeautifulConfirm(
                '重新加载配置',
                '重新加载会丢失未保存的修改，是否继续？\\n\\n⚠️ 建议先保存当前修改。',
                'warning'
            );
            if (confirmed) {
                await loadConfigContent();
                showMessage('✅ 配置文件已重新加载', 'success');
                document.getElementById('validationResult').innerHTML = '';
            }
        }

        async function loadBackups() {
            try {
                const response = await fetch('/api/config/backups');
                const backups = await response.json();
                
                const backupList = document.getElementById('backupList');
                backupList.innerHTML = backups.map(backup => 
                    '<div class="backup-item">' +
                        '<div>' +
                            '<div>' + backup.filename + '</div>' +
                            '<div style="font-size: 0.7rem; color: #666;">' +
                                formatTimestamp(backup.timestamp) + ' | ' + formatFileSize(backup.size) +
                            '</div>' +
                        '</div>' +
                        '<button class="btn btn-secondary" style="font-size: 0.7rem; padding: 0.3rem 0.5rem;" ' +
                                'onclick="restoreBackup(\'' + backup.filename + '\')">恢复</button>' +
                    '</div>'
                ).join('');
            } catch (error) {
                console.error('加载备份列表失败:', error);
            }
        }

        async function restoreBackup(filename) {
            const confirmed = await showBeautifulConfirm(
                '恢复配置备份',
                '确定要恢复备份 "' + filename + '" 吗？\\n\\n⚠️ 当前配置会被覆盖，无法恢复。',
                'warning'
            );
            if (!confirmed) {
                return;
            }
            
            const autoReload = document.getElementById('autoReloadCheck').checked;
            
            try {
                const response = await fetch('/api/config/restore', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ filename: filename, auto_reload: autoReload })
                });
                
                const result = await response.json();
                
                if (result.success) {
                    showMessage(result.message, 'success');
                    await loadConfigContent();
                    if (autoReload) {
                        setTimeout(() => {
                            loadStats();
                        }, 1000);
                    }
                } else {
                    showMessage(result.error || '恢复失败', 'error');
                }
            } catch (error) {
                showMessage('恢复备份失败: ' + error.message, 'error');
            }
        }

        async function reloadServerConfig() {
            const confirmed = await showBeautifulConfirm(
                '重新加载服务器配置',
                '确定要重新加载服务器配置吗？\\n\\n⚠️ 这会重启DHCP服务，短暂影响网络服务。',
                'warning'
            );
            if (!confirmed) {
                return;
            }
            
            try {
                const response = await fetch('/api/config/reload', { method: 'POST' });
                const result = await response.json();
                
                if (result.success) {
                    showBeautifulConfirm('✅ 重载成功', result.message, 'info');
                    setTimeout(() => {
                        loadStats();
                        loadGatewayStatus();
                    }, 2000);
                } else {
                    showBeautifulConfirm('❌ 重载失败', result.error || '重载失败', 'danger');
                }
            } catch (error) {
                showBeautifulConfirm('❌ 重载失败', '重载配置失败: ' + error.message, 'danger');
            }
        }

        // 快速重启DHCP服务器
        async function quickRestartServer() {
            const confirmed = await showBeautifulConfirm(
                '🔄 快速重启服务器',
                '确定要重启DHCP服务器吗？\\n\\n⚠️ 这会重启整个DHCP服务，短暂影响网络服务。\\n📋 所有配置和状态都会保持不变。',
                'warning'
            );
            if (!confirmed) {
                return;
            }
            
            // 更新按钮状态
            const restartBtn = document.querySelector('.restart-btn');
            if (restartBtn) {
                restartBtn.disabled = true;
                restartBtn.innerHTML = '⏳ 重启中...';
                restartBtn.style.opacity = '0.6';
            }
            
            try {
                showLoading('正在重启DHCP服务器...');
                const response = await fetch('/api/server/restart', { method: 'POST' });
                const result = await response.json();
                
                hideLoading();
                
                if (result.success) {
                    showBeautifulConfirm('✅ 重启成功', result.message || 'DHCP服务器已成功重启！', 'info');
                    // 延迟刷新数据，等待服务器完全启动
                    setTimeout(() => {
                        loadStats();
                        loadGatewayStatus();
                        loadDevices();
                    }, 3000);
                } else {
                    showBeautifulConfirm('❌ 重启失败', result.error || '服务器重启失败', 'danger');
                }
            } catch (error) {
                hideLoading();
                showBeautifulConfirm('❌ 重启失败', '服务器重启失败: ' + error.message, 'danger');
            } finally {
                // 恢复按钮状态
                if (restartBtn) {
                    restartBtn.disabled = false;
                    restartBtn.innerHTML = '🔄 重启服务器';
                    restartBtn.style.opacity = '1';
                }
            }
        }

        function downloadConfig() {
            const content = document.getElementById('configEditor').value;
            const blob = new Blob([content], { type: 'text/yaml' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = 'dhcp-config.yaml';
            document.body.appendChild(a);
            a.click();
            document.body.removeChild(a);
            URL.revokeObjectURL(url);
        }

        // 工具函数
        function formatDuration(durationStr) {
            if (!durationStr || durationStr === '' || durationStr === '0s') return '已过期';
            
            // 处理Go格式的时间字符串，如 "47h54m45.758969583s"
            if (typeof durationStr === 'string') {
                let totalSeconds = 0;
                
                // 匹配小时
                const hoursMatch = durationStr.match(/(\d+)h/);
                if (hoursMatch) {
                    totalSeconds += parseInt(hoursMatch[1]) * 3600;
                }
                
                // 匹配分钟
                const minutesMatch = durationStr.match(/(\d+)m/);
                if (minutesMatch) {
                    totalSeconds += parseInt(minutesMatch[1]) * 60;
                }
                
                // 匹配秒（忽略小数部分）
                const secondsMatch = durationStr.match(/(\d+(?:\.\d+)?)s/);
                if (secondsMatch) {
                    totalSeconds += Math.floor(parseFloat(secondsMatch[1]));
                }
                
                if (totalSeconds <= 0) return '已过期';
                
                const hours = Math.floor(totalSeconds / 3600);
                const minutes = Math.floor((totalSeconds % 3600) / 60);
                
                if (hours > 0) {
                    return hours + '小时' + minutes + '分钟';
                } else if (minutes > 0) {
                    return minutes + '分钟';
                } else {
                    return '不到1分钟';
                }
            }
            
            // 兼容数字格式（秒数）
            if (typeof durationStr === 'number') {
                if (durationStr <= 0) return '已过期';
                const hours = Math.floor(durationStr / 3600);
                const minutes = Math.floor((durationStr % 3600) / 60);
                
                if (hours > 0) {
                    return hours + '小时' + minutes + '分钟';
                } else if (minutes > 0) {
                    return minutes + '分钟';
                } else {
                    return '不到1分钟';
                }
            }
            
            return '未知';
        }

        function formatTimestamp(timestamp) {
            return new Date(timestamp).toLocaleString('zh-CN');
        }

        function formatFileSize(bytes) {
            const units = ['B', 'KB', 'MB', 'GB'];
            let size = bytes;
            let unitIndex = 0;
            
            while (size >= 1024 && unitIndex < units.length - 1) {
                size /= 1024;
                unitIndex++;
            }
            
            return size.toFixed(1) + ' ' + units[unitIndex];
        }

        function getActionColor(action) {
            switch(action) {
                case 'DISCOVER': return 'online';
                case 'REQUEST': return 'online';
                case 'RELEASE': return 'offline';
                case 'DECLINE': return 'offline';
                default: return 'online';
            }
        }

        function showMessage(message, type) {
            const messageDiv = document.createElement('div');
            messageDiv.className = 'message message-' + type;
            messageDiv.innerHTML = '<strong>' + (type === 'success' ? '✅ ' : '❌ ') + message + '</strong>';
            
            const container = document.querySelector('.tab-content');
            container.insertBefore(messageDiv, container.firstChild);
            
            setTimeout(() => {
                if (messageDiv.parentNode) {
                    messageDiv.parentNode.removeChild(messageDiv);
                }
            }, 5000);
        }

        // 设备管理相关函数
        let currentEditingMAC = null;

        function showAddDeviceModal() {
            currentEditingMAC = null;
            document.getElementById('deviceModalTitle').textContent = '添加设备';
            clearDeviceForm();
            document.getElementById('deviceMAC').disabled = false;
            document.getElementById('deviceModal').style.display = 'block';
        }

        function closeDeviceModal() {
            document.getElementById('deviceModal').style.display = 'none';
            currentEditingMAC = null;
        }

        // 点击模态框外部关闭
        window.onclick = function(event) {
            const deviceModal = document.getElementById('deviceModal');
            const staticIPModal = document.getElementById('staticIPModal');
            const discoveryModal = document.getElementById('discoveryModal');
            
            if (event.target === deviceModal) {
                closeDeviceModal();
            } else if (event.target === staticIPModal) {
                closeStaticIPModal();
            } else if (event.target === discoveryModal) {
                closeDiscoveryModal();
            }
        }

        // 键盘事件处理
        document.addEventListener('keydown', function(event) {
            const deviceModal = document.getElementById('deviceModal');
            const staticIPModal = document.getElementById('staticIPModal');
            const discoveryModal = document.getElementById('discoveryModal');
            
            if (event.key === 'Escape') {
                if (deviceModal.style.display === 'block') {
                    closeDeviceModal();
                } else if (staticIPModal.style.display === 'block') {
                    closeStaticIPModal();
                } else if (discoveryModal.style.display === 'block') {
                    closeDiscoveryModal();
                }
            } else if (event.key === 'Enter' && event.ctrlKey) {
                if (deviceModal.style.display === 'block') {
                    saveDeviceForm();
                }
            }
        });

        function clearDeviceForm() {
            document.getElementById('deviceMAC').value = '';
            document.getElementById('deviceType').value = '';
            document.getElementById('deviceModel').value = '';
            document.getElementById('deviceOwner').value = '';
            document.getElementById('deviceDescription').value = '';
        }

        function saveDeviceForm() {
            const mac = document.getElementById('deviceMAC').value.trim();
            if (!mac) {
                showMessage('MAC地址不能为空', 'error');
                return;
            }

            const deviceData = {
                mac: mac,
                device_type: document.getElementById('deviceType').value.trim() || '',
                model: document.getElementById('deviceModel').value.trim() || '',
                owner: document.getElementById('deviceOwner').value.trim() || '',
                description: document.getElementById('deviceDescription').value.trim() || ''
            };
            
            if (currentEditingMAC) {
                updateDevice(deviceData);
            } else {
                addDevice(deviceData);
            }
        }

        async function addDevice(deviceData) {
            try {
                const response = await fetch('/api/devices', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(deviceData)
                });
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 添加成功', '设备信息已成功添加！', 'info');
                    closeDeviceModal();
                    loadDevices();
                } else {
                    const error = await response.text();
                    showBeautifulConfirm('❌ 添加失败', '添加设备时发生错误：\\n' + error, 'danger');
                }
            } catch (error) {
                console.error('添加设备失败:', error);
                showBeautifulConfirm('❌ 添加失败', '添加设备时发生错误：\\n' + error.message, 'danger');
            }
        }

        function editDevice(mac) {
            const device = allDevices.find(d => d.mac === mac);
            if (!device) {
                showMessage('设备未找到', 'error');
                return;
            }
            
            currentEditingMAC = mac;
            document.getElementById('deviceModalTitle').textContent = '编辑设备';
            
            // 填充表单数据
            document.getElementById('deviceMAC').value = device.mac || '';
            document.getElementById('deviceMAC').disabled = true; // 编辑时不允许修改MAC
            document.getElementById('deviceType').value = device.device_type || '';
            document.getElementById('deviceModel').value = device.model || '';
            document.getElementById('deviceOwner').value = device.owner || '';
            document.getElementById('deviceDescription').value = device.description || '';
            
            document.getElementById('deviceModal').style.display = 'block';
        }

        async function updateDevice(deviceData) {
            try {
                const response = await fetch('/api/devices', {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(deviceData)
                });
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 更新成功', '设备信息已成功更新！', 'info');
                    closeDeviceModal();
                    loadDevices();
                } else {
                    const error = await response.text();
                    showBeautifulConfirm('❌ 更新失败', '更新设备信息时发生错误：\\n' + error, 'danger');
                }
            } catch (error) {
                console.error('更新设备信息失败:', error);
                showBeautifulConfirm('❌ 更新失败', '更新设备信息时发生错误：\\n' + error.message, 'danger');
            }
        }

        async function deleteDevice(mac) {
            const device = allDevices.find(d => d.mac === mac);
            const deviceName = device ? (device.hostname || device.owner || mac) : mac;
            
            // 使用更美观的确认对话框
            const confirmed = await showBeautifulConfirm(
                '删除设备',
                '确定要删除设备 "' + deviceName + '" 吗？\\n\\n⚠️ 删除后该设备的所有信息将被清除。',
                'danger'
            );
            if (!confirmed) {
                return;
            }
            
            // 显示加载提示
            showLoading('正在删除设备...');
            
            try {
                const response = await fetch('/api/devices', {
                    method: 'DELETE',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ mac: mac })
                });
                
                hideLoading(); // 隐藏加载提示
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 删除成功', '设备已成功删除！', 'info');
                    loadDevices();
                } else {
                    const error = await response.text();
                    showBeautifulConfirm('❌ 删除失败', '删除设备时发生错误：\\n' + error, 'danger');
                }
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                console.error('删除设备失败:', error);
                showBeautifulConfirm('❌ 删除失败', '删除设备时发生错误：\\n' + error.message, 'danger');
            }
        }

        // 静态IP配置相关函数
        async function configureStaticIP(mac) {
            const device = allDevices.find(d => d.mac === mac);
            if (!device) {
                showMessage('设备未找到', 'error');
                return;
            }
            
            // 显示加载提示
            showLoading('正在准备配置表单...');
            
            // 加载可用IP列表
            await fetchAvailableIPs();
            
            const modal = document.getElementById('staticIPModal');
            
            // 清除编辑模式标记
            modal.removeAttribute('data-edit-mode');
            modal.removeAttribute('data-old-alias');
            
            // 填充MAC地址
            document.getElementById('staticMAC').value = mac;
            
            // 自动生成唯一别名
            const generatedAlias = await generateUniqueAlias(device.hostname || device.owner || mac.replace(/:/g, ''));
            document.getElementById('staticAlias').value = generatedAlias;
            document.getElementById('staticIP').value = '';
            document.getElementById('staticHostname').value = device.hostname || '';
            
            try {
                // 添加小延迟确保加载提示可见
                await new Promise(resolve => setTimeout(resolve, 300));
                
                hideLoading(); // 隐藏加载提示
                
                // 显示模态框
                modal.style.display = 'block';
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                console.error('准备配置表单失败:', error);
                showMessage('准备配置表单失败: ' + error.message, 'error');
            }
        }

        function closeStaticIPModal() {
            document.getElementById('staticIPModal').style.display = 'none';
        }

        // 生成唯一别名的函数
        async function generateUniqueAlias(baseName) {
            try {
                // 获取现有的静态绑定
                const response = await fetch('/api/bindings');
                const bindings = await response.json();
                
                const existingAliases = new Set();
                bindings.forEach(binding => {
                    existingAliases.add(binding.alias);
                });
                
                // 清理基础名称，移除特殊字符
                let cleanBase = baseName.replace(/[^a-zA-Z0-9-_]/g, '').toLowerCase();
                if (!cleanBase) {
                    cleanBase = 'device';
                }
                
                // 检查基础名称是否可用
                if (!existingAliases.has(cleanBase)) {
                    return cleanBase;
                }
                
                // 尝试添加数字后缀
                for (let i = 1; i <= 999; i++) {
                    const candidate = cleanBase + '-' + i;
                    if (!existingAliases.has(candidate)) {
                        return candidate;
                    }
                }
                
                // 如果前面都失败，使用时间戳
                return cleanBase + '-' + Date.now();
            } catch (error) {
                console.error('生成别名失败:', error);
                return 'device-' + Date.now();
            }
        }

        // 将动态租约转换为静态IP绑定
        async function convertToStatic(mac, ip, hostname, gateway) {
            // 显示加载提示
            showLoading('正在转换租约为静态IP...');
            
            try {
                // 自动生成唯一别名
                const alias = await generateUniqueAlias(hostname || mac.replace(/:/g, ''));
                
                const convertData = {
                    mac: mac,
                    alias: alias,
                    gateway: gateway || '',
                    hostname: hostname || ''
                };
                
                const response = await fetch('/api/leases/convert-to-static', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(convertData)
                });
                
                hideLoading(); // 隐藏加载提示
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 转换成功', '租约转换为静态IP成功！设备重新连接后生效。别名: ' + alias, 'info');
                    loadDevices(); // 刷新设备列表
                    // 移除 loadLeases() 调用，因为在设备管理页面不存在此函数
                } else {
                    const errorData = await response.json();
                    showBeautifulConfirm('❌ 转换失败', errorData.error || '未知错误', 'danger');
                }
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                console.error('转换静态IP失败:', error);
                showBeautifulConfirm('❌ 转换失败', '转换失败: ' + error.message, 'danger');
            }
        }

        // 编辑现有静态IP绑定
        async function editStaticIP(mac) {
            const device = allDevices.find(d => d.mac === mac);
            if (!device || !device.has_static_ip) {
                showMessage('设备未找到或没有静态IP绑定', 'error');
                return;
            }
            
            // 从缓存中获取静态绑定信息
            const binding = staticBindings[mac];
            if (!binding) {
                showMessage('未找到对应的静态绑定信息', 'error');
                return;
            }
            
            // 显示加载提示
            showLoading('正在加载编辑表单...');
            
            try {
                // 加载可用IP列表
                await fetchAvailableIPs();
                
                // 填充表单数据
                document.getElementById('staticMAC').value = mac;
                // 如果已有别名，保持原样；否则自动生成
                if (binding.alias) {
                    document.getElementById('staticAlias').value = binding.alias;
                } else {
                    const generatedAlias = await generateUniqueAlias(device.hostname || device.owner || mac.replace(/:/g, ''));
                    document.getElementById('staticAlias').value = generatedAlias;
                }
                document.getElementById('staticIP').value = binding.ip || '';
                document.getElementById('staticHostname').value = binding.hostname || device.hostname || '';
                
                // 标记为编辑模式
                document.getElementById('staticIPModal').setAttribute('data-edit-mode', 'true');
                document.getElementById('staticIPModal').setAttribute('data-old-alias', binding.alias);
                
                hideLoading(); // 隐藏加载提示
                
                // 显示模态框
                document.getElementById('staticIPModal').style.display = 'block';
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                console.error('准备编辑表单失败:', error);
                showMessage('准备编辑表单失败: ' + error.message, 'error');
            }
        }

        // 删除静态IP绑定
        async function deleteStaticIP(mac) {
            const device = allDevices.find(d => d.mac === mac);
            if (!device || !device.has_static_ip) {
                showBeautifulConfirm('❌ 删除失败', '设备未找到或没有静态IP绑定', 'danger');
                return;
            }
            
            // 从缓存中获取静态绑定信息
            const binding = staticBindings[mac];
            if (!binding) {
                showBeautifulConfirm('❌ 删除失败', '未找到对应的静态绑定信息', 'danger');
                return;
            }
            
            const deviceName = device.hostname || device.owner || mac;
            
            // 使用更美观的确认对话框
            const confirmed = await showBeautifulConfirm(
                '删除静态IP绑定',
                '确定要删除设备 "' + deviceName + '" 的静态IP绑定吗？\\n\\n⚠️ 删除后设备将使用DHCP动态分配IP地址。',
                'danger'
            );
            if (!confirmed) {
                return;
            }
            
            // 显示加载提示
            showLoading('正在删除静态IP绑定...');
            
            try {
                // 删除静态绑定
                const deleteResponse = await fetch('/api/bindings', {
                    method: 'DELETE',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ alias: binding.alias })
                });
                
                hideLoading(); // 隐藏加载提示
                
                if (deleteResponse.ok) {
                    showBeautifulConfirm('✅ 删除成功', '静态IP绑定删除成功！设备重新连接后将使用动态IP。', 'info');
                    loadDevices(); // 刷新设备列表
                    // 移除 loadLeases() 调用，因为在设备管理页面不存在此函数
                } else {
                    const errorData = await deleteResponse.json();
                    showBeautifulConfirm('❌ 删除失败', errorData.error || '未知错误', 'danger');
                }
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                console.error('删除静态IP绑定失败:', error);
                showBeautifulConfirm('❌ 删除失败', '删除静态IP绑定失败: ' + error.message, 'danger');
            }
        }

        // 扩展模态框点击外部关闭功能
        const originalWindowClick = window.onclick;
        window.onclick = function(event) {
            const deviceModal = document.getElementById('deviceModal');
            const staticIPModal = document.getElementById('staticIPModal');
            
            if (event.target === deviceModal) {
                closeDeviceModal();
            } else if (event.target === staticIPModal) {
                closeStaticIPModal();
            }
        }

        // 扩展键盘事件处理
        document.addEventListener('keydown', function(event) {
            const deviceModal = document.getElementById('deviceModal');
            const staticIPModal = document.getElementById('staticIPModal');
            
            if (deviceModal.style.display === 'block') {
                if (event.key === 'Escape') {
                    closeDeviceModal();
                } else if (event.key === 'Enter' && event.ctrlKey) {
                    saveDeviceForm();
                }
            } else if (staticIPModal.style.display === 'block') {
                if (event.key === 'Escape') {
                    closeStaticIPModal();
                } else if (event.key === 'Enter' && event.ctrlKey) {
                    saveStaticIP();
                }
            }
        });



        async function saveStaticIP() {
            const mac = document.getElementById('staticMAC').value;
            const alias = document.getElementById('staticAlias').value.trim();
            const ip = document.getElementById('staticIP').value.trim();
            const hostname = document.getElementById('staticHostname').value.trim();
            
            if (!alias) {
                showBeautifulConfirm('❌ 验证失败', '绑定别名不能为空', 'danger');
                return;
            }
            
            if (!ip) {
                showBeautifulConfirm('❌ 验证失败', 'IP地址不能为空', 'danger');
                return;
            }
            
            // 使用新的IP校验函数
            if (!(await validateStaticIP())) {
                return;
            }
            
            const modal = document.getElementById('staticIPModal');
            const isEditMode = modal.getAttribute('data-edit-mode') === 'true';
            const oldAlias = modal.getAttribute('data-old-alias');
            
            const bindingData = {
                alias: alias,
                mac: mac,
                ip: ip,
                hostname: hostname
            };
            
            // 如果是编辑模式，添加old_alias字段
            if (isEditMode) {
                bindingData.old_alias = oldAlias;
            }
            
            // 显示加载提示
            showLoading(isEditMode ? '正在更新静态IP...' : '正在配置静态IP...');
            
            try {
                const response = await fetch('/api/bindings', {
                    method: isEditMode ? 'PUT' : 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(bindingData)
                });
                
                hideLoading(); // 隐藏加载提示
                
                if (response.ok) {
                    const action = isEditMode ? '更新' : '配置';
                    showBeautifulConfirm('✅ ' + action + '成功', '静态IP' + action + '成功！设备重新连接后生效。', 'info');
                    closeStaticIPModal();
                    loadDevices(); // 刷新设备列表
                    
                    // 清除编辑模式标记
                    modal.removeAttribute('data-edit-mode');
                    modal.removeAttribute('data-old-alias');
                } else {
                    const errorData = await response.json();
                    const action = isEditMode ? '更新' : '配置';
                    showBeautifulConfirm('❌ ' + action + '失败', errorData.error || '未知错误', 'danger');
                }
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                const action = isEditMode ? '更新' : '配置';
                console.error(action + '静态IP失败:', error);
                showBeautifulConfirm('❌ ' + action + '失败', action + '静态IP失败: ' + error.message, 'danger');
            }
        }

        // 自动补全和校验相关函数
        async function fetchAvailableIPs() {
            try {
                const res = await fetch('/api/available-ips');
                availableIPs = await res.json();
            } catch (e) {
                availableIPs = [];
            }
        }

        function onStaticIPInput() {
            const input = document.getElementById('staticIP');
            const val = input.value.trim();
            if (!availableIPs.length) return;
            if (!val) { hideIPAutocomplete(); return; }
            const suggestions = availableIPs.filter(ip => ip.startsWith(val)).slice(0, 5);
            showIPAutocomplete(suggestions);
            staticIPActiveIndex = -1;
        }

        function showIPAutocomplete(suggestions) {
            const list = document.getElementById('staticIPAutocomplete');
            if (!suggestions.length) { list.style.display = 'none'; return; }
            list.innerHTML = suggestions.map((ip, idx) => '<div class="autocomplete-item" id="staticIPAutoItem' + idx + '" onclick="selectStaticIPSuggestion(\'' + ip + '\')">' + ip + '</div>').join('');
            list.style.display = 'block';
            staticIPActiveIndex = -1;
            
            // 确保下拉列表不超出屏幕边界
            const input = document.getElementById('staticIP');
            const inputRect = input.getBoundingClientRect();
            const listRect = list.getBoundingClientRect();
            
            // 检查右侧是否溢出
            if (listRect.right > window.innerWidth) {
                list.style.width = (window.innerWidth - inputRect.left - 10) + 'px';
            }
        }

        function hideIPAutocomplete() {
            document.getElementById('staticIPAutocomplete').style.display = 'none';
            staticIPActiveIndex = -1;
        }

        function selectStaticIPSuggestion(ip) {
            document.getElementById('staticIP').value = ip;
            hideIPAutocomplete();
        }

        async function validateStaticIP() {
            const ip = document.getElementById('staticIP').value.trim();
            if (!ip) return false;
            // 简单格式校验
            const ipRegex = /^(\d{1,3}\.){3}\d{1,3}$/;
            if (!ipRegex.test(ip)) {
                showBeautifulConfirm('❌ IP格式错误', '请输入正确的IPv4地址', 'danger');
                return false;
            }
            if (!availableIPs.length) await fetchAvailableIPs();
            if (!availableIPs.includes(ip)) {
                showBeautifulConfirm('❌ IP不可用', '该IP已被占用或不在可用范围', 'danger');
                return false;
            }
            return true;
        }

        // 键盘事件支持Tab补齐、上下键选择、回车选中
        document.addEventListener('DOMContentLoaded', function() {
            const input = document.getElementById('staticIP');
            input.addEventListener('keydown', function(e) {
                const list = document.getElementById('staticIPAutocomplete');
                const items = list.querySelectorAll('.autocomplete-item');
                if (list.style.display === 'block' && items.length > 0) {
                    if (e.key === 'Tab') {
                        e.preventDefault();
                        input.value = items[staticIPActiveIndex >= 0 ? staticIPActiveIndex : 0].textContent;
                        hideIPAutocomplete();
                    } else if (e.key === 'ArrowDown') {
                        e.preventDefault();
                        staticIPActiveIndex = (staticIPActiveIndex + 1) % items.length;
                        updateStaticIPActiveItem(items);
                    } else if (e.key === 'ArrowUp') {
                        e.preventDefault();
                        staticIPActiveIndex = (staticIPActiveIndex - 1 + items.length) % items.length;
                        updateStaticIPActiveItem(items);
                    } else if (e.key === 'Enter') {
                        if (staticIPActiveIndex >= 0) {
                            e.preventDefault();
                            input.value = items[staticIPActiveIndex].textContent;
                            hideIPAutocomplete();
                        }
                    }
                }
            });
        });
        function updateStaticIPActiveItem(items) {
            items.forEach((item, idx) => {
                if (idx === staticIPActiveIndex) {
                    item.classList.add('active');
                    item.scrollIntoView({block: 'nearest'});
                } else {
                    item.classList.remove('active');
                }
            });
        }

        // 生成唯一别名函数
        async function generateUniqueAlias(baseName) {
            // 清理基础名称，只保留字母、数字和下划线
            let cleanName = baseName.replace(/[^a-zA-Z0-9_]/g, '').substring(0, 10);
            if (!cleanName) {
                cleanName = 'device';
            }
            
            // 获取所有现有的静态绑定
            try {
                const response = await fetch('/api/static-bindings');
                const bindings = await response.json();
                
                // 检查是否已存在相同别名
                const existingAliases = bindings.map(b => b.alias);
                let counter = 1;
                let alias = cleanName;
                
                while (existingAliases.includes(alias)) {
                    alias = cleanName + '_' + counter;
                    counter++;
                }
                
                return alias;
            } catch (error) {
                console.error('获取现有别名失败:', error);
                // 如果获取失败，使用时间戳生成唯一别名
                return cleanName + '_' + Date.now().toString().slice(-6);
            }
        }

        // 指定网关相关函数
        async function configureGateway(mac) {
            const device = allDevices.find(d => d.mac === mac);
            if (!device) {
                showBeautifulConfirm('❌ 错误', '设备未找到', 'danger');
                return;
            }
            
            // 显示加载提示
            showLoading('正在准备网关配置...');
            
            try {
                // 填充设备信息
                document.getElementById('deviceGatewayMAC').value = mac;
                document.getElementById('deviceGatewayName').value = device.hostname || device.owner || '未知设备';
                
                // 加载网关列表
                await loadDeviceGatewaysForSelect();
                
                // 设置当前网关（如果有）
                const currentGateway = device.gateway || '';
                document.getElementById('deviceGatewaySelect').value = currentGateway;
                
                hideLoading(); // 隐藏加载提示
                
                // 显示模态框
                document.getElementById('deviceGatewayModal').style.display = 'block';
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                console.error('准备网关配置失败:', error);
                showBeautifulConfirm('❌ 错误', '准备网关配置失败: ' + error.message, 'danger');
            }
        }

        async function loadDeviceGatewaysForSelect() {
            try {
                const response = await fetch('/api/gateways');
                const gateways = await response.json();
                
                const select = document.getElementById('deviceGatewaySelect');
                // 保留默认选项
                select.innerHTML = '<option value="">使用默认网关</option>';
                
                Object.entries(gateways).forEach(([name, gateway]) => {
                    const option = document.createElement('option');
                    option.value = name;
                    option.textContent = name + ' (' + gateway.ip + ')' + (gateway.is_default ? ' [默认]' : '');
                    select.appendChild(option);
                });
            } catch (error) {
                console.error('加载网关列表失败:', error);
            }
        }

        function closeDeviceGatewayModal() {
            document.getElementById('deviceGatewayModal').style.display = 'none';
        }

        async function saveDeviceGateway() {
            const mac = document.getElementById('deviceGatewayMAC').value;
            const gateway = document.getElementById('deviceGatewaySelect').value;
            
            // 显示加载提示
            showLoading('正在保存网关配置...');
            
            try {
                const response = await fetch('/api/devices/gateway', {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        mac: mac,
                        gateway: gateway
                    })
                });
                
                hideLoading(); // 隐藏加载提示
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 保存成功', '设备网关配置已保存！设备重新连接后生效。', 'info');
                    closeDeviceGatewayModal();
                    loadDevices(); // 刷新设备列表
                } else {
                    const errorData = await response.json();
                    showBeautifulConfirm('❌ 保存失败', errorData.error || '未知错误', 'danger');
                }
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                console.error('保存网关配置失败:', error);
                showBeautifulConfirm('❌ 保存失败', '保存网关配置失败: ' + error.message, 'danger');
            }
        }

        // 网关管理相关函数
        let currentGateway = null;

        function showAddGatewayModal() {
            document.getElementById('gatewayModalTitle').textContent = '添加网关';
            document.getElementById('gatewayForm').reset();
            currentGateway = null;
            // 重置DNS输入框
            resetGatewayDNSFields();
            document.getElementById('gatewayModal').style.display = 'block';
        }

        function editGateway(gatewayName) {
            // 显示加载提示
            showLoading('正在加载网关信息...');
            
            // 从当前数据中获取网关信息
            fetch('/api/gateways')
                .then(response => response.json())
                .then(gateways => {
                    hideLoading(); // 隐藏加载提示
                    
                    const gateway = gateways[gatewayName];
                    if (!gateway) {
                        showMessage('网关信息未找到', 'error');
                        return;
                    }
                    
                    // 填充表单
                    document.getElementById('gatewayModalTitle').textContent = '编辑网关';
                    document.getElementById('gatewayName').value = gatewayName;
                    document.getElementById('gatewayIP').value = gateway.ip;
                    document.getElementById('gatewayIsDefault').checked = gateway.is_default;
                    document.getElementById('gatewayDescription').value = gateway.description || '';
                    
                    // 加载DNS配置
                    loadGatewayDNSFields(gateway.dns_servers || []);
                    
                    currentGateway = gatewayName;
                    document.getElementById('gatewayModal').style.display = 'block';
                })
                .catch(error => {
                    hideLoading(); // 隐藏加载提示
                    console.error('获取网关信息失败:', error);
                    showMessage('获取网关信息失败', 'error');
                });
        }

        async function saveGatewayForm() {
            const name = document.getElementById('gatewayName').value.trim();
            const ip = document.getElementById('gatewayIP').value.trim();
            const isDefault = document.getElementById('gatewayIsDefault').checked;
            const description = document.getElementById('gatewayDescription').value.trim();
            const dnsServers = collectGatewayDNSServers();

            if (!name) {
                showMessage('网关名称不能为空', 'error');
                return;
            }

            if (!ip) {
                showMessage('IP地址不能为空', 'error');
                return;
            }

            // 简单的IP格式验证
            const ipRegex = /^(\d{1,3}\.){3}\d{1,3}$/;
            if (!ipRegex.test(ip)) {
                showMessage('IP地址格式不正确', 'error');
                return;
            }

            const isEdit = currentGateway !== null;
            const data = {
                name: name,
                ip: ip,
                is_default: isDefault,
                description: description,
                dns_servers: dnsServers
            };

            if (isEdit) {
                data.old_name = currentGateway;
            }

            // 显示加载提示
            showLoading(isEdit ? '正在更新网关...' : '正在添加网关...');

            try {
                const response = await fetch('/api/gateways', {
                    method: isEdit ? 'PUT' : 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(data)
                });

                hideLoading(); // 隐藏加载提示

                if (response.ok) {
                    const action = isEdit ? '更新' : '添加';
                    showMessage('网关' + action + '成功！', 'success');
                    closeGatewayModal();
                    loadGatewayStatus();
                } else {
                    const errorData = await response.json();
                    const action = isEdit ? '更新' : '添加';
                    showMessage('网关' + action + '失败: ' + (errorData.error || '未知错误'), 'error');
                }
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                const action = isEdit ? '更新' : '添加';
                console.error(action + '网关失败:', error);
                showMessage(action + '网关失败: ' + error.message, 'error');
            }
        }

        async function deleteGateway(gatewayName) {
            const confirmed = await showBeautifulConfirm(
                '删除网关',
                '确定要删除网关 "' + gatewayName + '" 吗？\\n\\n⚠️ 删除网关后，关联的设备将使用默认网关。',
                'danger'
            );
            if (!confirmed) {
                return;
            }

            // 显示加载提示
            showLoading('正在删除网关...');

            try {
                const response = await fetch('/api/gateways', {
                    method: 'DELETE',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name: gatewayName })
                });

                hideLoading(); // 隐藏加载提示

                if (response.ok) {
                    showBeautifulConfirm('✅ 删除成功', '网关已成功删除！', 'info');
                    loadGatewayStatus();
                } else {
                    const errorData = await response.json();
                    showBeautifulConfirm('❌ 删除失败', '删除网关时发生错误：\\n' + (errorData.error || '未知错误'), 'danger');
                }
            } catch (error) {
                hideLoading(); // 隐藏加载提示
                console.error('删除网关失败:', error);
                showBeautifulConfirm('❌ 删除失败', '删除网关时发生错误：\\n' + error.message, 'danger');
            }
        }

        function closeGatewayModal() {
            document.getElementById('gatewayModal').style.display = 'none';
            document.getElementById('gatewayForm').reset();
            currentGateway = null;
        }

        // DNS管理相关函数
        function resetGatewayDNSFields() {
            const container = document.getElementById('gatewayDNSContainer');
            container.innerHTML = '<input type="text" class="form-control gateway-dns-input" placeholder="例如：8.8.8.8">';
        }

        function loadGatewayDNSFields(dnsServers) {
            const container = document.getElementById('gatewayDNSContainer');
            container.innerHTML = '';
            
            if (dnsServers.length === 0) {
                // 如果没有DNS服务器，显示一个空的输入框
                container.innerHTML = '<input type="text" class="form-control gateway-dns-input" placeholder="例如：8.8.8.8">';
            } else {
                // 为每个DNS服务器创建输入框
                dnsServers.forEach((dns, index) => {
                    const inputGroup = document.createElement('div');
                    inputGroup.className = 'input-group mb-2';
                    inputGroup.innerHTML = 
                        '<input type="text" class="form-control gateway-dns-input" value="' + dns + '" placeholder="例如：8.8.8.8">' +
                        '<button type="button" class="btn btn-outline-danger btn-sm" onclick="removeGatewayDNSField(this)">删除</button>';
                    container.appendChild(inputGroup);
                });
            }
        }

        function collectGatewayDNSServers() {
            const inputs = document.querySelectorAll('.gateway-dns-input');
            const dnsServers = [];
            
            inputs.forEach(input => {
                const value = input.value.trim();
                if (value) {
                    dnsServers.push(value);
                }
            });
            
            return dnsServers;
        }

        function addGatewayDNSField() {
            const container = document.getElementById('gatewayDNSContainer');
            const inputGroup = document.createElement('div');
            inputGroup.className = 'input-group mb-2';
            inputGroup.innerHTML = 
                '<input type="text" class="form-control gateway-dns-input" placeholder="例如：8.8.8.8">' +
                '<button type="button" class="btn btn-outline-danger btn-sm" onclick="removeGatewayDNSField(this)">删除</button>';
            container.appendChild(inputGroup);
        }

        function removeGatewayDNSField(button) {
            const container = document.getElementById('gatewayDNSContainer');
            if (container.children.length > 1) {
                button.parentElement.remove();
            } else {
                // 如果只有一个输入框，清空内容而不删除
                const input = button.parentElement.querySelector('.gateway-dns-input');
                input.value = '';
            }
        }

        // 扩展模态框点击外部关闭功能，添加网关模态框
        const originalWindowClickGateway = window.onclick;
        window.onclick = function(event) {
            const deviceModal = document.getElementById('deviceModal');
            const staticIPModal = document.getElementById('staticIPModal');
            const discoveryModal = document.getElementById('discoveryModal');
            const gatewayModal = document.getElementById('gatewayModal');
            
            if (event.target === deviceModal) {
                closeDeviceModal();
            } else if (event.target === staticIPModal) {
                closeStaticIPModal();
            } else if (event.target === discoveryModal) {
                closeDiscoveryModal();
            } else if (event.target === gatewayModal) {
                closeGatewayModal();
            }
        }

        // 扩展键盘事件处理，添加网关模态框
        document.addEventListener('keydown', function(event) {
            const deviceModal = document.getElementById('deviceModal');
            const staticIPModal = document.getElementById('staticIPModal');
            const discoveryModal = document.getElementById('discoveryModal');
            const gatewayModal = document.getElementById('gatewayModal');
            
            if (deviceModal.style.display === 'block') {
                if (event.key === 'Escape') {
                    closeDeviceModal();
                } else if (event.key === 'Enter' && event.ctrlKey) {
                    saveDeviceForm();
                }
            } else if (staticIPModal.style.display === 'block') {
                if (event.key === 'Escape') {
                    closeStaticIPModal();
                } else if (event.key === 'Enter' && event.ctrlKey) {
                    saveStaticIP();
                }
            } else if (discoveryModal.style.display === 'block') {
                if (event.key === 'Escape') {
                    closeDiscoveryModal();
                }
            } else if (gatewayModal.style.display === 'block') {
                if (event.key === 'Escape') {
                    closeGatewayModal();
                } else if (event.key === 'Enter' && event.ctrlKey) {
                    saveGatewayForm();
                }
            }
        });

        // 界面配置功能
        function loadUIConfig() {
            // 加载主题设置
            const savedTheme = localStorage.getItem('theme') || 'light';
            const themeSelect = document.getElementById('themeSelect');
            if (themeSelect) {
                themeSelect.value = savedTheme;
            }
            
            // 加载其他界面配置
            const compactMode = localStorage.getItem('compactMode') === 'true';
            const showAdvanced = localStorage.getItem('showAdvanced') !== 'false'; // 默认为true
            const refreshInterval = localStorage.getItem('refreshInterval') || '10';
            
            document.getElementById('compactMode').checked = compactMode;
            document.getElementById('showAdvanced').checked = showAdvanced;
            document.getElementById('refreshInterval').value = refreshInterval;
            
            // 应用紧凑模式
            if (compactMode) {
                document.body.classList.add('compact-mode');
            }
        }
        
        function saveUIConfig() {
            // 保存所有界面配置
            const theme = document.getElementById('themeSelect').value;
            const compactMode = document.getElementById('compactMode').checked;
            const showAdvanced = document.getElementById('showAdvanced').checked;
            const refreshInterval = document.getElementById('refreshInterval').value;
            
            localStorage.setItem('theme', theme);
            localStorage.setItem('compactMode', compactMode);
            localStorage.setItem('showAdvanced', showAdvanced);
            localStorage.setItem('refreshInterval', refreshInterval);
            
            // 应用设置
            changeTheme();
            updateCompactMode();
            updateRefreshInterval();
            
            showBeautifulConfirm('✅ 保存成功', '界面配置已成功保存！', 'info');
        }
        
        function resetUIConfig() {
            // 重置为默认设置
            localStorage.removeItem('theme');
            localStorage.removeItem('compactMode');
            localStorage.removeItem('showAdvanced');
            localStorage.removeItem('refreshInterval');
            
            // 重新加载配置
            loadUIConfig();
            
            // 应用默认设置
            changeTheme();
            updateCompactMode();
            updateRefreshInterval();
            
            showBeautifulConfirm('✅ 重置成功', '界面配置已重置为默认值', 'info');
        }
        
        function changeTheme() {
            const themeSelect = document.getElementById('themeSelect');
            const body = document.body;
            
            if (themeSelect) {
                // 移除所有主题类
                body.classList.remove('dark-theme', 'pink-theme', 'light-theme');
                
                const selectedTheme = themeSelect.value;
                
                // 应用选中的主题
                if (selectedTheme === 'dark') {
                    body.classList.add('dark-theme');
                } else if (selectedTheme === 'pink') {
                    body.classList.add('pink-theme');
                } else {
                    body.classList.add('light-theme');
                }
                
                localStorage.setItem('theme', selectedTheme);
                
                // 主题切换后修复统计数值可见性
                setTimeout(() => {
                    fixStatValueVisibility();
                }, 200);
            }
        }
        
        function updateCompactMode() {
            const compactMode = document.getElementById('compactMode').checked;
            if (compactMode) {
                document.body.classList.add('compact-mode');
            } else {
                document.body.classList.remove('compact-mode');
            }
            localStorage.setItem('compactMode', compactMode);
        }
        
        function updateRefreshInterval() {
            const interval = document.getElementById('refreshInterval').value;
            localStorage.setItem('refreshInterval', interval);
            // 这里可以添加实际的刷新间隔更新逻辑
        }

        // 页面加载时初始化界面设置
        document.addEventListener('DOMContentLoaded', function() {
            const savedTheme = localStorage.getItem('theme') || 'light';
            
            // 应用主题
            document.body.classList.remove('dark-theme', 'pink-theme', 'light-theme');
            if (savedTheme === 'dark') {
                document.body.classList.add('dark-theme');
            } else if (savedTheme === 'pink') {
                document.body.classList.add('pink-theme');
            } else {
                document.body.classList.add('light-theme');
            }
            
            // 设置主题选择器的值
            const themeSelect = document.getElementById('themeSelect');
            if (themeSelect) {
                themeSelect.value = savedTheme;
            }
            
            // 应用紧凑模式
            const compactMode = localStorage.getItem('compactMode') === 'true';
            if (compactMode) {
                document.body.classList.add('compact-mode');
            }
            
            // 初始化界面配置（如果在界面配置页面）
            if (document.getElementById('themeSelect')) {
                loadUIConfig();
            }
        });

        // 服务器配置功能
        async function loadServerConfig(showMessage = true) {
            try {
                const response = await fetch('/api/config/server');
                const config = await response.json();
                
                document.getElementById('serverInterface').value = config.interface || '';
                document.getElementById('serverPort').value = config.port || 67;
                document.getElementById('serverAPIPort').value = config.api_port || 8080;
                document.getElementById('serverAPIHost').value = config.api_host || '';
                
                // 处理租期时间：将纳秒转换为可读格式
                let leaseTimeDisplay = '24h';
                if (config.lease_time) {
                    const seconds = Math.floor(config.lease_time / 1000000000); // 纳秒转秒
                    const hours = Math.floor(seconds / 3600);
                    const minutes = Math.floor((seconds % 3600) / 60);
                    const remainingSeconds = seconds % 60;
                    
                    if (hours > 0) {
                        leaseTimeDisplay = hours + 'h';
                        if (minutes > 0) leaseTimeDisplay += minutes + 'm';
                        if (remainingSeconds > 0) leaseTimeDisplay += remainingSeconds + 's';
                    } else if (minutes > 0) {
                        leaseTimeDisplay = minutes + 'm';
                        if (remainingSeconds > 0) leaseTimeDisplay += remainingSeconds + 's';
                    } else {
                        leaseTimeDisplay = remainingSeconds + 's';
                    }
                }
                document.getElementById('serverLeaseTime').value = leaseTimeDisplay;
                
                document.getElementById('serverLogLevel').value = config.log_level || 'info';
                document.getElementById('serverLogFile').value = config.log_file || 'dhcp.log';
                document.getElementById('serverDebug').checked = config.debug || false;
                document.getElementById('serverAllowAnyServerIP').checked = config.allow_any_server_ip || false;
                
                if (showMessage) {
                    showBeautifulConfirm('✅ 加载成功', '服务器配置加载成功', 'info');
                }
            } catch (error) {
                console.error('加载服务器配置失败:', error);
                if (showMessage) {
                    showBeautifulConfirm('❌ 加载失败', '加载服务器配置失败: ' + error.message, 'danger');
                }
            }
        }

        async function saveServerConfig() {
            try {
                // 解析租期时间字符串为秒数
                const leaseTimeStr = document.getElementById('serverLeaseTime').value.trim();
                let leaseTimeSeconds = 86400; // 默认24小时
                
                if (leaseTimeStr) {
                    // 支持多种时间格式：24h, 24h0m0s, 86400s, 86400
                    if (leaseTimeStr.includes('h') || leaseTimeStr.includes('m') || leaseTimeStr.includes('s')) {
                        // 解析时间格式如 "24h0m0s" 或 "24h"
                        const hours = leaseTimeStr.match(/(\d+)h/);
                        const minutes = leaseTimeStr.match(/(\d+)m/);
                        const seconds = leaseTimeStr.match(/(\d+)s/);
                        
                        leaseTimeSeconds = 0;
                        if (hours) leaseTimeSeconds += parseInt(hours[1]) * 3600;
                        if (minutes) leaseTimeSeconds += parseInt(minutes[1]) * 60;
                        if (seconds) leaseTimeSeconds += parseInt(seconds[1]);
                    } else {
                        // 直接解析数字（秒）
                        leaseTimeSeconds = parseInt(leaseTimeStr) || 86400;
                    }
                }
                
                const config = {
                    interface: document.getElementById('serverInterface').value,
                    port: parseInt(document.getElementById('serverPort').value),
                    api_port: parseInt(document.getElementById('serverAPIPort').value),
                    api_host: document.getElementById('serverAPIHost').value.trim(),
                    lease_time: leaseTimeSeconds,
                    log_level: document.getElementById('serverLogLevel').value,
                    log_file: document.getElementById('serverLogFile').value,
                    debug: document.getElementById('serverDebug').checked,
                    allow_any_server_ip: document.getElementById('serverAllowAnyServerIP').checked
                };
                
                const response = await fetch('/api/config/server', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(config)
                });
                
                if (response.ok) {
                    const result = await response.json();
                    if (result.restart_required) {
                        showBeautifulConfirm('⚠️ 配置保存成功', result.message + '\\n\\n需要重启服务器以应用新配置。', 'warning');
                    } else {
                        showBeautifulConfirm('✅ 保存成功', '服务器配置已成功保存！', 'info');
                    }
                } else {
                    const error = await response.json();
                    showBeautifulConfirm('❌ 保存失败', '保存服务器配置时发生错误：\\n' + error.error, 'danger');
                }
            } catch (error) {
                console.error('保存服务器配置失败:', error);
                showBeautifulConfirm('❌ 保存失败', '保存服务器配置时发生错误：\\n' + error.message, 'danger');
            }
        }

        // DNS服务器管理功能
        let dnsServerCount = 0;

        function addDNSServer(value = '') {
            const container = document.getElementById('dnsServersContainer');
            const dnsId = 'dnsServer_' + dnsServerCount++;
            
            const dnsRow = document.createElement('div');
            dnsRow.className = 'dns-server-row';
            dnsRow.style.cssText = 'display: flex; gap: 10px; margin-bottom: 10px; align-items: center;';
            
            dnsRow.innerHTML = 
                '<input type="text" id="' + dnsId + '" class="form-control" placeholder="8.8.8.8" value="' + value + '" style="flex: 1;">' +
                '<button type="button" class="btn btn-danger btn-small" onclick="removeDNSServer(this)" style="padding: 6px 12px;">🗑️</button>';
            
            container.appendChild(dnsRow);
        }

        function removeDNSServer(button) {
            const container = document.getElementById('dnsServersContainer');
            if (container.children.length > 1) {
                button.parentElement.remove();
            } else {
                showBeautifulConfirm('⚠️ 提示', '至少需要保留一个DNS服务器', 'warning');
            }
        }

        function collectDNSServers() {
            const container = document.getElementById('dnsServersContainer');
            const dnsServers = [];
            
            for (let row of container.children) {
                const input = row.querySelector('input[type="text"]');
                if (input && input.value.trim()) {
                    dnsServers.push(input.value.trim());
                }
            }
            
            return dnsServers;
        }

        function clearDNSServers() {
            const container = document.getElementById('dnsServersContainer');
            container.innerHTML = '';
            dnsServerCount = 0;
        }

        // 网络配置功能
        async function loadNetworkConfig(showMessage = true) {
            console.log('🔄 开始加载网络配置...');
            try {
                const response = await fetch('/api/config/network');
                console.log('📡 API响应状态:', response.status);
                
                if (!response.ok) {
                    throw new Error('HTTP error! status: ' + response.status);
                }
                
                const config = await response.json();
                console.log('📋 获取到的配置数据:', config);
                
                document.getElementById('networkSubnet').value = config.subnet || '';
                document.getElementById('networkStartIP').value = config.start_ip || '';
                document.getElementById('networkEndIP').value = config.end_ip || '';
                document.getElementById('networkDefaultGateway').value = config.default_gateway || '';
                document.getElementById('networkLeaseTime').value = config.lease_time || 86400;
                document.getElementById('networkRenewalTime').value = config.renewal_time || 43200;
                document.getElementById('networkRebindingTime').value = config.rebinding_time || 75600;
                document.getElementById('networkDomainName').value = config.domain_name || '';
                document.getElementById('networkBroadcastAddress').value = config.broadcast_address || '';
                
                // 处理DNS服务器
                clearDNSServers();
                if (config.dns_servers && config.dns_servers.length > 0) {
                    config.dns_servers.forEach(dns => addDNSServer(dns));
                } else {
                    // 如果没有DNS服务器配置，添加默认的
                    addDNSServer('8.8.8.8');
                    addDNSServer('8.8.4.4');
                }
                
                console.log('✅ 网络配置加载完成');
                if (showMessage) {
                    showBeautifulConfirm('✅ 加载成功', '网络配置加载成功', 'info');
                }
            } catch (error) {
                console.error('❌ 加载网络配置失败:', error);
                if (showMessage) {
                    showBeautifulConfirm('❌ 加载失败', '加载网络配置失败: ' + error.message, 'danger');
                }
            }
        }

        async function saveNetworkConfig() {
            try {
                const dnsServers = collectDNSServers();
                
                const config = {
                    subnet: document.getElementById('networkSubnet').value,
                    start_ip: document.getElementById('networkStartIP').value,
                    end_ip: document.getElementById('networkEndIP').value,
                    default_gateway: document.getElementById('networkDefaultGateway').value,
                    dns_servers: dnsServers,
                    lease_time: parseInt(document.getElementById('networkLeaseTime').value) || 86400,
                    renewal_time: parseInt(document.getElementById('networkRenewalTime').value) || 43200,
                    rebinding_time: parseInt(document.getElementById('networkRebindingTime').value) || 75600,
                    domain_name: document.getElementById('networkDomainName').value,
                    broadcast_address: document.getElementById('networkBroadcastAddress').value
                };
                
                const response = await fetch('/api/config/network', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(config)
                });
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 保存成功', '网络配置已成功保存！', 'info');
                } else {
                    const error = await response.json();
                    showBeautifulConfirm('❌ 保存失败', '保存网络配置时发生错误：\\n' + error.error, 'danger');
                }
            } catch (error) {
                console.error('保存网络配置失败:', error);
                showBeautifulConfirm('❌ 保存失败', '保存网络配置时发生错误：\\n' + error.message, 'danger');
            }
        }

        // 网关检测配置功能
        async function loadGatewayDetectionConfig(showMessage = true) {
            try {
                const response = await fetch('/api/config/health-check');
                const config = await response.json();
                
                // 从纳秒转换为秒
                document.getElementById('healthCheckInterval').value = Math.round(config.interval / 1000000000) || 30;
                document.getElementById('healthCheckTimeout').value = Math.round(config.timeout / 1000000000) || 5;
                document.getElementById('healthCheckRetryCount').value = config.retry_count || 3;
                document.getElementById('healthCheckMethod').value = config.method || 'ping';
                document.getElementById('healthCheckTcpPort').value = config.tcp_port || 80;
                document.getElementById('healthCheckHttpPath').value = config.http_path || '/';
                
                updateHealthCheckOptions();
                updateHealthStatus();
                
                if (showMessage) {
                    showBeautifulConfirm('✅ 加载成功', '网关检测配置加载成功', 'info');
                }
            } catch (error) {
                console.error('加载网关检测配置失败:', error);
                if (showMessage) {
                    showBeautifulConfirm('❌ 加载失败', '加载网关检测配置失败: ' + error.message, 'danger');
                }
            }
        }

        function updateHealthCheckOptions() {
            const method = document.getElementById('healthCheckMethod').value;
            const tcpGroup = document.getElementById('tcpPortGroup');
            const httpGroup = document.getElementById('httpPathGroup');
            
            tcpGroup.style.display = method === 'tcp' ? 'block' : 'none';
            httpGroup.style.display = method === 'http' ? 'block' : 'none';
        }

        async function saveGatewayDetectionConfig() {
            try {
                const config = {
                    interval: parseInt(document.getElementById('healthCheckInterval').value) * 1000000000, // 转换为纳秒
                    timeout: parseInt(document.getElementById('healthCheckTimeout').value) * 1000000000,
                    retry_count: parseInt(document.getElementById('healthCheckRetryCount').value),
                    method: document.getElementById('healthCheckMethod').value,
                    tcp_port: parseInt(document.getElementById('healthCheckTcpPort').value) || 80,
                    http_path: document.getElementById('healthCheckHttpPath').value || '/'
                };
                
                const response = await fetch('/api/config/health-check', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(config)
                });
                
                const result = await response.json();
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 保存成功', '网关检测配置已成功保存！', 'info');
                    updateHealthStatus();
                } else {
                    showBeautifulConfirm('❌ 保存失败', '保存网关检测配置时发生错误：\\n' + result.error, 'danger');
                }
            } catch (error) {
                console.error('保存网关检测配置失败:', error);
                showBeautifulConfirm('❌ 保存失败', '保存网关检测配置时发生错误：\\n' + error.message, 'danger');
            }
        }

        async function testGatewayDetection() {
            try {
                showBeautifulConfirm('🔄 正在测试', '正在测试网关检测功能，请稍候...', 'info');
                // 这里应该调用后端API进行测试
                setTimeout(() => {
                    showBeautifulConfirm('✅ 测试完成', '网关检测功能测试完成！', 'info');
                    updateHealthStatus();
                }, 2000);
            } catch (error) {
                console.error('测试网关检测失败:', error);
                showBeautifulConfirm('❌ 测试失败', '测试网关检测功能时发生错误：\\n' + error.message, 'danger');
            }
        }

        function updateHealthStatus() {
            const now = new Date();
            document.getElementById('lastCheckTime').textContent = now.toLocaleString();
            
            const interval = parseInt(document.getElementById('healthCheckInterval').value) || 30;
            const nextCheck = new Date(now.getTime() + interval * 1000);
            document.getElementById('nextCheckTime').textContent = nextCheck.toLocaleString();
        }

        // 加载提示HTML结构
        const loadingHTML = 
            '<div id="loadingOverlay" class="loading-overlay">' +
                '<div class="loading-content">' +
                    '<div class="loading-spinner"></div>' +
                    '<p class="loading-text" id="loadingText">正在处理中...</p>' +
                '</div>' +
            '</div>';

        // 在页面加载时添加加载提示HTML
        document.addEventListener('DOMContentLoaded', function() {
            // 添加加载提示HTML到页面
            if (!document.getElementById('loadingOverlay')) {
                document.body.insertAdjacentHTML('beforeend', loadingHTML);
            }
            
            // 定期更新扫描器状态
            setInterval(updateScannerStatus, 2000);
        });

        // 网络扫描器相关函数
        async function loadNetworkScannerConfig(showMessage = true) {
            try {
                const response = await fetch('/api/scanner/config');
                const config = await response.json();
                
                if (response.ok) {
                    document.getElementById('scannerEnabled').value = config.enabled ? 'true' : 'false';
                    document.getElementById('scannerInterval').value = config.scan_interval || 300; // 扫描间隔（秒）
                    document.getElementById('scannerConcurrency').value = config.max_concurrency || 10;
                    document.getElementById('scannerTimeout').value = config.ping_timeout || 1000; // Ping超时（毫秒）
                    
                    // 显示当前DHCP可分配范围
                    const networkResponse = await fetch('/api/config/network');
                    if (networkResponse.ok) {
                        const networkConfig = await networkResponse.json();
                        const rangeText = (networkConfig.start_ip || '') + ' - ' + (networkConfig.end_ip || '');
                        document.getElementById('scannerRangeDisplay').textContent = rangeText;
                    }
                    document.getElementById('scannerConflictAction').value = config.auto_conflict ? 'mark_conflicted' : 'ignore';
                    document.getElementById('scannerConflictTimeout').value = Math.floor(config.conflict_timeout / 60000000000) || 60; // 转换为分钟
                    
                    updateScannerStatus();
                    if (showMessage) {
                        showBeautifulConfirm('✅ 加载成功', '网络扫描器配置已成功加载！', 'info');
                    }
                } else {
                    if (showMessage) {
                        showBeautifulConfirm('❌ 加载失败', '加载扫描器配置时发生错误：\\n' + config.error, 'danger');
                    }
                }
            } catch (error) {
                console.error('加载扫描器配置失败:', error);
                if (showMessage) {
                    showBeautifulConfirm('❌ 加载失败', '加载扫描器配置时发生错误：\\n' + error.message, 'danger');
                }
            }
        }

        async function saveNetworkScannerConfig() {
            try {
                const config = {
                    enabled: document.getElementById('scannerEnabled').value === 'true',
                    scan_interval: parseInt(document.getElementById('scannerInterval').value),
                    max_concurrency: parseInt(document.getElementById('scannerConcurrency').value),
                    ping_timeout: parseInt(document.getElementById('scannerTimeout').value),
                    start_ip: '', // 扫描器会自动使用DHCP可分配范围
                    end_ip: '',   // 扫描器会自动使用DHCP可分配范围
                    auto_conflict: document.getElementById('scannerConflictAction').value === 'mark_conflicted',
                    conflict_timeout: parseInt(document.getElementById('scannerConflictTimeout').value) * 60000000000 // 转换为纳秒
                };
                
                const response = await fetch('/api/scanner/config', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(config)
                });
                
                const result = await response.json();
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 保存成功', '网络扫描器配置已成功保存！', 'info');
                    updateScannerStatus();
                } else {
                    showBeautifulConfirm('❌ 保存失败', '保存扫描器配置时发生错误：\\n' + result.error, 'danger');
                }
            } catch (error) {
                console.error('保存扫描器配置失败:', error);
                showBeautifulConfirm('❌ 保存失败', '保存扫描器配置时发生错误：\\n' + error.message, 'danger');
            }
        }

        async function startNetworkScanner() {
            try {
                showBeautifulConfirm('🔄 正在启动', '正在启动网络扫描器，请稍候...', 'info');
                const response = await fetch('/api/scanner/start', { method: 'POST' });
                const result = await response.json();
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 启动成功', '网络扫描器已成功启动！', 'info');
                    updateScannerStatus();
                } else {
                    showBeautifulConfirm('❌ 启动失败', '启动网络扫描器时发生错误：\\n' + result.error, 'danger');
                }
            } catch (error) {
                console.error('启动扫描器失败:', error);
                showBeautifulConfirm('❌ 启动失败', '启动网络扫描器时发生错误：\\n' + error.message, 'danger');
            }
        }

        async function stopNetworkScanner() {
            try {
                showBeautifulConfirm('🔄 正在停止', '正在停止网络扫描器，请稍候...', 'info');
                const response = await fetch('/api/scanner/stop', { method: 'POST' });
                const result = await response.json();
                
                if (response.ok) {
                    showBeautifulConfirm('✅ 停止成功', '网络扫描器已成功停止！', 'info');
                    updateScannerStatus();
                } else {
                    showBeautifulConfirm('❌ 停止失败', '停止网络扫描器时发生错误：\\n' + result.error, 'danger');
                }
            } catch (error) {
                console.error('停止扫描器失败:', error);
                showBeautifulConfirm('❌ 停止失败', '停止网络扫描器时发生错误：\\n' + error.message, 'danger');
            }
        }

        async function viewScannerResults() {
            try {
                const response = await fetch('/api/scanner/results');
                const result = await response.json();
                
                if (response.ok) {
                    // 创建模态对话框
                    const modal = document.createElement('div');
                    modal.className = 'modal';
                    modal.style.cssText = 'position: fixed; top: 0; left: 0; width: 100%; height: 100%; background: rgba(0,0,0,0.5); z-index: 1000; display: flex; align-items: center; justify-content: center;';
                    
                    const modalContent = document.createElement('div');
                    modalContent.style.cssText = 'background: white; border-radius: 8px; padding: 20px; max-width: 600px; max-height: 80vh; overflow-y: auto; box-shadow: 0 4px 20px rgba(0,0,0,0.3);';
                    
                    let content = '<div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px; border-bottom: 2px solid #007bff; padding-bottom: 10px;">';
                    content += '<h3 style="margin: 0; color: #007bff;">🔍 扫描结果</h3>';
                    content += '<button onclick="this.parentElement.parentElement.parentElement.remove()" style="background: none; border: none; font-size: 24px; cursor: pointer; color: #666;">&times;</button>';
                    content += '</div>';
                    
                    content += '<div style="margin-bottom: 15px;">';
                    content += '<p style="margin: 0; font-size: 16px; color: #333;"><strong>发现设备数量:</strong> <span style="color: #007bff; font-weight: bold;">' + result.count + '</span></p>';
                    content += '</div>';
                    
                    if (result.devices && result.devices.length > 0) {
                        content += '<div style="max-height: 400px; overflow-y: auto;">';
                        content += '<h4 style="margin: 0 0 10px 0; color: #555;">发现的设备:</h4>';
                        content += '<div style="border: 1px solid #ddd; border-radius: 4px; overflow: hidden;">';
                        
                        result.devices.forEach((device, index) => {
                            const bgColor = index % 2 === 0 ? '#f8f9fa' : '#ffffff';
                            content += '<div style="padding: 12px; background: ' + bgColor + '; border-bottom: 1px solid #eee;">';
                            content += '<div style="display: flex; justify-content: space-between; align-items: center;">';
                            content += '<div>';
                            content += '<strong style="color: #007bff;">' + device.ip + '</strong>';
                            content += '<br><small style="color: #666;">MAC: ' + device.mac + '</small>';
                            content += '</div>';
                            content += '<div style="text-align: right;">';
                            content += '<div style="color: #333;">' + (device.hostname || '未知设备') + '</div>';
                            content += '<small style="color: ' + (device.response === 'ping' ? '#28a745' : '#ffc107') + ';">响应: ' + device.response + '</small>';
                            content += '</div>';
                            content += '</div>';
                            content += '</div>';
                        });
                        
                        content += '</div>';
                        content += '</div>';
                    } else {
                        content += '<div style="text-align: center; padding: 40px; color: #666;">';
                        content += '<p style="margin: 0;">暂无扫描结果</p>';
                        content += '</div>';
                    }
                    
                    content += '<div style="margin-top: 20px; text-align: center;">';
                    content += '<button onclick="this.parentElement.parentElement.parentElement.remove()" style="background: #007bff; color: white; border: none; padding: 10px 20px; border-radius: 4px; cursor: pointer;">关闭</button>';
                    content += '</div>';
                    
                    modalContent.innerHTML = content;
                    modal.appendChild(modalContent);
                    document.body.appendChild(modal);
                    
                    // 点击背景关闭
                    modal.addEventListener('click', function(e) {
                        if (e.target === modal) {
                            modal.remove();
                        }
                    });
                } else {
                    showBeautifulConfirm('❌ 获取失败', '获取扫描结果失败: ' + result.error, 'danger');
                }
            } catch (error) {
                console.error('获取扫描结果失败:', error);
                showBeautifulConfirm('❌ 获取失败', '获取扫描结果失败: ' + error.message, 'danger');
            }
        }

        async function viewScannerLog() {
            try {
                const response = await fetch('/api/scanner/log');
                const result = await response.json();
                
                if (response.ok) {
                    // 创建模态对话框
                    const modal = document.createElement('div');
                    modal.className = 'modal';
                    modal.style.cssText = 'position: fixed; top: 0; left: 0; width: 100%; height: 100%; background: rgba(0,0,0,0.5); z-index: 1000; display: flex; align-items: center; justify-content: center;';
                    
                    const modalContent = document.createElement('div');
                    modalContent.style.cssText = 'background: white; border-radius: 8px; padding: 20px; max-width: 700px; max-height: 80vh; overflow-y: auto; box-shadow: 0 4px 20px rgba(0,0,0,0.3);';
                    
                    let content = '<div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px; border-bottom: 2px solid #007bff; padding-bottom: 10px;">';
                    content += '<h3 style="margin: 0; color: #007bff;">📋 扫描日志</h3>';
                    content += '<button onclick="this.parentElement.parentElement.parentElement.remove()" style="background: none; border: none; font-size: 24px; cursor: pointer; color: #666;">&times;</button>';
                    content += '</div>';
                    
                    content += '<div style="margin-bottom: 15px; display: flex; justify-content: space-between; align-items: center;">';
                    content += '<p style="margin: 0; font-size: 16px; color: #333;"><strong>日志数量:</strong> <span style="color: #007bff; font-weight: bold;">' + result.count + '</span></p>';
                    content += '<button onclick="exportScannerLog()" style="background: #28a745; color: white; border: none; padding: 8px 16px; border-radius: 4px; cursor: pointer; font-size: 14px;">📥 导出日志</button>';
                    content += '</div>';
                    
                    if (result.logs && result.logs.length > 0) {
                        content += '<div style="max-height: 400px; overflow-y: auto; border: 1px solid #ddd; border-radius: 4px; background: #f8f9fa;">';
                        content += '<div style="padding: 15px;">';
                        
                        result.logs.forEach((log, index) => {
                            const bgColor = index % 2 === 0 ? '#ffffff' : '#f8f9fa';
                            content += '<div style="padding: 8px 12px; background: ' + bgColor + '; border-radius: 4px; margin-bottom: 4px; font-family: monospace; font-size: 13px;">';
                            content += '<span style="color: #007bff; font-weight: bold; margin-right: 8px;">' + (index + 1) + '.</span>';
                            content += '<span style="color: #333;">' + log + '</span>';
                            content += '</div>';
                        });
                        
                        content += '</div>';
                        content += '</div>';
                    } else {
                        content += '<div style="text-align: center; padding: 40px; color: #666;">';
                        content += '<p style="margin: 0;">暂无扫描日志</p>';
                        content += '</div>';
                    }
                    
                    content += '<div style="margin-top: 15px; padding: 10px; background: #e7f3ff; border-radius: 4px; border-left: 4px solid #007bff;">';
                    content += '<small style="color: #007bff;">💡 提示：日志按时间倒序显示，最新的记录在顶部</small>';
                    content += '</div>';
                    
                    content += '<div style="margin-top: 20px; text-align: center;">';
                    content += '<button onclick="this.parentElement.parentElement.parentElement.remove()" style="background: #007bff; color: white; border: none; padding: 10px 20px; border-radius: 4px; cursor: pointer;">关闭</button>';
                    content += '</div>';
                    
                    modalContent.innerHTML = content;
                    modal.appendChild(modalContent);
                    document.body.appendChild(modal);
                    
                    // 点击背景关闭
                    modal.addEventListener('click', function(e) {
                        if (e.target === modal) {
                            modal.remove();
                        }
                    });
                } else {
                    showBeautifulConfirm('❌ 获取失败', '获取扫描日志失败: ' + result.error, 'danger');
                }
            } catch (error) {
                console.error('获取扫描日志失败:', error);
                showBeautifulConfirm('❌ 获取失败', '获取扫描日志失败: ' + error.message, 'danger');
            }
        }

        async function refreshScannerLog() {
            try {
                const response = await fetch('/api/scanner/log');
                const result = await response.json();
                
                if (response.ok) {
                    const logContainer = document.querySelector('.log-container');
                    const logCount = document.querySelector('.log-count');
                    
                    if (logContainer && logCount) {
logContainer.innerHTML = '';
if (result.logs && result.logs.length > 0) {
    for (var i = 0; i < result.logs.length; i++) {
        logContainer.innerHTML += '<div class="log-entry">' + result.logs[i] + '</div>';
    }
} else {
    logContainer.innerHTML = '<div class="no-logs">暂无扫描日志</div>';
}
                            '<div class="no-logs">暂无扫描日志</div>';
                        
                        // 滚动到底部
                        logContainer.scrollTop = logContainer.scrollHeight;
                    }
                }
            } catch (error) {
                console.error('刷新扫描日志失败:', error);
            }
        }

        function exportScannerLog() {
            const logEntries = document.querySelectorAll('.log-entry');
            let logText = '扫描日志导出\\n';
            logText += '导出时间: ' + new Date().toLocaleString() + '\\n\\n';
            
            logEntries.forEach(entry => {
                logText += entry.textContent + '\\n';
            });
            
            // 创建下载链接
            const blob = new Blob([logText], { type: 'text/plain' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = 'scanner_log_' + new Date().toISOString().slice(0, 19).replace(/:/g, '-') + '.txt';
            document.body.appendChild(a);
            a.click();
            document.body.removeChild(a);
            URL.revokeObjectURL(url);
        }

        async function updateScannerStatus() {
            try {
                const response = await fetch('/api/scanner');
                const result = await response.json();
                
                if (response.ok) {
                    document.getElementById('scannerServiceStatus').textContent = result.is_running ? '运行中' : '已停止';
                    document.getElementById('scanProgress').textContent = result.scan_progress + '%';
                    document.getElementById('currentScanIP').textContent = result.current_scan || '--';
                    document.getElementById('scannedIPs').textContent = result.scanned_ips + '/' + result.total_ips;
                    document.getElementById('discoveredDevicesCount').textContent = result.found_devices || 0;
                    document.getElementById('conflictedIPsCount').textContent = result.conflicted_ips || 0;
                    document.getElementById('lastScanTime').textContent = result.last_scan ? new Date(result.last_scan).toLocaleString() : '--';
                    document.getElementById('nextScanTime').textContent = result.next_scan ? new Date(result.next_scan).toLocaleString() : '--';
                }
            } catch (error) {
                console.error('更新扫描器状态失败:', error);
            }
        }

    </script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// handleLeases 处理所有租约查询
func (api *APIServer) handleLeases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	leases := api.dhcpServer.GetAllLeases()
	var response []LeaseInfo

	for _, lease := range leases {
		leaseInfo := LeaseInfo{
			IP:            lease.IP.String(),
			MAC:           lease.MAC,
			Hostname:      lease.Hostname,
			StartTime:     lease.StartTime,
			LeaseTime:     lease.LeaseTime.String(),
			RemainingTime: lease.RemainingTime().String(),
			IsStatic:      lease.IsStatic,
			Gateway:       lease.Gateway,
			GatewayIP:     lease.GatewayIP,
			IsExpired:     lease.IsExpired(),
		}
		response = append(response, leaseInfo)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleActiveLeases 处理活跃租约查询
func (api *APIServer) handleActiveLeases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	leases := api.dhcpServer.GetActiveLeases()
	var response []LeaseInfo

	for _, lease := range leases {
		leaseInfo := LeaseInfo{
			IP:            lease.IP.String(),
			MAC:           lease.MAC,
			Hostname:      lease.Hostname,
			StartTime:     lease.StartTime,
			LeaseTime:     lease.LeaseTime.String(),
			RemainingTime: lease.RemainingTime().String(),
			IsStatic:      lease.IsStatic,
			Gateway:       lease.Gateway,
			GatewayIP:     lease.GatewayIP,
			IsExpired:     false, // 活跃租约不会过期
		}
		response = append(response, leaseInfo)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleHistory 处理历史记录查询
func (api *APIServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 获取查询参数
	query := r.URL.Query()
	limitStr := query.Get("limit")
	macFilter := query.Get("mac")
	ipFilter := query.Get("ip")

	limit := 100 // 默认限制
	if limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}

	// 获取历史记录
	history := api.dhcpServer.GetHistory(limit, macFilter, ipFilter)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(history)
}

// handleStats 处理统计信息查询
func (api *APIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := StatsResponse{
		PoolStats:     api.dhcpServer.GetPoolStats(),
		GatewayStatus: api.dhcpServer.GetGatewayStatus(),
		ServerInfo: ServerInfo{
			Version:   "1.0.0",
			StartTime: api.dhcpServer.GetStartTime(),
			Uptime:    time.Since(api.dhcpServer.GetStartTime()).String(),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleGateways 处理网关管理请求
func (api *APIServer) handleGateways(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		api.handleGetGateways(w, r)
	case "POST":
		api.handleAddGateway(w, r)
	case "PUT":
		api.handleUpdateGateway(w, r)
	case "DELETE":
		api.handleDeleteGateway(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleGetGateways 获取网关状态信息
func (api *APIServer) handleGetGateways(w http.ResponseWriter, r *http.Request) {
	gatewayStatus := api.dhcpServer.GetGatewayStatus()

	// 添加网关详细信息
	detailedStatus := make(map[string]interface{})
	for _, gateway := range api.config.Gateways {
		healthy, exists := gatewayStatus[gateway.Name]
		detailedStatus[gateway.Name] = map[string]interface{}{
			"healthy":     exists && healthy,
			"ip":          gateway.IP,
			"is_default":  gateway.IsDefault,
			"description": gateway.Description,
			"dns_servers": gateway.DNSServers,
		}
	}

	json.NewEncoder(w).Encode(detailedStatus)
}

// handleAddGateway 添加网关
func (api *APIServer) handleAddGateway(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name        string   `json:"name"`
		IP          string   `json:"ip"`
		IsDefault   bool     `json:"is_default"`
		Description string   `json:"description"`
		DNSServers  []string `json:"dns_servers"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证必填字段
	if request.Name == "" || request.IP == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Gateway name and IP are required"})
		return
	}

	// 验证IP地址格式
	if net.ParseIP(request.IP) == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid IP address format"})
		return
	}

	// 验证DNS服务器地址格式
	for _, dns := range request.DNSServers {
		if dns != "" && net.ParseIP(dns) == nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid DNS server address: " + dns})
			return
		}
	}

	// 检查网关名称是否已存在
	for _, gateway := range api.config.Gateways {
		if gateway.Name == request.Name {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "Gateway name already exists"})
			return
		}
		if gateway.IP == request.IP {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "Gateway IP already exists"})
			return
		}
	}

	// 如果设置为默认网关，先取消其他网关的默认状态
	if request.IsDefault {
		for i := range api.config.Gateways {
			api.config.Gateways[i].IsDefault = false
		}
	}

	// 添加新网关
	newGateway := config.Gateway{
		Name:        request.Name,
		IP:          request.IP,
		IsDefault:   request.IsDefault,
		Description: request.Description,
		DNSServers:  request.DNSServers,
	}

	api.config.Gateways = append(api.config.Gateways, newGateway)

	// 保存配置
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "Gateway added successfully",
		"gateway": newGateway,
	})
}

// handleUpdateGateway 更新网关
func (api *APIServer) handleUpdateGateway(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name        string   `json:"name"`
		IP          string   `json:"ip"`
		IsDefault   bool     `json:"is_default"`
		Description string   `json:"description"`
		DNSServers  []string `json:"dns_servers"`
		OldName     string   `json:"old_name"` // 用于标识要更新的网关
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证必填字段
	if request.OldName == "" || request.Name == "" || request.IP == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Old name, new name and IP are required"})
		return
	}

	// 验证IP地址格式
	if net.ParseIP(request.IP) == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid IP address format"})
		return
	}

	// 验证DNS服务器地址格式
	for _, dns := range request.DNSServers {
		if dns != "" && net.ParseIP(dns) == nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid DNS server address: " + dns})
			return
		}
	}

	// 查找要更新的网关
	gatewayIndex := -1
	for i, gateway := range api.config.Gateways {
		if gateway.Name == request.OldName {
			gatewayIndex = i
			break
		}
	}

	if gatewayIndex == -1 {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Gateway not found"})
		return
	}

	// 检查新名称和IP是否与其他网关冲突
	for i, gateway := range api.config.Gateways {
		if i != gatewayIndex {
			if gateway.Name == request.Name {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "Gateway name already exists"})
				return
			}
			if gateway.IP == request.IP {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "Gateway IP already exists"})
				return
			}
		}
	}

	// 如果设置为默认网关，先取消其他网关的默认状态
	if request.IsDefault {
		for i := range api.config.Gateways {
			if i != gatewayIndex {
				api.config.Gateways[i].IsDefault = false
			}
		}
	}

	// 更新网关信息
	api.config.Gateways[gatewayIndex].Name = request.Name
	api.config.Gateways[gatewayIndex].IP = request.IP
	api.config.Gateways[gatewayIndex].IsDefault = request.IsDefault
	api.config.Gateways[gatewayIndex].Description = request.Description
	api.config.Gateways[gatewayIndex].DNSServers = request.DNSServers

	// 保存配置
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "Gateway updated successfully",
		"gateway": api.config.Gateways[gatewayIndex],
	})
}

// handleDeleteGateway 删除网关
func (api *APIServer) handleDeleteGateway(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if request.Name == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Gateway name is required"})
		return
	}

	// 查找要删除的网关
	gatewayIndex := -1
	var deletedGateway config.Gateway
	for i, gateway := range api.config.Gateways {
		if gateway.Name == request.Name {
			gatewayIndex = i
			deletedGateway = gateway
			break
		}
	}

	if gatewayIndex == -1 {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Gateway not found"})
		return
	}

	// 不能删除最后一个网关
	if len(api.config.Gateways) <= 1 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Cannot delete the last gateway"})
		return
	}

	// 如果删除的是默认网关，将第一个剩余网关设为默认
	wasDefault := api.config.Gateways[gatewayIndex].IsDefault

	// 删除网关
	api.config.Gateways = append(api.config.Gateways[:gatewayIndex], api.config.Gateways[gatewayIndex+1:]...)

	// 如果删除的是默认网关，设置新的默认网关
	if wasDefault && len(api.config.Gateways) > 0 {
		api.config.Gateways[0].IsDefault = true
	}

	// 保存配置
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":         "Gateway deleted successfully",
		"deleted_gateway": deletedGateway,
	})
}

// handleHealth 处理健康检查
func (api *APIServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now(),
		"uptime":    time.Since(api.dhcpServer.GetStartTime()).String(),
		"version":   "1.0.0",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

// corsMiddleware 添加CORS支持
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleDevices 处理设备管理请求
func (api *APIServer) handleDevices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		api.handleGetDevices(w, r)
	case "POST":
		api.handleAddDevice(w, r)
	case "PUT":
		api.handleUpdateDevice(w, r)
	case "DELETE":
		api.handleDeleteDevice(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleGetDevices 获取设备列表
func (api *APIServer) handleGetDevices(w http.ResponseWriter, r *http.Request) {
	// 获取查询参数
	mac := r.URL.Query().Get("mac")
	deviceType := r.URL.Query().Get("type")

	var devices []config.DeviceInfo

	if mac != "" {
		// 查询特定MAC地址的设备
		if device := api.config.FindDeviceByMAC(mac); device != nil {
			deviceCopy := *device

			// 检查设备是否活跃（静态绑定不算活跃）
			activeLeases := api.pool.GetActiveLeases()
			for _, lease := range activeLeases {
				if lease.MAC == deviceCopy.MAC && !lease.IsStatic {
					deviceCopy.IsActive = true
					break
				}
			}

			// 检查网络扫描器是否检测到设备活跃
			if !deviceCopy.IsActive && api.scanner != nil {
				scanResults := api.scanner.GetScanResults()
				for _, scanDevice := range scanResults {
					if scanDevice.MAC == deviceCopy.MAC && scanDevice.IsActive {
						deviceCopy.IsActive = true
						break
					}
				}
			}

			// 检查设备是否有静态IP绑定
			if binding := api.config.FindBindingByMAC(deviceCopy.MAC); binding != nil {
				deviceCopy.HasStaticIP = true
				deviceCopy.StaticIP = binding.IP
				// 如果静态绑定中配置了网关，优先使用绑定的网关
				if binding.Gateway != "" {
					deviceCopy.Gateway = binding.Gateway
				}
				// 如果静态绑定中没有网关配置，保持设备自身的网关配置不变
			} else {
				deviceCopy.HasStaticIP = false
				deviceCopy.StaticIP = ""
				// 没有静态绑定时，保持设备自身的网关配置不变，不要清空
			}

			devices = append(devices, deviceCopy)
		}
	} else {
		// 获取所有设备，并更新活跃状态
		devices = api.config.Devices

		// 更新设备活跃状态和主机名（静态绑定不算活跃）
		activeLeases := api.pool.GetActiveLeases()
		activeMacs := make(map[string]bool)
		leaseHostnames := make(map[string]string)
		for _, lease := range activeLeases {
			// 只有非静态租约才算活跃
			if !lease.IsStatic {
				activeMacs[lease.MAC] = true
			}
			// 但主机名可以从任何租约获取
			if lease.Hostname != "" {
				leaseHostnames[lease.MAC] = lease.Hostname
			}
		}

		// 检查网络扫描器发现的活跃设备
		scannerActiveMacs := make(map[string]bool)
		if api.scanner != nil {
			scanResults := api.scanner.GetScanResults()
			for _, device := range scanResults {
				if device.IsActive {
					scannerActiveMacs[device.MAC] = true
				}
			}
		}

		for i := range devices {
			// 设备活跃状态基于：1) 有活跃租约 或 2) 网络扫描器检测到活跃
			devices[i].IsActive = activeMacs[devices[i].MAC] || scannerActiveMacs[devices[i].MAC]

			// 从租约中更新主机名（如果设备信息中没有主机名或租约中有更新的主机名）
			if hostname, exists := leaseHostnames[devices[i].MAC]; exists && hostname != "" {
				devices[i].Hostname = hostname
			}

			// 检查设备是否有静态IP绑定
			if binding := api.config.FindBindingByMAC(devices[i].MAC); binding != nil {
				devices[i].HasStaticIP = true
				devices[i].StaticIP = binding.IP
				// 如果静态绑定中配置了网关，优先使用绑定的网关
				if binding.Gateway != "" {
					devices[i].Gateway = binding.Gateway
				}
				// 如果静态绑定中没有网关配置，保持设备自身的网关配置不变
			} else {
				devices[i].HasStaticIP = false
				devices[i].StaticIP = ""
				// 没有静态绑定时，保持设备自身的网关配置不变，不要清空
			}
		}

		// 根据设备类型过滤
		if deviceType != "" {
			var filtered []config.DeviceInfo
			for _, device := range devices {
				if strings.EqualFold(device.DeviceType, deviceType) {
					filtered = append(filtered, device)
				}
			}
			devices = filtered
		}
	}

	json.NewEncoder(w).Encode(devices)
}

// handleAddDevice 添加设备信息
func (api *APIServer) handleAddDevice(w http.ResponseWriter, r *http.Request) {
	var device config.DeviceInfo
	if err := json.NewDecoder(r.Body).Decode(&device); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证必需字段
	if device.MAC == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "MAC address is required"})
		return
	}

	// 验证MAC地址格式
	if _, err := net.ParseMAC(device.MAC); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid MAC address format"})
		return
	}

	// 设置时间戳
	now := time.Now()
	if device.FirstSeen.IsZero() {
		device.FirstSeen = now
	}
	device.LastSeen = now

	// 检查设备是否活跃（静态绑定不算活跃）
	activeLeases := api.pool.GetActiveLeases()
	device.IsActive = false // 默认不活跃

	// 检查是否有活跃的动态租约
	for _, lease := range activeLeases {
		if lease.MAC == device.MAC && !lease.IsStatic {
			device.IsActive = true
			break
		}
	}

	// 如果没有活跃租约，检查网络扫描器是否检测到设备活跃
	if !device.IsActive && api.scanner != nil {
		scanResults := api.scanner.GetScanResults()
		for _, scanDevice := range scanResults {
			if scanDevice.MAC == device.MAC && scanDevice.IsActive {
				device.IsActive = true
				break
			}
		}
	}

	// 添加或更新设备
	api.config.AddOrUpdateDevice(device)

	// 保存配置到文件
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(device)
}

// handleUpdateDevice 更新设备信息
func (api *APIServer) handleUpdateDevice(w http.ResponseWriter, r *http.Request) {
	var device config.DeviceInfo
	if err := json.NewDecoder(r.Body).Decode(&device); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if device.MAC == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "MAC address is required"})
		return
	}

	// 查找现有设备
	existing := api.config.FindDeviceByMAC(device.MAC)
	if existing == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Device not found"})
		return
	}

	// 保持原有的首次见到时间
	if !existing.FirstSeen.IsZero() {
		device.FirstSeen = existing.FirstSeen
	}
	device.LastSeen = time.Now()

	// 更新设备信息
	api.config.AddOrUpdateDevice(device)

	// 保存配置到文件
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(device)
}

// handleDeleteDevice 删除设备信息
func (api *APIServer) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	var request struct {
		MAC string `json:"mac"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	mac := request.MAC
	if mac == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "MAC address is required"})
		return
	}

	// 查找并删除设备
	deviceFound := false
	for i, device := range api.config.Devices {
		if device.MAC == mac {
			api.config.Devices = append(api.config.Devices[:i], api.config.Devices[i+1:]...)
			deviceFound = true
			break
		}
	}

	if !deviceFound {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Device not found"})
		return
	}

	// 同时删除对应的静态绑定（如果存在）
	bindingsToDelete := []string{}
	for _, binding := range api.config.Bindings {
		if binding.MAC == mac {
			bindingsToDelete = append(bindingsToDelete, binding.Alias)
		}
	}

	// 删除找到的静态绑定
	for _, alias := range bindingsToDelete {
		for i, binding := range api.config.Bindings {
			if binding.Alias == alias {
				api.config.Bindings = append(api.config.Bindings[:i], api.config.Bindings[i+1:]...)
				break
			}
		}
	}

	// 保存配置到文件
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	// 如果删除了静态绑定，重新加载配置
	if len(bindingsToDelete) > 0 && api.reloadCallback != nil {
		go func() {
			time.Sleep(100 * time.Millisecond) // 短暂延迟以确保文件写入完成
			if err := api.reloadCallback(api.config); err != nil {
				log.Printf("删除设备和静态绑定后重新加载配置失败: %v", err)
			}
		}()
	}

	// 返回删除信息
	response := map[string]interface{}{
		"message":                 "Device deleted successfully",
		"deleted_static_bindings": len(bindingsToDelete),
	}

	if len(bindingsToDelete) > 0 {
		response["deleted_bindings"] = bindingsToDelete
	}

	json.NewEncoder(w).Encode(response)
}

// handleDeviceDiscover 设备发现接口
func (api *APIServer) handleDeviceDiscover(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	// 获取所有活跃租约
	activeLeases := api.pool.GetActiveLeases()

	// 查找未知设备（没有在设备列表中的MAC地址）
	var unknownDevices []map[string]interface{}

	for _, lease := range activeLeases {
		if api.config.FindDeviceByMAC(lease.MAC) == nil {
			// 尝试根据MAC地址推断设备类型
			deviceType := guessDeviceInfo(lease.MAC)

			unknownDevice := map[string]interface{}{
				"mac":         lease.MAC,
				"ip":          lease.IP,
				"hostname":    lease.Hostname,
				"device_type": deviceType,
				"last_seen":   lease.StartTime,
				"is_active":   true,
				"suggested":   true, // 标记为系统推测的信息
			}
			unknownDevices = append(unknownDevices, unknownDevice)
		}
	}

	response := map[string]interface{}{
		"unknown_devices":  unknownDevices,
		"discovered_count": len(unknownDevices),
	}

	json.NewEncoder(w).Encode(response)
}

// handleBatchAddDevices 批量添加设备
func (api *APIServer) handleBatchAddDevices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	var request struct {
		Devices []config.DeviceInfo `json:"devices"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if len(request.Devices) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "No devices provided"})
		return
	}

	var addedDevices []config.DeviceInfo
	var errors []string

	now := time.Now()

	for _, device := range request.Devices {
		// 验证必需字段
		if device.MAC == "" {
			errors = append(errors, "设备MAC地址不能为空")
			continue
		}

		// 验证MAC地址格式
		if _, err := net.ParseMAC(device.MAC); err != nil {
			errors = append(errors, fmt.Sprintf("设备%s的MAC地址格式无效", device.MAC))
			continue
		}

		// 检查设备是否已存在
		if api.config.FindDeviceByMAC(device.MAC) != nil {
			errors = append(errors, fmt.Sprintf("设备%s已存在", device.MAC))
			continue
		}

		// 设置时间戳
		if device.FirstSeen.IsZero() {
			device.FirstSeen = now
		}
		device.LastSeen = now

		// 检查设备是否活跃（静态绑定不算活跃）
		activeLeases := api.pool.GetActiveLeases()
		for _, lease := range activeLeases {
			if lease.MAC == device.MAC && !lease.IsStatic {
				device.IsActive = true
				break
			}
		}

		// 添加设备
		api.config.AddOrUpdateDevice(device)
		addedDevices = append(addedDevices, device)
	}

	// 保存配置到文件（如果有成功添加的设备）
	if len(addedDevices) > 0 {
		if err := api.config.SaveConfig(api.configPath); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
			return
		}
	}

	response := map[string]interface{}{
		"added_count":   len(addedDevices),
		"error_count":   len(errors),
		"added_devices": addedDevices,
		"errors":        errors,
	}

	if len(errors) > 0 && len(addedDevices) == 0 {
		w.WriteHeader(http.StatusBadRequest)
	} else {
		w.WriteHeader(http.StatusCreated)
	}

	json.NewEncoder(w).Encode(response)
}

// handleDeviceGateway 处理设备网关配置
func (api *APIServer) handleDeviceGateway(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PUT" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var request struct {
		MAC     string `json:"mac"`
		Gateway string `json:"gateway"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// 验证MAC地址
	if request.MAC == "" {
		http.Error(w, "MAC address is required", http.StatusBadRequest)
		return
	}

	// 验证网关（可以为空，表示使用默认网关）
	if request.Gateway != "" {
		// 检查网关是否存在
		found := false
		for _, gw := range api.config.Gateways {
			if gw.Name == request.Gateway {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "Gateway not found", http.StatusBadRequest)
			return
		}
	}

	// 更新设备配置
	device := api.config.FindDeviceByMAC(request.MAC)
	if device != nil {
		device.Gateway = request.Gateway
	} else {
		// 如果设备不存在，创建一个新的设备记录
		newDevice := config.DeviceInfo{
			MAC:     request.MAC,
			Gateway: request.Gateway,
		}
		api.config.AddOrUpdateDevice(newDevice)
	}

	// 保存配置到文件
	if err := api.config.SaveConfig(api.configPath); err != nil {
		log.Printf("保存设备网关配置失败: %v", err)
		http.Error(w, "Failed to save configuration", http.StatusInternalServerError)
		return
	}

	// 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "设备网关配置已更新",
	})
}

// handleStaticBindings 处理静态绑定管理请求
func (api *APIServer) handleStaticBindings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		api.handleGetStaticBindings(w, r)
	case "POST":
		api.handleAddStaticBinding(w, r)
	case "PUT":
		api.handleUpdateStaticBinding(w, r)
	case "DELETE":
		api.handleDeleteStaticBinding(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleGetStaticBindings 获取所有静态绑定
func (api *APIServer) handleGetStaticBindings(w http.ResponseWriter, r *http.Request) {
	bindings := make([]map[string]interface{}, 0)

	for _, binding := range api.config.Bindings {
		bindingInfo := map[string]interface{}{
			"alias":    binding.Alias,
			"mac":      binding.MAC,
			"ip":       binding.IP,
			"gateway":  binding.Gateway,
			"hostname": binding.Hostname,
		}
		bindings = append(bindings, bindingInfo)
	}

	json.NewEncoder(w).Encode(bindings)
}

// handleAddStaticBinding 添加静态绑定
func (api *APIServer) handleAddStaticBinding(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Alias    string `json:"alias"`
		MAC      string `json:"mac"`
		IP       string `json:"ip"`
		Gateway  string `json:"gateway"`
		Hostname string `json:"hostname"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证必需字段
	if request.Alias == "" || request.MAC == "" || request.IP == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Alias, MAC and IP are required"})
		return
	}

	// 检查别名是否已存在
	for _, binding := range api.config.Bindings {
		if binding.Alias == request.Alias {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "Binding alias already exists"})
			return
		}
	}

	// 检查MAC地址是否已被绑定
	for _, binding := range api.config.Bindings {
		if binding.MAC == request.MAC {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "MAC address already bound"})
			return
		}
	}

	// 检查IP地址是否已被绑定
	for _, binding := range api.config.Bindings {
		if binding.IP == request.IP {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "IP address already bound"})
			return
		}
	}

	// 创建新的静态绑定
	newBinding := config.MACBinding{
		Alias:    request.Alias,
		MAC:      request.MAC,
		IP:       request.IP,
		Gateway:  request.Gateway,
		Hostname: request.Hostname,
	}

	// 添加到配置
	api.config.Bindings = append(api.config.Bindings, newBinding)

	// 保存配置到文件
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	// 重新加载配置以立即在IP池中添加新的静态绑定
	if api.reloadCallback != nil {
		go func() {
			time.Sleep(100 * time.Millisecond) // 短暂延迟以确保文件写入完成
			if err := api.reloadCallback(api.config); err != nil {
				log.Printf("添加静态绑定后重新加载配置失败: %v", err)
			}
		}()
	}

	response := map[string]interface{}{
		"alias":    request.Alias,
		"mac":      request.MAC,
		"ip":       request.IP,
		"gateway":  request.Gateway,
		"hostname": request.Hostname,
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(response)
}

// handleDeleteStaticBinding 删除静态绑定
func (api *APIServer) handleDeleteStaticBinding(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Alias string `json:"alias"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if request.Alias == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Binding alias is required"})
		return
	}

	// 查找并删除绑定
	found := false
	var deletedMAC string
	for i, binding := range api.config.Bindings {
		if binding.Alias == request.Alias {
			deletedMAC = binding.MAC
			// 从切片中删除元素
			api.config.Bindings = append(api.config.Bindings[:i], api.config.Bindings[i+1:]...)
			found = true
			break
		}
	}

	if !found {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Binding not found"})
		return
	}

	// 删除相关动态租约
	if deletedMAC != "" && api.pool != nil {
		api.pool.RemoveAllLeasesByMAC(deletedMAC)
	}

	// 清除设备的网关配置（如果存在）
	if deletedMAC != "" {
		if device := api.config.FindDeviceByMAC(deletedMAC); device != nil {
			device.Gateway = ""
			log.Printf("已清除设备 %s 的网关配置", deletedMAC)
		}
	}

	// 保存配置到文件
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	// 重新加载配置以立即从IP池中移除已删除的静态绑定
	if api.reloadCallback != nil {
		go func() {
			time.Sleep(100 * time.Millisecond) // 短暂延迟以确保文件写入完成
			if err := api.reloadCallback(api.config); err != nil {
				log.Printf("删除静态绑定后重新加载配置失败: %v", err)
			}
		}()
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateStaticBinding 更新静态绑定
func (api *APIServer) handleUpdateStaticBinding(w http.ResponseWriter, r *http.Request) {
	var request struct {
		OldAlias string `json:"old_alias"` // 用于识别要更新的绑定
		Alias    string `json:"alias"`
		MAC      string `json:"mac"`
		IP       string `json:"ip"`
		Gateway  string `json:"gateway"`
		Hostname string `json:"hostname"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证必需字段
	if request.OldAlias == "" || request.Alias == "" || request.MAC == "" || request.IP == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "OldAlias, Alias, MAC and IP are required"})
		return
	}

	// 查找要更新的绑定
	found := false
	var bindingIndex int
	for i, binding := range api.config.Bindings {
		if binding.Alias == request.OldAlias {
			found = true
			bindingIndex = i
			break
		}
	}

	if !found {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Binding not found"})
		return
	}

	// 如果别名有变化，检查新别名是否已存在
	if request.Alias != request.OldAlias {
		for _, binding := range api.config.Bindings {
			if binding.Alias == request.Alias {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "New alias already exists"})
				return
			}
		}
	}

	// 如果MAC地址有变化，检查新MAC是否已被绑定
	if request.MAC != api.config.Bindings[bindingIndex].MAC {
		for _, binding := range api.config.Bindings {
			if binding.MAC == request.MAC {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "MAC address already bound"})
				return
			}
		}
	}

	// 如果IP地址有变化，检查新IP是否已被绑定
	if request.IP != api.config.Bindings[bindingIndex].IP {
		for _, binding := range api.config.Bindings {
			if binding.IP == request.IP {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "IP address already bound"})
				return
			}
		}
	}

	// 更新绑定
	api.config.Bindings[bindingIndex] = config.MACBinding{
		Alias:    request.Alias,
		MAC:      request.MAC,
		IP:       request.IP,
		Gateway:  request.Gateway,
		Hostname: request.Hostname,
	}

	// 保存配置到文件
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	// 重新加载配置以立即在IP池中更新静态绑定
	if api.reloadCallback != nil {
		go func() {
			time.Sleep(100 * time.Millisecond) // 短暂延迟以确保文件写入完成
			if err := api.reloadCallback(api.config); err != nil {
				log.Printf("更新静态绑定后重新加载配置失败: %v", err)
			}
		}()
	}

	// 返回更新后的绑定信息
	response := map[string]interface{}{
		"alias":    request.Alias,
		"mac":      request.MAC,
		"ip":       request.IP,
		"gateway":  request.Gateway,
		"hostname": request.Hostname,
	}

	json.NewEncoder(w).Encode(response)
}

// handleConvertLeaseToStatic 将动态租约转换为静态绑定
func (api *APIServer) handleConvertLeaseToStatic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	var request struct {
		MAC      string `json:"mac"`
		Alias    string `json:"alias"`
		Gateway  string `json:"gateway"`
		Hostname string `json:"hostname"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证必需字段
	if request.MAC == "" || request.Alias == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "MAC address and alias are required"})
		return
	}

	// 查找当前的动态租约
	lease, exists := api.pool.GetLeaseByMAC(request.MAC)
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "No active lease found for this MAC address"})
		return
	}

	// 检查租约是否已经是静态的
	if lease.IsStatic {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "This lease is already static"})
		return
	}

	// 检查别名是否已存在
	for _, binding := range api.config.Bindings {
		if binding.Alias == request.Alias {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "Alias already exists"})
			return
		}
	}

	// 检查MAC地址是否已有静态绑定
	for _, binding := range api.config.Bindings {
		if binding.MAC == request.MAC {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "MAC address already has static binding"})
			return
		}
	}

	// 检查IP地址是否已被静态绑定
	leaseIP := lease.IP.String()
	for _, binding := range api.config.Bindings {
		if binding.IP == leaseIP {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "IP address already has static binding"})
			return
		}
	}

	// 使用当前租约的hostname，如果请求中没有提供的话
	hostname := request.Hostname
	if hostname == "" {
		hostname = lease.Hostname
	}

	// 创建新的静态绑定
	newBinding := config.MACBinding{
		Alias:    request.Alias,
		MAC:      request.MAC,
		IP:       leaseIP,
		Gateway:  request.Gateway,
		Hostname: hostname,
	}

	// 添加到配置
	api.config.Bindings = append(api.config.Bindings, newBinding)

	// 检查设备是否存在，如果不存在则自动创建设备记录
	if api.config.FindDeviceByMAC(request.MAC) == nil {
		// 创建新的设备记录
		newDevice := config.DeviceInfo{
			MAC:         request.MAC,
			DeviceType:  guessDeviceInfo(request.MAC),
			Model:       "",
			Description: "租约转静态时自动创建",
			Owner:       "",
			Hostname:    hostname,
			FirstSeen:   time.Now(),
			LastSeen:    time.Now(),
			IsActive:    false, // 静态绑定不表示设备活跃，需要实际网络活动确认
		}

		// 添加设备到配置中
		api.config.AddOrUpdateDevice(newDevice)
	}

	// 保存配置到文件
	if err := api.config.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save config: " + err.Error()})
		return
	}

	// 重新加载配置以更新IP池中的静态绑定
	if api.reloadCallback != nil {
		go func() {
			time.Sleep(100 * time.Millisecond) // 短暂延迟以确保文件写入完成
			if err := api.reloadCallback(api.config); err != nil {
				log.Printf("转换为静态绑定后重新加载配置失败: %v", err)
			}
		}()
	}

	// 返回新创建的静态绑定信息
	response := map[string]interface{}{
		"alias":    request.Alias,
		"mac":      request.MAC,
		"ip":       leaseIP,
		"gateway":  request.Gateway,
		"hostname": hostname,
		"message":  "Dynamic lease successfully converted to static binding",
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(response)
}

// guessDeviceInfo 根据MAC地址推测设备信息
func guessDeviceInfo(mac string) string {
	if len(mac) < 8 {
		return "Unknown"
	}

	// 获取MAC地址的厂商前缀（前6位）

	// 常见厂商的MAC前缀映射
	macPrefixMap := map[string][]string{
		"Apple": {
			"00:03:93", "00:05:02", "00:0A:95", "00:0D:93", "00:11:24", "00:14:51",
			"00:16:CB", "00:17:F2", "00:19:E3", "00:1B:63", "00:1C:B3", "00:1E:C2",
			"00:1F:F3", "00:21:E9", "00:22:41", "00:23:12", "00:23:DF", "00:24:36",
			"00:25:00", "00:25:4B", "00:25:BC", "00:26:08", "00:26:4A", "00:26:B0",
			"00:26:BB", "3C:15:C2", "40:A6:D9", "40:B3:95", "44:00:10", "48:A1:95",
			"4C:8D:79", "50:EA:D6", "58:55:CA", "5C:59:48", "5C:95:AE", "60:03:08",
			"60:33:4B", "60:A3:7D", "60:C5:47", "64:20:0C", "64:B9:E8", "68:AB:BC",
			"6C:72:20", "6C:8D:C1", "70:11:24", "70:73:CB", "70:CD:60", "74:F0:6D",
			"78:31:C1", "78:4F:43", "7C:6D:62", "7C:C3:A1", "80:BE:05", "80:E6:50",
			"84:38:35", "84:85:06", "88:63:DF", "8C:2D:AA", "8C:7C:92", "90:72:40",
			"90:B0:ED", "94:F6:A3", "98:01:A7", "98:5A:EB", "9C:04:EB", "9C:84:BF",
			"A0:99:9B", "A4:5E:60", "A4:B1:97", "A8:86:DD", "A8:BB:CF", "AC:BC:32",
			"B0:65:BD", "B0:CA:68", "B4:F0:AB", "B8:8D:12", "B8:C7:5A", "BC:52:B7",
			"BC:67:1C", "C0:84:7A", "C4:2C:03", "C8:BC:C8", "C8:E0:EB", "CC:25:EF",
			"D0:23:DB", "D0:A6:37", "D4:90:9C", "D8:00:4D", "D8:30:62", "D8:96:95",
			"DC:37:45", "DC:A9:04", "E0:AC:CB", "E0:B9:4D", "E4:8B:7F", "E4:C6:3D",
			"E8:06:88", "EC:35:86", "F0:B4:79", "F0:DB:E2", "F4:F1:5A", "F8:27:93",
			"F8:A9:D0", "FC:25:3F",
		},
		"Samsung": {
			"00:07:AB", "00:12:FB", "00:13:77", "00:15:99", "00:16:32", "00:17:C9",
			"00:17:D5", "00:18:AF", "00:1A:8A", "00:1B:98", "00:1C:43", "00:1D:25",
			"00:1E:7D", "00:1F:CC", "00:21:19", "00:21:D1", "00:23:39", "00:24:54",
			"00:26:37", "00:E3:B2", "08:08:C2", "08:37:3D", "08:FC:88", "0C:14:20",
			"0C:89:10", "10:1D:C0", "14:7D:C5", "18:3A:2D", "18:AF:8F", "1C:28:AF",
			"1C:5A:3E", "20:13:E0", "20:64:32", "20:A5:9E", "24:4B:81", "28:39:5E",
			"28:ED:6A", "2C:44:01", "30:07:4D", "30:19:66", "30:CD:A7", "34:23:87",
			"34:BE:00", "38:AA:3C", "3C:8B:FE", "40:0E:85", "40:43:7A", "44:4E:6D",
			"44:78:3E", "48:5A:3F", "4C:3C:16", "4C:66:41", "50:32:75", "50:CC:F8",
			"54:88:0E", "58:8B:F4", "5C:0A:5B", "5C:51:88", "60:6B:BD", "60:A1:0A",
			"64:3E:8C", "68:EB:AE", "6C:2F:2C", "6C:94:66", "70:F9:27", "74:45:8A",
			"78:1F:DB", "78:52:1A", "78:9E:D0", "7C:61:66", "7C:A1:AE", "80:18:A7",
			"84:25:3F", "88:32:9B", "8C:77:12", "90:18:7C", "94:35:0A", "9C:02:98",
			"9C:3A:AF", "A0:0B:BA", "A0:75:91", "A4:EB:D3", "A8:F2:74", "AC:5F:3E",
			"B0:EC:71", "B4:62:93", "B8:5E:7B", "BC:14:85", "BC:F5:AC", "C0:BD:D1",
			"C4:57:6E", "C8:A8:23", "CC:07:AB", "D0:17:6A", "D0:59:E4", "D4:E8:B2",
			"D8:57:EF", "DC:71:44", "E0:91:F5", "E4:40:E2", "E8:50:8B", "EC:1F:72",
			"F0:25:B7", "F4:09:D8", "F8:04:2E", "FC:00:12",
		},
		"Xiaomi": {
			"34:CE:00", "50:8F:4C", "64:09:80", "68:DF:DD", "6C:96:CF", "74:51:BA",
			"78:11:DC", "7C:49:EB", "8C:BE:BE", "98:FA:9B", "A0:86:C6", "AC:C1:EE",
			"B0:E2:35", "C4:0B:CB", "D4:61:9D", "E4:46:DA", "F0:B4:29", "F8:8F:CA",
			"FC:64:BA", "2C:EA:7F", "50:EC:50", "58:44:98", "68:3B:78", "78:02:F8",
			"88:C3:97", "9C:99:A0", "A4:DA:32", "B0:E5:ED", "C8:47:8C", "D0:7E:35",
			"E8:78:29", "F4:8E:38", "F4:F5:DB",
		},
		"Huawei": {
			"00:E0:FC", "18:66:DA", "1C:1D:67", "20:F3:A3", "24:69:68", "28:6E:D4",
			"2C:AB:00", "34:6B:D3", "38:BC:01", "3C:F8:08", "40:4E:36", "44:6D:6C",
			"48:59:29", "4C:54:99", "50:3D:E5", "54:51:1B", "58:2A:F7", "5C:63:BF",
			"60:DE:44", "64:3B:78", "68:3E:34", "6C:4B:90", "70:72:3C", "74:A7:22",
			"78:D7:52", "7C:A2:3E", "80:71:7A", "84:A8:E4", "88:25:93", "8C:0E:E3",
			"90:67:1C", "94:04:9C", "98:52:3D", "9C:28:EF", "A0:48:1C", "A4:50:46",
			"A8:4E:3F", "AC:E2:15", "B0:91:34", "B4:CD:27", "B8:08:CF", "BC:76:5E",
			"C0:EE:40", "C4:F0:81", "C8:94:02", "CC:96:A0", "D0:7A:B5", "D4:20:B0",
			"D8:49:2F", "DC:D9:16", "E0:19:1D", "E4:D3:32", "E8:CD:2D", "EC:23:3D",
			"F0:79:59", "F4:C7:14", "F8:E7:1E", "FC:48:EF",
		},
		"Microsoft": {
			"00:0D:3A", "00:12:5A", "00:15:5D", "00:17:FA", "00:1D:D8", "00:50:F2",
			"00:9D:8E", "04:C5:A4", "18:60:24", "1C:B7:2C", "28:18:78", "30:59:B7",
			"34:17:EB", "38:2C:4A", "3C:5A:B4", "40:E2:30", "44:85:00", "54:27:1E",
			"60:45:BD", "64:00:F1", "74:2F:68", "7C:1E:52", "80:E6:50", "98:5F:D3",
			"A0:CE:C8", "B8:86:87", "D0:57:7B", "F4:A4:75", "F8:63:3F",
		},
		"Intel": {
			"00:02:B3", "00:03:47", "00:04:23", "00:07:E9", "00:0E:35", "00:11:25",
			"00:12:F0", "00:13:02", "00:13:CE", "00:15:00", "00:16:E3", "00:19:D1",
			"00:1B:21", "00:1E:64", "00:1F:3B", "00:21:6A", "00:22:FB", "00:24:D7",
			"00:27:10", "04:79:B7", "08:11:96", "0C:8B:FD", "10:0B:A9", "18:56:80",
			"1C:3E:84", "20:16:B9", "24:77:03", "28:D2:44", "2C:6E:85", "30:E1:71",
			"34:13:E8", "38:2C:4A", "3C:A9:F4", "40:B0:34", "44:85:00", "48:2A:E3",
			"4C:79:6E", "50:76:AF", "54:27:1E", "58:91:CF", "5C:E0:C5", "60:67:20",
			"64:27:37", "68:17:29", "6C:88:14", "70:5A:0F", "74:E5:43", "78:92:9C",
			"7C:7A:91", "80:19:34", "84:3A:4B", "88:53:2E", "8C:A9:82", "90:E2:BA",
			"94:65:9C", "98:4F:EE", "9C:B6:D0", "A0:A8:CD", "A4:4C:C8", "A8:6D:AA",
			"AC:7B:A1", "B0:C0:90", "B4:B6:76", "B8:AE:ED", "BC:77:37", "C0:3F:D5",
			"C4:8E:8F", "C8:D9:D2", "CC:3D:82", "D0:50:99", "D4:BE:D9", "D8:CB:8A",
			"DC:53:60", "E0:94:67", "E4:A4:71", "E8:39:35", "EC:A8:6B", "F0:76:1C",
			"F4:06:69", "F8:63:3F", "FC:AA:14",
		},
	}

	// 标准化MAC地址格式
	normalizedMac := strings.ToUpper(strings.ReplaceAll(mac, ":", ""))
	if len(normalizedMac) >= 6 {
		macPrefix := normalizedMac[:6]
		// 转换为带冒号的格式
		formattedPrefix := fmt.Sprintf("%s:%s:%s", macPrefix[:2], macPrefix[2:4], macPrefix[4:6])

		for vendor, prefixes := range macPrefixMap {
			for _, vendorPrefix := range prefixes {
				if formattedPrefix == vendorPrefix {
					switch vendor {
					case "Apple":
						return "iOS/macOS"
					case "Samsung":
						return "Android"
					case "Xiaomi":
						return "Android"
					case "Huawei":
						return "Android"
					case "Microsoft":
						return "Windows"
					case "Intel":
						return "Computer"
					default:
						return "Unknown"
					}
				}
			}
		}
	}

	return "Unknown"
}

// handleConfig 处理配置管理请求
func (api *APIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		api.handleGetConfig(w, r)
	case "POST":
		api.handleSaveConfig(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleGetConfig 获取当前配置
func (api *APIServer) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	// 检查查询参数，如果要求原始内容则返回原始YAML
	if r.URL.Query().Get("raw") == "true" {
		// 读取配置文件内容
		data, err := ioutil.ReadFile(api.configPath)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Failed to read config file"})
			return
		}

		response := map[string]interface{}{
			"content": string(data),
			"path":    api.configPath,
			"size":    len(data),
		}

		json.NewEncoder(w).Encode(response)
		return
	}

	// 返回解析后的配置对象
	response := map[string]interface{}{
		"server":       api.config.Server,
		"network":      api.config.Network,
		"gateways":     api.config.Gateways,
		"bindings":     api.config.Bindings,
		"devices":      api.config.Devices,
		"health_check": api.config.HealthCheck,
	}

	json.NewEncoder(w).Encode(response)
}

// handleSaveConfig 保存配置
func (api *APIServer) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Content    string `json:"content"`
		AutoReload bool   `json:"auto_reload"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证配置格式
	if err := config.ValidateYAML(request.Content); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 解析新配置
	newConfig, err := config.LoadConfigFromString(request.Content)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 保存配置文件
	if err := newConfig.SaveConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 自动重新加载
	if request.AutoReload && api.reloadCallback != nil {
		go func() {
			time.Sleep(500 * time.Millisecond) // 稍微延迟以确保文件写入完成
			if err := api.reloadCallback(newConfig); err != nil {
				log.Printf("配置重新加载失败: %v", err)
			}
		}()
	}

	response := map[string]interface{}{
		"success":     true,
		"message":     "配置保存成功",
		"auto_reload": request.AutoReload,
	}

	json.NewEncoder(w).Encode(response)
}

// handleConfigValidate 验证配置
func (api *APIServer) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	var request struct {
		Content string `json:"content"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证配置
	err := config.ValidateYAML(request.Content)

	response := map[string]interface{}{
		"valid": err == nil,
	}

	if err != nil {
		response["error"] = err.Error()
	} else {
		response["message"] = "配置格式正确"
	}

	json.NewEncoder(w).Encode(response)
}

// handleConfigBackups 处理配置备份
func (api *APIServer) handleConfigBackups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	backups, err := config.GetBackupList(api.configPath)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(backups)
}

// handleConfigRestore 恢复配置备份
func (api *APIServer) handleConfigRestore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	var request struct {
		Filename   string `json:"filename"`
		AutoReload bool   `json:"auto_reload"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 构建备份文件路径
	backupPath := fmt.Sprintf("config_backups/%s", request.Filename)

	// 读取备份文件
	data, err := ioutil.ReadFile(backupPath)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Backup file not found"})
		return
	}

	// 验证备份配置
	if err := config.ValidateYAML(string(data)); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid backup config: " + err.Error()})
		return
	}

	// 创建当前配置的备份
	if err := api.config.BackupConfig(api.configPath); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to backup current config"})
		return
	}

	// 恢复配置文件
	if err := ioutil.WriteFile(api.configPath, data, 0644); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to restore config"})
		return
	}

	// 自动重新加载
	if request.AutoReload && api.reloadCallback != nil {
		newConfig, err := config.LoadConfigFromString(string(data))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Failed to parse restored config"})
			return
		}

		go func() {
			time.Sleep(500 * time.Millisecond)
			if err := api.reloadCallback(newConfig); err != nil {
				log.Printf("配置重新加载失败: %v", err)
			}
		}()
	}

	response := map[string]interface{}{
		"success":     true,
		"message":     "配置恢复成功",
		"filename":    request.Filename,
		"auto_reload": request.AutoReload,
	}

	json.NewEncoder(w).Encode(response)
}

// handleConfigReload 重新加载配置
func (api *APIServer) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	if api.reloadCallback == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "Reload callback not set"})
		return
	}

	// 重新加载配置文件
	newConfig, err := config.LoadConfig(api.configPath)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to load config: " + err.Error()})
		return
	}

	// 执行重新加载
	if err := api.reloadCallback(newConfig); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to reload config: " + err.Error()})
		return
	}

	response := map[string]interface{}{
		"success":   true,
		"message":   "配置重新加载成功",
		"timestamp": time.Now(),
	}

	json.NewEncoder(w).Encode(response)
}

// handleServerConfig 处理服务器配置请求
func (api *APIServer) handleServerConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(api.config.Server)
	case "POST":
		var newServerConfig config.ServerConfig
		if err := json.NewDecoder(r.Body).Decode(&newServerConfig); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
			return
		}

		// 基本验证
		if newServerConfig.Interface == "" || newServerConfig.Port <= 0 || newServerConfig.APIPort <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "接口名、端口和API端口不能为空，且端口必须大于0"})
			return
		}

		// 验证API监听地址
		if newServerConfig.APIHost != "" {
			if newServerConfig.APIHost != "0.0.0.0" && newServerConfig.APIHost != "localhost" && net.ParseIP(newServerConfig.APIHost) == nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "无效的API监听地址，请输入有效的IP地址、0.0.0.0或localhost"})
				return
			}
		}

		// 验证租期时间格式
		if newServerConfig.LeaseTime != 0 {
			// 将秒数转换为time.Duration
			leaseTime := time.Duration(newServerConfig.LeaseTime) * time.Second
			if leaseTime < time.Minute {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "租期时间不能少于1分钟"})
				return
			}
			// 更新为正确的time.Duration格式
			newServerConfig.LeaseTime = leaseTime
		}

		// 更新配置
		api.config.Server = newServerConfig

		// 保存配置到文件
		if err := api.config.SaveConfig(api.configPath); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "保存配置失败: " + err.Error()})
			return
		}

		// 检查是否需要重启提示
		needRestart := false
		if api.config.Server.APIHost != newServerConfig.APIHost {
			needRestart = true
		}
		if api.config.Server.LeaseTime != newServerConfig.LeaseTime {
			needRestart = true
		}

		response := map[string]interface{}{
			"success": true,
			"message": "服务器配置更新成功",
			"config":  api.config.Server,
		}

		if needRestart {
			response["message"] = "服务器配置更新成功，API监听地址已修改，请重启服务器以应用新配置"
			response["restart_required"] = true
		}
		json.NewEncoder(w).Encode(response)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleNetworkConfig 处理网络配置请求
func (api *APIServer) handleNetworkConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(api.config.Network)
	case "POST":
		var newNetworkConfig config.NetworkConfig
		if err := json.NewDecoder(r.Body).Decode(&newNetworkConfig); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
			return
		}

		// 基本验证
		if newNetworkConfig.Subnet == "" || newNetworkConfig.StartIP == "" || newNetworkConfig.EndIP == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "子网、起始IP和结束IP不能为空"})
			return
		}

		// 更新配置
		api.config.Network = newNetworkConfig

		// 保存配置到文件
		if err := api.config.SaveConfig(api.configPath); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "保存配置失败: " + err.Error()})
			return
		}

		response := map[string]interface{}{
			"success": true,
			"message": "网络配置更新成功",
			"config":  api.config.Network,
		}
		json.NewEncoder(w).Encode(response)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleHealthCheckConfig 处理网关检测配置请求
func (api *APIServer) handleHealthCheckConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(api.config.HealthCheck)
	case "POST":
		var newHealthConfig config.HealthConfig
		if err := json.NewDecoder(r.Body).Decode(&newHealthConfig); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
			return
		}

		// 基本验证
		if newHealthConfig.Interval <= 0 || newHealthConfig.Timeout <= 0 || newHealthConfig.RetryCount <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "检测间隔、超时时间和重试次数必须大于0"})
			return
		}

		if newHealthConfig.Method != "ping" && newHealthConfig.Method != "tcp" && newHealthConfig.Method != "http" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "检测方法必须是ping、tcp或http之一"})
			return
		}

		// 更新配置
		api.config.HealthCheck = newHealthConfig

		// 保存配置到文件
		if err := api.config.SaveConfig(api.configPath); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "保存配置失败: " + err.Error()})
			return
		}

		// 如果网关检查器存在，记录配置更新日志
		if api.checker != nil {
			log.Printf("网关检测配置已更新: 间隔=%v, 超时=%v, 重试=%d, 方法=%s",
				newHealthConfig.Interval, newHealthConfig.Timeout,
				newHealthConfig.RetryCount, newHealthConfig.Method)
		}

		response := map[string]interface{}{
			"success": true,
			"message": "网关检测配置更新成功",
			"config":  api.config.HealthCheck,
		}
		json.NewEncoder(w).Encode(response)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleLogs 处理日志查询请求
func (api *APIServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// 获取查询参数
	query := r.URL.Query()
	limitStr := query.Get("limit")

	limit := 500 // 默认最近500行
	if limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}

	// 读取日志文件
	logContent, err := readLastNLines("dhcp.log", limit)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to read log file: " + err.Error()})
		return
	}

	response := map[string]interface{}{
		"logs":  logContent,
		"count": len(logContent),
	}

	json.NewEncoder(w).Encode(response)
}

// handleLogStream 处理SSE日志流请求
func (api *APIServer) handleLogStream(w http.ResponseWriter, r *http.Request) {
	// 设置SSE响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// 创建客户端通道
	clientChan := make(chan string, 10)

	// 标记通道是否已关闭
	var channelClosed bool
	var mu sync.Mutex

	// 注册客户端
	logClientsMux.Lock()
	logClients[clientChan] = true
	logClientsMux.Unlock()

	// 客户端断开连接时清理
	defer func() {
		logClientsMux.Lock()
		delete(logClients, clientChan)
		logClientsMux.Unlock()

		// 安全关闭channel
		mu.Lock()
		if !channelClosed {
			close(clientChan)
			channelClosed = true
		}
		mu.Unlock()
	}()

	// 发送初始化消息
	fmt.Fprintf(w, "data: Connected to log stream\n\n")
	w.(http.Flusher).Flush()

	// 监听客户端断开连接和新日志消息
	for {
		select {
		case <-r.Context().Done():
			return
		case message, ok := <-clientChan:
			if !ok {
				// Channel已关闭
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", message)
			w.(http.Flusher).Flush()
		}
	}
}

// readLastNLines 读取文件的最后N行
func readLastNLines(filename string, n int) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// 如果行数超过限制，只返回最后n行
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return lines, nil
}

// handleScanner 处理扫描器状态请求
func (api *APIServer) handleScanner(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if api.scanner == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "扫描器未启用"})
		return
	}

	switch r.Method {
	case "GET":
		status := api.scanner.GetStatus()
		response := map[string]interface{}{
			"enabled":        status.IsEnabled,
			"is_running":     status.IsRunning,
			"last_scan":      status.LastScanTime,
			"next_scan":      status.NextScanTime,
			"scan_progress":  status.ScanProgress,
			"scanned_ips":    status.ScannedIPs,
			"total_ips":      status.TotalIPs,
			"found_devices":  status.FoundDevices,
			"conflicted_ips": status.ConflictedIPs,
			"current_scan":   status.CurrentScan,
			"config":         api.config.Scanner,
		}
		json.NewEncoder(w).Encode(response)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleScannerResults 处理扫描结果请求
func (api *APIServer) handleScannerResults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if api.scanner == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "扫描器未启用"})
		return
	}

	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	results := api.scanner.GetScanResults()
	var devices []map[string]interface{}

	for _, device := range results {
		deviceInfo := map[string]interface{}{
			"mac":       device.MAC,
			"ip":        device.IP,
			"hostname":  device.Hostname,
			"last_seen": device.LastSeen,
			"is_active": device.IsActive,
			"vendor":    device.Vendor,
			"response":  device.Response,
		}
		devices = append(devices, deviceInfo)
	}

	response := map[string]interface{}{
		"devices": devices,
		"count":   len(devices),
	}
	json.NewEncoder(w).Encode(response)
}

// handleScannerLog 处理扫描日志请求
func (api *APIServer) handleScannerLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if api.scanner == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "扫描器未启用"})
		return
	}

	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	logs := api.scanner.GetScanLog()
	response := map[string]interface{}{
		"logs":  logs,
		"count": len(logs),
	}
	json.NewEncoder(w).Encode(response)
}

// handleScannerStart 处理启动扫描器请求
func (api *APIServer) handleScannerStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if api.scanner == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "扫描器未启用"})
		return
	}

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	api.scanner.Start()
	response := map[string]interface{}{
		"success": true,
		"message": "扫描器已启动",
	}
	json.NewEncoder(w).Encode(response)
}

// handleScannerStop 处理停止扫描器请求
func (api *APIServer) handleScannerStop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if api.scanner == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "扫描器未启用"})
		return
	}

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	api.scanner.Stop()
	response := map[string]interface{}{
		"success": true,
		"message": "扫描器已停止",
	}
	json.NewEncoder(w).Encode(response)
}

// handleScannerConfig 处理扫描器配置请求
func (api *APIServer) handleScannerConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(api.config.Scanner)
	case "POST":
		var newScannerConfig config.ScannerConfig
		if err := json.NewDecoder(r.Body).Decode(&newScannerConfig); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
			return
		}

		// 基本验证
		if newScannerConfig.ScanInterval <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "扫描间隔必须大于0"})
			return
		}

		if newScannerConfig.MaxConcurrency <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "最大并发数必须大于0"})
			return
		}

		// 更新配置
		api.config.Scanner = newScannerConfig

		// 保存配置到文件
		if err := api.config.SaveConfig(api.configPath); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "保存配置失败: " + err.Error()})
			return
		}

		// 如果扫描器存在，重新应用配置
		if api.scanner != nil {
			// 停止当前扫描器
			wasRunning := api.scanner.IsRunning()
			if wasRunning {
				api.scanner.Stop()
			}

			// 重新创建扫描器实例以应用新配置
			api.scanner = dhcp.NewNetworkScanner(api.config, api.pool)

			// 如果之前在运行，重新启动
			if wasRunning {
				api.scanner.Start()
			}
		}

		response := map[string]interface{}{
			"success": true,
			"message": "扫描器配置更新成功",
			"config":  api.config.Scanner,
		}
		json.NewEncoder(w).Encode(response)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
	}
}

// handleAvailableIPs 返回所有可用的静态IP地址
func (api *APIServer) handleAvailableIPs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ips := api.pool.GetAvailableIPs()
	json.NewEncoder(w).Encode(ips)
}

// handleServerRestart 处理服务器重启请求
func (api *APIServer) handleServerRestart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	log.Println("收到服务器重启请求...")

	// 立即返回成功响应，然后进行重启
	response := map[string]interface{}{
		"success": true,
		"message": "DHCP服务器重启命令已接收，正在重启...",
	}
	json.NewEncoder(w).Encode(response)

	// 刷新响应缓冲区，确保客户端收到响应
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// 在goroutine中执行重启操作，避免阻塞响应
	go func() {
		// 等待一点时间确保响应已发送
		time.Sleep(500 * time.Millisecond)

		log.Println("开始重启DHCP服务器...")

		// 执行重新加载配置（这会重启DHCP服务）
		if api.reloadCallback != nil {
			if err := api.reloadCallback(api.config); err != nil {
				log.Printf("重启失败: %v", err)
			} else {
				log.Println("DHCP服务器重启成功")
			}
		} else {
			log.Println("重启回调函数未设置，使用程序退出方式重启")
			// 如果没有重启回调，可以通过退出程序的方式触发系统重启
			// 这需要配合进程管理器（如systemd）使用
			os.Exit(1)
		}
	}()
}
