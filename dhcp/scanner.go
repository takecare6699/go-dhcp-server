package dhcp

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"dhcp-server/config"
)

// ScanStatus 扫描状态
type ScanStatus struct {
	IsRunning     bool      `json:"is_running"`
	IsEnabled     bool      `json:"is_enabled"`
	LastScanTime  time.Time `json:"last_scan_time"`
	NextScanTime  time.Time `json:"next_scan_time"`
	ScanProgress  int       `json:"scan_progress"` // 0-100
	ScannedIPs    int       `json:"scanned_ips"`
	TotalIPs      int       `json:"total_ips"`
	FoundDevices  int       `json:"found_devices"`
	ConflictedIPs int       `json:"conflicted_ips"`
	CurrentScan   string    `json:"current_scan"` // 当前扫描的IP
}

// NetworkScanner 网络扫描器
type NetworkScanner struct {
	config     *config.Config
	pool       *IPPool
	stopChan   chan struct{}
	wg         sync.WaitGroup
	scanMutex  sync.RWMutex
	logMutex   sync.Mutex // 独立的日志锁
	status     ScanStatus
	scanResult map[string]*DeviceInfo
	scanLog    []string // 扫描日志
}

// DeviceInfo 设备信息
type DeviceInfo struct {
	MAC      string    `json:"mac"`
	IP       string    `json:"ip"`
	Hostname string    `json:"hostname"`
	LastSeen time.Time `json:"last_seen"`
	IsActive bool      `json:"is_active"`
	Vendor   string    `json:"vendor"`
	Response string    `json:"response"` // 响应时间
}

// NewNetworkScanner 创建网络扫描器
func NewNetworkScanner(cfg *config.Config, pool *IPPool) *NetworkScanner {
	return &NetworkScanner{
		config:     cfg,
		pool:       pool,
		stopChan:   make(chan struct{}),
		scanResult: make(map[string]*DeviceInfo),
		scanLog:    make([]string, 0),
		status: ScanStatus{
			IsEnabled: cfg.Scanner.Enabled,
		},
	}
}

// Start 启动扫描器
func (scanner *NetworkScanner) Start() {
	scanner.scanMutex.Lock()
	defer scanner.scanMutex.Unlock()

	if scanner.status.IsRunning {
		log.Println("扫描器已在运行中")
		return
	}

	log.Println("启动网络扫描器...")
	scanner.status.IsRunning = true
	scanner.status.IsEnabled = true

	// 立即执行一次扫描
	go scanner.performScan()

	// 启动定时扫描任务
	scanner.wg.Add(1)
	go scanner.scanLoop()

	// 在锁外添加日志
	scanner.addScanLog("扫描器已启动")
}

// Stop 停止扫描器
func (scanner *NetworkScanner) Stop() {
	scanner.scanMutex.Lock()
	defer scanner.scanMutex.Unlock()

	if !scanner.status.IsRunning {
		log.Println("扫描器未在运行")
		return
	}

	log.Println("停止网络扫描器...")
	scanner.status.IsRunning = false
	scanner.status.IsEnabled = false

	close(scanner.stopChan)
	scanner.wg.Wait()

	// 重新创建stopChan以便下次启动
	scanner.stopChan = make(chan struct{})

	// 在锁外添加日志
	scanner.addScanLog("扫描器已停止")
}

// IsRunning 检查扫描器是否运行
func (scanner *NetworkScanner) IsRunning() bool {
	scanner.scanMutex.RLock()
	defer scanner.scanMutex.RUnlock()
	return scanner.status.IsRunning
}

// GetStatus 获取扫描器状态
func (scanner *NetworkScanner) GetStatus() ScanStatus {
	scanner.scanMutex.RLock()
	defer scanner.scanMutex.RUnlock()
	return scanner.status
}

// scanLoop 扫描循环
func (scanner *NetworkScanner) scanLoop() {
	defer scanner.wg.Done()

	ticker := time.NewTicker(time.Duration(scanner.config.Scanner.ScanInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-scanner.stopChan:
			return
		case <-ticker.C:
			scanner.performScan()
		}
	}
}

