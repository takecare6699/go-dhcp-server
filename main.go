package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"dhcp-server/api"
	"dhcp-server/config"
	"dhcp-server/dhcp"
)

var (
	configPath = flag.String("config", "config.yaml", "配置文件路径")
	version    = flag.Bool("version", false, "显示版本信息")
	help       = flag.Bool("help", false, "显示帮助信息")

	// 全局变量，用于热重载
	globalConfig *config.Config
	globalServer *dhcp.Server
	globalAPI    *api.APIServer
	serverMutex  sync.RWMutex
	isRestarting bool
	restartMutex sync.Mutex
)

const (
	Version = "1.0.0"
	AppName = "DHCP Server"
)

func main() {
	flag.Parse()

	if *version {
		printVersion()
		return
	}

	if *help {
		printHelp()
		return
	}

	// 设置日志同时输出到控制台和文件
	logFile, err := os.OpenFile("dhcp.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("无法打开日志文件: %v", err)
	}
	defer logFile.Close()

	// 创建多重写入器，同时写入控制台和文件
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)

	log.Println("启动 DHCP Server v1.0.0")

	// 加载配置文件
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("配置文件加载失败: %v", err)
	}
	globalConfig = cfg
	log.Printf("配置文件加载成功: %s", *configPath)

	// 初始化DHCP服务器
	if err := initializeDHCPServer(cfg); err != nil {
		log.Fatalf("DHCP服务器初始化失败: %v", err)
	}

	// 创建HTTP API服务器
	globalAPI = api.NewAPIServer(
		globalServer.GetPool(),
		globalServer.GetChecker(),
		globalConfig,
		*configPath,
		globalServer,
		globalConfig.Server.APIPort,
	)

	// 设置配置重新加载回调
	globalAPI.SetReloadCallback(reloadConfig)

	// 启动日志监控器
	go startLogMonitor("dhcp.log")

	// 启动HTTP API服务器
	go func() {
		if err := globalAPI.Start(); err != nil {
			log.Printf("HTTP API服务器错误: %v", err)
		}
	}()

	// 启动DHCP服务器
	go func() {
		if err := globalServer.Start(); err != nil {
			log.Printf("DHCP服务器错误: %v", err)
		}
	}()

	// 等待中断信号
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	log.Printf("服务器启动完成，按 Ctrl+C 停止")
	<-c
	log.Println("收到信号 terminated，正在关闭服务器...")

	// 优雅关闭HTTP API服务器
	if err := globalAPI.Stop(); err != nil {
		log.Printf("HTTP API服务器停止错误: %v", err)
	}

	// 停止DHCP服务器
	globalServer.Stop()

	log.Println("服务器已成功关闭")
}

// initializeDHCPServer 初始化DHCP服务器
func initializeDHCPServer(cfg *config.Config) error {
	serverMutex.Lock()
	defer serverMutex.Unlock()

	// 创建DHCP服务器
	server, err := dhcp.NewServer(cfg)
	if err != nil {
		return err
	}
	globalServer = server

	// 打印配置摘要
	printConfigSummary(cfg)

	return nil
}

// reloadConfig 重新加载配置
func reloadConfig(newConfig *config.Config) error {
	restartMutex.Lock()
	defer restartMutex.Unlock()

	if isRestarting {
		return fmt.Errorf("配置重载正在进行中，请稍后再试")
	}

	isRestarting = true
	defer func() { isRestarting = false }()

	log.Println("开始重新加载配置...")

	// 保存旧的服务器引用
	oldServer := globalServer

	// 创建新的DHCP服务器
	newServer, err := dhcp.NewServer(newConfig)
	if err != nil {
		log.Printf("创建新DHCP服务器失败: %v", err)
		return err
	}

	// 迁移现有租约
	if err := migrateLeases(oldServer, newServer); err != nil {
		log.Printf("迁移租约失败: %v", err)
		return err
	}

	// 更新全局引用
	serverMutex.Lock()
	globalConfig = newConfig
	globalServer = newServer
	serverMutex.Unlock()

	// 重启服务
	if err := restartServices(oldServer, newServer); err != nil {
		log.Printf("重启服务失败: %v", err)
		return err
	}

	// 更新API服务器的引用
	if globalAPI != nil {
		globalAPI.UpdateReferences(newServer.GetPool(), newServer.GetChecker(), newConfig, newServer)
	}

	log.Println("配置重新加载成功")
	printConfigSummary(newConfig)

	return nil
}

// restartServices 重启服务
func restartServices(oldServer, newServer *dhcp.Server) error {
	// 停止旧服务器（但不关闭监听器，避免端口冲突）
	log.Println("正在停止旧的DHCP服务器...")
	oldServer.Stop()

	// 等待一小段时间确保旧服务器完全停止
	time.Sleep(500 * time.Millisecond)

	// 启动新服务器
	log.Println("正在启动新的DHCP服务器...")
	go func() {
		if err := newServer.Start(); err != nil {
			log.Printf("新DHCP服务器启动错误: %v", err)
		}
	}()

	return nil
}

