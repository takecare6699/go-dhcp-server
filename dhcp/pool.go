package dhcp

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"dhcp-server/config"
)

// IPLease IP租约信息
type IPLease struct {
	IP        net.IP
	MAC       string
	Hostname  string
	StartTime time.Time
	LeaseTime time.Duration
	IsStatic  bool
	Gateway   string // 配置中的网关名称
	GatewayIP string // 实际响应的网关IP地址
}

// IsExpired 检查租约是否过期
func (lease *IPLease) IsExpired() bool {
	if lease.IsStatic {
		return false
	}
	return time.Now().After(lease.StartTime.Add(lease.LeaseTime))
}

// RemainingTime 获取剩余租期时间
func (lease *IPLease) RemainingTime() time.Duration {
	if lease.IsStatic {
		return time.Hour * 24 * 365 // 静态地址返回一年
	}
	remaining := lease.StartTime.Add(lease.LeaseTime).Sub(time.Now())
	if remaining < 0 {
		return 0
	}
	return remaining
}

// IPPool IP地址池
type IPPool struct {
	config    *config.Config
	startIP   net.IP
	endIP     net.IP
	leases    map[string]*IPLease // key: IP地址字符串
	macToIP   map[string]string   // MAC地址到IP的映射
	mutex     sync.RWMutex
	leaseTime time.Duration
}

// NewIPPool 创建IP地址池
func NewIPPool(cfg *config.Config) (*IPPool, error) {
	startIP := net.ParseIP(cfg.Network.StartIP)
	if startIP == nil {
		return nil, fmt.Errorf("无效的起始IP地址: %s", cfg.Network.StartIP)
	}

	endIP := net.ParseIP(cfg.Network.EndIP)
	if endIP == nil {
		return nil, fmt.Errorf("无效的结束IP地址: %s", cfg.Network.EndIP)
	}

	pool := &IPPool{
		config:    cfg,
		startIP:   startIP.To4(),
		endIP:     endIP.To4(),
		leases:    make(map[string]*IPLease),
		macToIP:   make(map[string]string),
		leaseTime: cfg.Server.LeaseTime,
	}

	// 初始化静态绑定
	if err := pool.initStaticBindings(); err != nil {
		return nil, fmt.Errorf("初始化静态绑定失败: %v", err)
	}

	log.Printf("IP地址池初始化完成，范围: %s - %s", startIP, endIP)
	return pool, nil
}

// initStaticBindings 初始化静态绑定
func (pool *IPPool) initStaticBindings() error {
	for _, binding := range pool.config.Bindings {
		ip := net.ParseIP(binding.IP)
		if ip == nil {
			return fmt.Errorf("静态绑定中的IP地址无效: %s", binding.IP)
		}

		// 标准化MAC地址格式
		mac, err := net.ParseMAC(binding.MAC)
		if err != nil {
			return fmt.Errorf("静态绑定中的MAC地址无效: %s", binding.MAC)
		}
		macStr := mac.String()

		lease := &IPLease{
			IP:        ip.To4(),
			MAC:       macStr,
			Hostname:  binding.Hostname,
			StartTime: time.Now(),
			IsStatic:  true,
			Gateway:   binding.Gateway,
		}

		ipStr := ip.String()
		pool.leases[ipStr] = lease
		pool.macToIP[macStr] = ipStr

		log.Printf("添加静态绑定: %s (%s) -> %s, 网关: %s",
			binding.Alias, macStr, binding.IP, binding.Gateway)
	}

	return nil
}

// RequestIP 请求IP地址
func (pool *IPPool) RequestIP(clientMAC string, requestedIP net.IP, hostname string) (*IPLease, error) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()

	// 标准化MAC地址格式
	mac, err := net.ParseMAC(clientMAC)
	if err != nil {
		return nil, fmt.Errorf("无效的MAC地址: %s", clientMAC)
	}
	macStr := mac.String()

	// 检查是否有静态绑定
	if existingIP, exists := pool.macToIP[macStr]; exists {
		if lease, ok := pool.leases[existingIP]; ok && lease.IsStatic {
			log.Printf("返回静态绑定IP: %s -> %s", macStr, existingIP)
			return lease, nil
		}
	}

	// 检查是否已有动态租约
	if existingIP, exists := pool.macToIP[macStr]; exists {
		if lease, ok := pool.leases[existingIP]; ok && !lease.IsExpired() {
			// 续租现有IP
			lease.StartTime = time.Now()
			lease.Hostname = hostname
			log.Printf("续租IP: %s -> %s", macStr, existingIP)
			return lease, nil
		} else {
			// 清理过期租约
			delete(pool.leases, existingIP)
			delete(pool.macToIP, macStr)
		}
	}

	// 如果客户端请求特定IP，检查是否可用
	if requestedIP != nil && !requestedIP.IsUnspecified() {
		if pool.isIPInRange(requestedIP) && pool.isIPAvailable(requestedIP.String()) {
			return pool.allocateIP(requestedIP, macStr, hostname), nil
		}
	}

	// 分配新的IP地址
	newIP := pool.findAvailableIP()
	if newIP == nil {
		return nil, fmt.Errorf("地址池已满，无法分配新IP")
	}

	return pool.allocateIP(newIP, macStr, hostname), nil
}

// allocateIP 分配IP地址
func (pool *IPPool) allocateIP(ip net.IP, mac, hostname string) *IPLease {
	lease := &IPLease{
		IP:        ip.To4(),
		MAC:       mac,
		Hostname:  hostname,
		StartTime: time.Now(),
		LeaseTime: pool.leaseTime,
		IsStatic:  false,
		Gateway:   "", // 动态分配的使用默认网关
	}

	ipStr := ip.String()
	pool.leases[ipStr] = lease
	pool.macToIP[mac] = ipStr

	log.Printf("分配新IP: %s -> %s", mac, ipStr)
	return lease
}