// performScan 执行网络扫描
func (scanner *NetworkScanner) performScan() {
	scanner.scanMutex.Lock()
	if !scanner.status.IsRunning {
		scanner.scanMutex.Unlock()
		return
	}
	scanner.scanMutex.Unlock()

	log.Println("开始网络扫描...")
	scanner.addScanLog("开始网络扫描")
	startTime := time.Now()

	// 获取网络范围
	network := scanner.config.Network.Subnet
	ipNet, err := scanner.parseNetwork(network)
	if err != nil {
		log.Printf("解析网络地址失败: %v", err)
		scanner.addScanLog(fmt.Sprintf("解析网络地址失败: %v", err))
		return
	}

	// 生成要扫描的IP列表
	ips := scanner.generateIPList(ipNet)

	// 更新状态（需要锁保护）
	scanner.scanMutex.Lock()
	scanner.status.TotalIPs = len(ips)
	scanner.status.ScannedIPs = 0
	scanner.status.FoundDevices = 0
	scanner.status.ScanProgress = 0
	scanner.scanMutex.Unlock()

	scanner.addScanLog(fmt.Sprintf("准备扫描 %d 个IP地址", len(ips)))

	// 扫描网络中的设备
	devices := scanner.scanNetworkWithProgress(ips)

	// 更新扫描结果
	scanner.updateScanResults(devices)

	// 检查IP冲突
	conflictedCount := scanner.checkIPConflicts()

	// 清理过期设备
	scanner.cleanupInactiveDevices()

	// 更新最终状态（需要锁保护）
	scanner.scanMutex.Lock()
	scanner.status.LastScanTime = time.Now()
	scanner.status.NextScanTime = time.Now().Add(time.Duration(scanner.config.Scanner.ScanInterval) * time.Second)
	scanner.status.ConflictedIPs = conflictedCount
	scanner.status.ScanProgress = 100
	scanner.status.CurrentScan = ""
	scanner.scanMutex.Unlock()

	duration := time.Since(startTime)
	log.Printf("网络扫描完成，耗时: %v，发现 %d 个设备，冲突IP: %d", duration, len(devices), conflictedCount)
	scanner.addScanLog(fmt.Sprintf("扫描完成，耗时: %v，发现 %d 个设备，冲突IP: %d", duration, len(devices), conflictedCount))
}

// scanNetworkWithProgress 带进度跟踪的网络扫描
func (scanner *NetworkScanner) scanNetworkWithProgress(ips []net.IP) map[string]*DeviceInfo {
	devices := make(map[string]*DeviceInfo)
	var wg sync.WaitGroup
	var mu sync.Mutex

	// 限制并发数
	semaphore := make(chan struct{}, scanner.config.Scanner.MaxConcurrency)

	for i, ip := range ips {
		wg.Add(1)
		go func(targetIP net.IP, index int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 更新进度
			scanner.scanMutex.Lock()
			scanner.status.ScannedIPs++
			scanner.status.ScanProgress = (scanner.status.ScannedIPs * 100) / scanner.status.TotalIPs
			scanner.status.CurrentScan = targetIP.String()
			scanner.scanMutex.Unlock()

			if device := scanner.pingDevice(targetIP); device != nil {
				mu.Lock()
				devices[device.MAC] = device
				mu.Unlock()

				// 更新发现设备计数
				scanner.scanMutex.Lock()
				scanner.status.FoundDevices++
				scanner.scanMutex.Unlock()

				scanner.addScanLog(fmt.Sprintf("发现设备: %s (%s) - %s", device.MAC, device.IP, device.Hostname))
			}
		}(ip, i)
	}

	wg.Wait()
	return devices
}

// parseNetwork 解析网络地址
func (scanner *NetworkScanner) parseNetwork(network string) (*net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(network)
	if err != nil {
		return nil, fmt.Errorf("无效的网络地址: %s", network)
	}
	return ipNet, nil
}