// migrateLeases 迁移租约从旧服务器到新服务器
func migrateLeases(oldServer, newServer *dhcp.Server) error {
	if oldServer == nil || newServer == nil {
		return nil
	}

	log.Println("开始迁移租约...")

	// 获取所有活跃租约
	activeLeases := oldServer.GetActiveLeases()

	log.Printf("发现 %d 个活跃租约需要迁移", len(activeLeases))

	// 迁移每个租约
	migratedCount := 0
	for _, lease := range activeLeases {
		// 对于每个活跃租约，在新服务器中请求相同的IP
		newPool := newServer.GetPool()

		// 尝试在新池中分配相同的IP
		if _, err := newPool.RequestIP(lease.MAC, lease.IP, lease.Hostname); err != nil {
			log.Printf("迁移租约失败 %s -> %s: %v", lease.MAC, lease.IP, err)
		} else {
			migratedCount++
			log.Printf("成功迁移租约: %s -> %s", lease.MAC, lease.IP)
		}
	}

	log.Printf("租约迁移完成: %d/%d 成功", migratedCount, len(activeLeases))
	return nil
}

// printConfigSummary 打印配置摘要
func printConfigSummary(cfg *config.Config) {
	log.Println("=== 配置摘要 ===")
	log.Printf("监听接口: %s:%d", cfg.Server.Interface, cfg.Server.Port)
	log.Printf("HTTP API端口: %d", cfg.Server.APIPort)
	log.Printf("IP地址池: %s - %s", cfg.Network.StartIP, cfg.Network.EndIP)
	log.Printf("子网配置: %s/%s", cfg.Network.Subnet, cfg.Network.Netmask)
	log.Printf("DNS服务器: %v", cfg.Network.DNSServers)
	log.Printf("域名: %s", cfg.Network.DomainName)
	log.Printf("租期时间: %s", cfg.Server.LeaseTime)

	log.Printf("配置了 %d 个网关:", len(cfg.Gateways))
	for _, gw := range cfg.Gateways {
		defaultMarker := ""
		if gw.IsDefault {
			defaultMarker = " [默认]"
		}
		log.Printf("  - %s (%s)%s: %s", gw.Name, gw.IP, defaultMarker, gw.Description)
	}

	log.Printf("配置了 %d 个静态绑定:", len(cfg.Bindings))
	for _, binding := range cfg.Bindings {
		log.Printf("  - %s (%s) -> %s, 网关: %s", binding.Alias, binding.MAC, binding.IP, binding.Gateway)
	}

	log.Printf("配置了 %d 个设备:", len(cfg.Devices))
	for _, device := range cfg.Devices {
		log.Printf("  - %s (%s) [%s]: %s", device.MAC, device.MAC, device.DeviceType, device.Description)
	}

	log.Printf("健康检查: %s间隔, %s超时, %d重试, 方法: %s",
		cfg.HealthCheck.Interval, cfg.HealthCheck.Timeout, cfg.HealthCheck.RetryCount, cfg.HealthCheck.Method)

	log.Println("==================")
}

// printVersion 打印版本信息
func printVersion() {
	fmt.Println("DHCP Server v1.0.0")
	fmt.Println("Build with Go")
}

// printHelp 打印帮助信息
func printHelp() {
	fmt.Println("DHCP Server - 企业级DHCP服务器")
	fmt.Println()
	fmt.Println("用法:")
	fmt.Printf("  %s [选项]\n", os.Args[0])
	fmt.Println()
	fmt.Println("选项:")
	flag.PrintDefaults()
	fmt.Println()
	fmt.Println("示例:")
	fmt.Printf("  %s -config config.yaml\n", os.Args[0])
	fmt.Printf("  %s -version\n", os.Args[0])
}

// init 初始化函数
func init() {
	// 检查是否以root权限运行
	if os.Geteuid() != 0 {
		fmt.Println("警告: 建议以root权限运行以绑定特权端口(67)")
	}
}

// startLogMonitor 启动日志监控器
func startLogMonitor(filename string) {
	// 打开日志文件
	file, err := os.Open(filename)
	if err != nil {
		log.Printf("无法打开日志文件进行监控: %v", err)
		return
	}
	defer file.Close()

	// 移动到文件末尾
	file.Seek(0, 2)

	reader := bufio.NewReader(file)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// 如果到达文件末尾，等待一会儿再试
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// 去除换行符并发送到广播通道
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}

		// 发送到广播通道
		select {
		case api.GetLogBroadcast() <- line:
		default:
			// 通道满了，丢弃这条消息
		}
	}
}