// findAvailableIP 查找可用的IP地址
func (pool *IPPool) findAvailableIP() net.IP {
	start := binary.BigEndian.Uint32(pool.startIP)
	end := binary.BigEndian.Uint32(pool.endIP)

	for i := start; i <= end; i++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, i)

		if pool.isIPAvailable(ip.String()) {
			return ip
		}
	}

	return nil
}

// isIPAvailable 检查IP是否可用
func (pool *IPPool) isIPAvailable(ip string) bool {
	lease, exists := pool.leases[ip]
	if !exists {
		return true
	}

	// 静态绑定的IP不可用
	if lease.IsStatic {
		return false
	}

	// 检查是否过期
	return lease.IsExpired()
}

// isIPInRange 检查IP是否在范围内
func (pool *IPPool) isIPInRange(ip net.IP) bool {
	start := binary.BigEndian.Uint32(pool.startIP)
	end := binary.BigEndian.Uint32(pool.endIP)
	target := binary.BigEndian.Uint32(ip.To4())

	return target >= start && target <= end
}

// ReleaseIP 释放IP地址
func (pool *IPPool) ReleaseIP(clientMAC string) error {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()

	mac, err := net.ParseMAC(clientMAC)
	if err != nil {
		return fmt.Errorf("无效的MAC地址: %s", clientMAC)
	}
	macStr := mac.String()

	ipStr, exists := pool.macToIP[macStr]
	if !exists {
		return fmt.Errorf("未找到MAC地址 %s 的租约", macStr)
	}

	lease, ok := pool.leases[ipStr]
	if !ok {
		return fmt.Errorf("未找到IP地址 %s 的租约", ipStr)
	}

	// 静态绑定不能释放
	if lease.IsStatic {
		return fmt.Errorf("无法释放静态绑定的IP地址: %s", ipStr)
	}

	delete(pool.leases, ipStr)
	delete(pool.macToIP, macStr)

	log.Printf("释放IP: %s -> %s", macStr, ipStr)
	return nil
}

// GetLease 获取租约信息
func (pool *IPPool) GetLease(ip string) (*IPLease, bool) {
	pool.mutex.RLock()
	defer pool.mutex.RUnlock()

	lease, exists := pool.leases[ip]
	return lease, exists
}

// GetLeaseByMAC 根据MAC地址获取租约
func (pool *IPPool) GetLeaseByMAC(mac string) (*IPLease, bool) {
	pool.mutex.RLock()
	defer pool.mutex.RUnlock()

	ipStr, exists := pool.macToIP[mac]
	if !exists {
		return nil, false
	}

	lease, ok := pool.leases[ipStr]
	return lease, ok
}

// CleanupExpiredLeases 清理过期租约
func (pool *IPPool) CleanupExpiredLeases() {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()

	var expiredIPs []string
	var expiredMACs []string

	for ip, lease := range pool.leases {
		if !lease.IsStatic && lease.IsExpired() {
			expiredIPs = append(expiredIPs, ip)
			expiredMACs = append(expiredMACs, lease.MAC)
		}
	}

	for i, ip := range expiredIPs {
		delete(pool.leases, ip)
		delete(pool.macToIP, expiredMACs[i])
		log.Printf("清理过期租约: %s -> %s", expiredMACs[i], ip)
	}

	if len(expiredIPs) > 0 {
		log.Printf("清理了 %d 个过期租约", len(expiredIPs))
	}
}

// GetPoolStats 获取地址池统计信息
func (pool *IPPool) GetPoolStats() map[string]interface{} {
	pool.mutex.RLock()
	defer pool.mutex.RUnlock()

	start := binary.BigEndian.Uint32(pool.startIP)
	end := binary.BigEndian.Uint32(pool.endIP)
	total := int(end - start + 1)

	staticCount := 0
	dynamicCount := 0
	expiredCount := 0

	for _, lease := range pool.leases {
		if lease.IsStatic {
			staticCount++
		} else if lease.IsExpired() {
			expiredCount++
		} else {
			dynamicCount++
		}
	}

	available := total - staticCount - dynamicCount

	return map[string]interface{}{
		"total_ips":      total,
		"static_leases":  staticCount,
		"dynamic_leases": dynamicCount,
		"expired_leases": expiredCount,
		"available_ips":  available,
		"utilization":    float64(staticCount+dynamicCount) / float64(total) * 100,
	}
}

// StartCleanupTask 启动清理任务
func (pool *IPPool) StartCleanupTask() {
	go func() {
		ticker := time.NewTicker(time.Hour) // 每小时清理一次
		defer ticker.Stop()

		for range ticker.C {
			pool.CleanupExpiredLeases()
		}
	}()

	log.Println("启动IP租约清理任务")
}

// GetAllLeases 获取所有租约（包括过期的）
func (pool *IPPool) GetAllLeases() []*IPLease {
	pool.mutex.RLock()
	defer pool.mutex.RUnlock()

	var leases []*IPLease
	for _, lease := range pool.leases {
		leases = append(leases, lease)
	}

	return leases
}

// GetActiveLeases 获取所有活跃租约（未过期的）
func (pool *IPPool) GetActiveLeases() []*IPLease {
	pool.mutex.RLock()
	defer pool.mutex.RUnlock()

	var activeLeases []*IPLease
	for _, lease := range pool.leases {
		if !lease.IsExpired() {
			activeLeases = append(activeLeases, lease)
		}
	}

	return activeLeases
}