// generateIPList 生成要扫描的IP列表
func (scanner *NetworkScanner) generateIPList(ipNet *net.IPNet) []net.IP {
	var ips []net.IP

	// 获取网络地址
	networkIP := ipNet.IP.To4()
	if networkIP == nil {
		return ips
	}

	// 计算网络掩码
	mask := ipNet.Mask
	ones, bits := mask.Size()

	// 计算可用的IP数量
	hosts := 1 << uint(bits-ones)

	// 生成IP列表（跳过网络地址和广播地址）
	for i := 1; i < hosts-1; i++ {
		ip := make(net.IP, 4)
		copy(ip, networkIP)

		// 计算IP地址
		for j := 0; j < 4; j++ {
			ip[j] = networkIP[j] + byte((i>>(8*(3-j)))&0xFF)
		}

		// 检查是否在扫描范围内
		if scanner.isInScanRange(ip) {
			ips = append(ips, ip)
		}
	}

	return ips
}

// isInScanRange 检查IP是否在扫描范围内
func (scanner *NetworkScanner) isInScanRange(ip net.IP) bool {
	// 直接使用DHCP可分配的IP范围
	dhcpStartIP := net.ParseIP(scanner.config.Network.StartIP)
	dhcpEndIP := net.ParseIP(scanner.config.Network.EndIP)

	if dhcpStartIP != nil && dhcpEndIP != nil {
		return bytesToUint32(ip) >= bytesToUint32(dhcpStartIP) &&
			bytesToUint32(ip) <= bytesToUint32(dhcpEndIP)
	}

	return true
}

// pingDevice 探测设备
func (scanner *NetworkScanner) pingDevice(ip net.IP) *DeviceInfo {
	startTime := time.Now()

	// 使用ping命令探测设备
	cmd := exec.Command("ping", "-c", "1", "-W", strconv.Itoa(scanner.config.Scanner.PingTimeout/1000), ip.String())
	err := cmd.Run()

	responseTime := time.Since(startTime)

	if err != nil {
		return nil
	}

	// 获取MAC地址
	mac, err := scanner.getMACAddress(ip)
	if err != nil {
		return nil
	}

	// 获取主机名
	hostname := scanner.reverseDNSLookup(ip)

	// 获取厂商信息
	vendor := scanner.getVendorInfo(mac)

	return &DeviceInfo{
		MAC:      mac,
		IP:       ip.String(),
		Hostname: hostname,
		LastSeen: time.Now(),
		IsActive: true,
		Vendor:   vendor,
		Response: responseTime.String(),
	}
}

// getMACAddress 获取MAC地址
func (scanner *NetworkScanner) getMACAddress(ip net.IP) (string, error) {
	// 使用arp命令获取MAC地址
	cmd := exec.Command("arp", "-n", ip.String())
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, ip.String()) {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				mac := fields[2]
				if mac != "<incomplete>" && mac != "incomplete" {
					return mac, nil
				}
			}
		}
	}

	return "", fmt.Errorf("无法获取MAC地址")
}

// reverseDNSLookup 反向DNS查找
func (scanner *NetworkScanner) reverseDNSLookup(ip net.IP) string {
	names, err := net.LookupAddr(ip.String())
	if err != nil || len(names) == 0 {
		return ""
	}

	// 移除末尾的点
	hostname := names[0]
	if len(hostname) > 0 && hostname[len(hostname)-1] == '.' {
		hostname = hostname[:len(hostname)-1]
	}

	return hostname
}

// getVendorInfo 获取厂商信息
func (scanner *NetworkScanner) getVendorInfo(mac string) string {
	// 简单的MAC地址厂商识别
	if strings.HasPrefix(mac, "00:50:56") {
		return "VMware"
	} else if strings.HasPrefix(mac, "00:0c:29") {
		return "VMware"
	} else if strings.HasPrefix(mac, "00:1a:11") {
		return "Google"
	} else if strings.HasPrefix(mac, "00:16:3e") {
		return "Xen"
	} else if strings.HasPrefix(mac, "52:54:00") {
		return "QEMU"
	}
	return "Unknown"
}

// updateScanResults 更新扫描结果
func (scanner *NetworkScanner) updateScanResults(devices map[string]*DeviceInfo) {
	now := time.Now()

	// 更新现有设备状态
	for mac, device := range devices {
		if existing, exists := scanner.scanResult[mac]; exists {
			existing.IP = device.IP
			existing.Hostname = device.Hostname
			existing.LastSeen = now
			existing.IsActive = true
			existing.Response = device.Response
		} else {
			scanner.scanResult[mac] = device
		}
	}

	// 标记未响应的设备为非活跃
	for mac, device := range scanner.scanResult {
		if _, exists := devices[mac]; !exists {
			device.IsActive = false
		}
	}
}

// checkIPConflicts 检查IP冲突
func (scanner *NetworkScanner) checkIPConflicts() int {
	conflictCount := 0
	// 检查扫描结果与DHCP租约的冲突
	leases := scanner.pool.GetAllLeases()

	for _, lease := range leases {
		// 检查租约中的IP是否被其他设备使用
		for deviceMAC, device := range scanner.scanResult {
			if device.IP == lease.IP.String() && deviceMAC != lease.MAC {
				log.Printf("检测到IP冲突: %s 被 %s 使用，但租约分配给 %s",
					device.IP, deviceMAC, lease.MAC)

				// 标记IP为冲突状态
				scanner.pool.MarkIPAsConflict(device.IP)
				conflictCount++
			}
		}
	}

	return conflictCount
}

// cleanupInactiveDevices 清理非活跃设备
func (scanner *NetworkScanner) cleanupInactiveDevices() {
	cutoffTime := time.Now().Add(-time.Duration(scanner.config.Scanner.InactiveTimeout) * time.Hour)
	var devicesToRemove []string

	// 先收集需要清理的设备
	for mac, device := range scanner.scanResult {
		if !device.IsActive && device.LastSeen.Before(cutoffTime) {
			devicesToRemove = append(devicesToRemove, mac)
		}
	}

	// 然后清理设备并记录日志
	for _, mac := range devicesToRemove {
		device := scanner.scanResult[mac]
		delete(scanner.scanResult, mac)
		log.Printf("清理非活跃设备: %s (%s)", mac, device.IP)
		scanner.addScanLog(fmt.Sprintf("清理非活跃设备: %s (%s)", mac, device.IP))
	}
}

// GetScanResults 获取扫描结果
func (scanner *NetworkScanner) GetScanResults() map[string]*DeviceInfo {
	scanner.scanMutex.RLock()
	defer scanner.scanMutex.RUnlock()

	result := make(map[string]*DeviceInfo)
	for mac, device := range scanner.scanResult {
		result[mac] = device
	}
	return result
}

// GetLastScanTime 获取最后扫描时间
func (scanner *NetworkScanner) GetLastScanTime() time.Time {
	scanner.scanMutex.RLock()
	defer scanner.scanMutex.RUnlock()
	return scanner.status.LastScanTime
}

// GetScanLog 获取扫描日志
func (scanner *NetworkScanner) GetScanLog() []string {
	scanner.logMutex.Lock()
	defer scanner.logMutex.Unlock()

	// 只返回最近的100条日志
	if len(scanner.scanLog) > 100 {
		return scanner.scanLog[len(scanner.scanLog)-100:]
	}
	return scanner.scanLog
}

// addScanLog 添加扫描日志
func (scanner *NetworkScanner) addScanLog(message string) {
	scanner.logMutex.Lock()
	defer scanner.logMutex.Unlock()

	timestamp := time.Now().Format("15:04:05")
	logEntry := fmt.Sprintf("[%s] %s", timestamp, message)
	scanner.scanLog = append(scanner.scanLog, logEntry)

	// 限制日志数量
	if len(scanner.scanLog) > 1000 {
		scanner.scanLog = scanner.scanLog[len(scanner.scanLog)-1000:]
	}
}

// bytesToUint32 将IP地址转换为uint32
func bytesToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}
