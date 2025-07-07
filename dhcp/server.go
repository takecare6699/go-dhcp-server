package dhcp

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"

	"dhcp-server/config"
	"dhcp-server/gateway"
)

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

// Server DHCP服务器
type Server struct {
	config        *config.Config
	pool          *IPPool
	healthChecker *gateway.HealthChecker
	server        *server4.Server
	startTime     time.Time
	history       []HistoryRecord
	historyMutex  sync.RWMutex
	maxHistory    int
}

// NewServer 创建DHCP服务器
func NewServer(cfg *config.Config) (*Server, error) {
	// 创建IP地址池
	pool, err := NewIPPool(cfg)
	if err != nil {
		return nil, err
	}

	// 创建健康检查器
	healthChecker := gateway.NewHealthChecker(cfg)

	// 创建服务器
	s := &Server{
		config:        cfg,
		pool:          pool,
		healthChecker: healthChecker,
		startTime:     time.Now(),
		history:       make([]HistoryRecord, 0),
		maxHistory:    1000, // 保留最近1000条记录
	}

	// 添加一些示例历史记录用于测试（如果没有真实的DHCP活动）
	s.addSampleHistory()

	return s, nil
}

// addHistory 添加历史记录
func (s *Server) addHistory(ip, mac, hostname, action, gateway string) {
	s.historyMutex.Lock()
	defer s.historyMutex.Unlock()

	record := HistoryRecord{
		IP:        ip,
		MAC:       mac,
		Hostname:  hostname,
		Action:    action,
		Timestamp: time.Now(),
		Gateway:   gateway,
		ServerIP:  s.getServerIP().String(),
	}

	s.history = append(s.history, record)

	// 保持历史记录数量在限制内
	if len(s.history) > s.maxHistory {
		s.history = s.history[len(s.history)-s.maxHistory:]
	}
}

// addSampleHistory 添加示例历史记录（用于测试和演示）
func (s *Server) addSampleHistory() {
	s.historyMutex.Lock()
	defer s.historyMutex.Unlock()

	// 只有在历史记录为空时才添加示例数据
	if len(s.history) > 0 {
		return
	}

	sampleRecords := []HistoryRecord{
		{
			IP:        "192.168.1.101",
			MAC:       "aa:bb:cc:dd:ee:01",
			Hostname:  "test-device-01",
			Action:    "DISCOVER",
			Timestamp: time.Now().Add(-time.Hour * 2),
			Gateway:   "main_gateway",
			ServerIP:  s.getServerIP().String(),
		},
	}

	s.history = append(s.history, sampleRecords...)
	log.Printf("已添加 %d 条示例历史记录用于测试", len(sampleRecords))
}

// Start 启动DHCP服务器
func (s *Server) Start() error {
	log.Printf("启动DHCP服务器，监听接口: %s, 端口: %d",
		s.config.Server.Interface, s.config.Server.Port)

	// 启动健康检查服务
	go s.healthChecker.Start()

	// 启动IP租约清理任务
	s.pool.StartCleanupTask()

	// 创建DHCP服务器
	laddr := &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: s.config.Server.Port,
	}

	server, err := server4.NewServer(s.config.Server.Interface, laddr, s.handleDHCPPacket)
	if err != nil {
		return err
	}

	s.server = server

	log.Println("DHCP服务器启动成功")
	return server.Serve()
}

// Stop 停止DHCP服务器
func (s *Server) Stop() {
	log.Println("正在停止DHCP服务器...")

	if s.healthChecker != nil {
		s.healthChecker.Stop()
	}

	if s.server != nil {
		s.server.Close()
	}

	log.Println("DHCP服务器已停止")
}

// handleDHCPPacket 处理DHCP数据包
func (s *Server) handleDHCPPacket(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
	log.Printf("收到来自 %s 的DHCP请求: %s", peer, m.MessageType())

	var response *dhcpv4.DHCPv4
	var err error

	switch m.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		response, err = s.handleDiscover(m)
	case dhcpv4.MessageTypeRequest:
		response, err = s.handleRequest(m)
	case dhcpv4.MessageTypeRelease:
		err = s.handleRelease(m)
	case dhcpv4.MessageTypeDecline:
		err = s.handleDecline(m)
	case dhcpv4.MessageTypeInform:
		response, err = s.handleInform(m)
	default:
		log.Printf("不支持的DHCP消息类型: %s", m.MessageType())
		return
	}

	if err != nil {
		log.Printf("处理DHCP请求出错: %v", err)
		return
	}

	if response != nil {
		// 发送响应
		if _, err := conn.WriteTo(response.ToBytes(), peer); err != nil {
			log.Printf("发送DHCP响应失败: %v", err)
		} else {
			log.Printf("发送DHCP响应到 %s: %s (IP: %s)",
				peer, response.MessageType(), response.YourIPAddr)
		}
	}
}

// handleDiscover 处理DHCP Discover消息
func (s *Server) handleDiscover(req *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, error) {
	log.Printf("=== 开始处理DHCP Discover ===")

	clientMAC := req.ClientHWAddr.String()
	hostname := req.HostName()
	requestedIP := req.RequestedIPAddress()

	log.Printf("DHCP Discover: MAC=%s, Hostname=%s, RequestedIP=%s",
		clientMAC, hostname, requestedIP)

	log.Printf("准备调用 s.pool.RequestIP...")

	// 尝试分配IP地址
	lease, err := s.pool.RequestIP(clientMAC, requestedIP, hostname)
	if err != nil {
		log.Printf("无法分配IP地址: %v", err)
		return nil, err
	}

	log.Printf("IP分配成功: %s", lease.IP)

	// 记录历史和更新租约网关信息
	gateway := s.selectGateway(lease)
	gatewayName := ""
	if gateway != nil {
		gatewayName = gateway.Name
		lease.GatewayIP = gateway.IP // 记录实际响应的网关IP
		log.Printf("为客户端分配网关: %s (%s)", gatewayName, gateway.IP)
	} else {
		log.Printf("警告: 没有找到可用的网关")
	}
	s.addHistory(lease.IP.String(), clientMAC, hostname, "DISCOVER", gatewayName)

	log.Printf("准备创建DHCP Offer响应...")

	// 创建DHCP Offer响应
	offer := s.createResponse(req, dhcpv4.MessageTypeOffer, lease)
	if offer == nil {
		log.Printf("创建DHCP Offer响应失败")
		return nil, fmt.Errorf("创建DHCP Offer响应失败")
	}

	log.Printf("发送DHCP Offer: MAC=%s, IP=%s", clientMAC, lease.IP)
	log.Printf("=== DHCP Discover处理完成 ===")
	return offer, nil
}

// handleRequest 处理DHCP Request消息
func (s *Server) handleRequest(req *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, error) {
	clientMAC := req.ClientHWAddr.String()
	requestedIP := req.RequestedIPAddress()
	serverIP := req.ServerIdentifier()

	log.Printf("DHCP Request: MAC=%s, RequestedIP=%s, ServerIP=%s",
		clientMAC, requestedIP, serverIP)

	// 检查是否是对我们的响应
	if serverIP != nil && !s.isOurServerIP(serverIP) && !s.config.Server.AllowAnyServerIP {
		log.Printf("DHCP Request不是发给我们的服务器 (ServerIP: %s)，已拒绝。若需允许响应任意ServerIP，请在配置中开启该选项。", serverIP)
		return nil, nil
	}

	// 验证请求的IP地址
	if requestedIP == nil {
		requestedIP = req.ClientIPAddr
	}

	if requestedIP == nil || requestedIP.IsUnspecified() {
		log.Printf("DHCP Request中没有有效的IP地址")
		return s.createNAK(req, "无效的IP地址"), nil
	}

	// 优先查找MAC的租约
	lease, existsByMAC := s.pool.GetLeaseByMAC(clientMAC)
	if existsByMAC {
		if lease.IP.Equal(requestedIP) {
			log.Printf("DHCP Request: MAC和IP都匹配，续租")
			if !lease.IsStatic {
				lease.StartTime = time.Now()
			}
		} else {
			log.Printf("DHCP Request: 设备请求IP %s，但分配的IP是 %s，使用分配的IP", requestedIP, lease.IP)
			if !lease.IsStatic {
				lease.StartTime = time.Now()
			}
		}
	} else {
		// 没有MAC租约，尝试分配新IP
		var err error
		lease, err = s.pool.RequestIP(clientMAC, nil, req.HostName())
		if err != nil {
			log.Printf("DHCP Request: 无法分配新IP: %v", err)
			return s.createNAK(req, "无法分配新IP"), nil
		}
		log.Printf("DHCP Request: 为MAC=%s分配新IP: %s", clientMAC, lease.IP)
	}

	// 记录历史和更新租约网关信息
	gateway := s.selectGateway(lease)
	gatewayName := ""
	if gateway != nil {
		gatewayName = gateway.Name
		lease.GatewayIP = gateway.IP // 记录实际响应的网关IP
	}
	s.addHistory(lease.IP.String(), clientMAC, req.HostName(), "REQUEST", gatewayName)

	// 创建DHCP ACK响应
	ack := s.createResponse(req, dhcpv4.MessageTypeAck, lease)

	log.Printf("发送DHCP ACK: MAC=%s, IP=%s", clientMAC, lease.IP)
	return ack, nil
}

// handleRelease 处理DHCP Release消息
func (s *Server) handleRelease(req *dhcpv4.DHCPv4) error {
	clientMAC := req.ClientHWAddr.String()
	clientIP := req.ClientIPAddr

	log.Printf("DHCP Release: MAC=%s, IP=%s", clientMAC, clientIP)

	// 记录历史
	s.addHistory(clientIP.String(), clientMAC, req.HostName(), "RELEASE", "")

	if err := s.pool.ReleaseIP(clientMAC); err != nil {
		log.Printf("释放IP地址失败: %v", err)
		return err
	}

	log.Printf("成功释放IP地址: %s", clientIP)
	return nil
}

// handleDecline 处理DHCP Decline消息
func (s *Server) handleDecline(req *dhcpv4.DHCPv4) error {
	clientMAC := req.ClientHWAddr.String()
	requestedIP := req.RequestedIPAddress()

	log.Printf("DHCP Decline: MAC=%s, IP=%s", clientMAC, requestedIP)

	// 记录历史
	s.addHistory(requestedIP.String(), clientMAC, req.HostName(), "DECLINE", "")

	// 将IP地址标记为有问题，暂时从池中移除
	// 这里可以实现更复杂的逻辑，比如记录问题IP等
	log.Printf("客户端拒绝IP地址: %s", requestedIP)

	// 改进：将拒绝的IP地址标记为冲突状态
	if requestedIP != nil && !requestedIP.IsUnspecified() {
		// 释放该IP的租约
		if err := s.pool.ReleaseIP(clientMAC); err != nil {
			log.Printf("释放被拒绝IP的租约失败: %v", err)
		}

		// 将IP标记为冲突状态
		s.pool.MarkIPAsConflict(requestedIP.String())
		log.Printf("IP地址 %s 被标记为冲突状态，将暂时避免分配给其他客户端", requestedIP)
	}

	return nil
}

// handleInform 处理DHCP Inform消息
func (s *Server) handleInform(req *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, error) {
	clientMAC := req.ClientHWAddr.String()
	clientIP := req.ClientIPAddr

	log.Printf("DHCP Inform: MAC=%s, IP=%s", clientMAC, clientIP)

	// 记录历史
	s.addHistory(clientIP.String(), clientMAC, req.HostName(), "INFORM", "")

	// 为DHCP Inform创建响应（只提供配置信息，不分配IP）
	resp, err := dhcpv4.NewReplyFromRequest(req)
	if err != nil {
		return nil, err
	}

	resp.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
	s.addNetworkOptions(resp, nil) // 不指定特定网关

	log.Printf("发送DHCP ACK (Inform): MAC=%s", clientMAC)
	return resp, nil
}

// createResponse 创建DHCP响应
func (s *Server) createResponse(req *dhcpv4.DHCPv4, msgType dhcpv4.MessageType, lease *IPLease) *dhcpv4.DHCPv4 {
	resp, err := dhcpv4.NewReplyFromRequest(req)
	if err != nil {
		log.Printf("创建DHCP响应失败: %v", err)
		return nil
	}

	// 设置消息类型
	resp.UpdateOption(dhcpv4.OptMessageType(msgType))

	// 设置分配的IP地址
	resp.YourIPAddr = lease.IP

	// 设置服务器标识符
	resp.UpdateOption(dhcpv4.OptServerIdentifier(s.getServerIP()))

	// 设置租期时间
	if !lease.IsStatic {
		resp.UpdateOption(dhcpv4.OptIPAddressLeaseTime(lease.LeaseTime))
	} else {
		resp.UpdateOption(dhcpv4.OptIPAddressLeaseTime(time.Hour * 24 * 365)) // 静态地址设置长租期
	}

	// 添加网络配置选项
	s.addNetworkOptions(resp, lease)

	return resp
}

// createNAK 创建DHCP NAK响应
func (s *Server) createNAK(req *dhcpv4.DHCPv4, message string) *dhcpv4.DHCPv4 {
	resp, err := dhcpv4.NewReplyFromRequest(req)
	if err != nil {
		log.Printf("创建DHCP NAK失败: %v", err)
		return nil
	}

	resp.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeNak))
	resp.UpdateOption(dhcpv4.OptServerIdentifier(s.getServerIP()))
	resp.UpdateOption(dhcpv4.OptMessage(message))

	// NAK消息不分配IP地址
	resp.YourIPAddr = net.IPv4zero

	return resp
}

// addNetworkOptions 添加网络配置选项
func (s *Server) addNetworkOptions(resp *dhcpv4.DHCPv4, lease *IPLease) {
	// 子网掩码
	if netmask := net.ParseIP(s.config.Network.Netmask); netmask != nil {
		resp.UpdateOption(dhcpv4.OptSubnetMask(net.IPMask(netmask.To4())))
	}

	// 网关 - 根据租约选择合适的网关
	gateway := s.selectGateway(lease)
	if gateway != nil {
		gatewayIP := net.ParseIP(gateway.IP)
		if gatewayIP != nil {
			resp.UpdateOption(dhcpv4.OptRouter(gatewayIP.To4()))
			log.Printf("为客户端分配网关: %s (%s)", gateway.Name, gateway.IP)
		}
	} else {
		// 即使网关不健康，也要添加默认网关
		if len(s.config.Gateways) > 0 {
			defaultGateway := s.config.Gateways[0]
			gatewayIP := net.ParseIP(defaultGateway.IP)
			if gatewayIP != nil {
				resp.UpdateOption(dhcpv4.OptRouter(gatewayIP.To4()))
				log.Printf("为客户端分配默认网关: %s (%s)", defaultGateway.Name, defaultGateway.IP)
			}
		}
	}

	// DNS服务器 - 优先使用网关的DNS，然后合并网络配置的DNS
	var dnsServers []net.IP

	// 如果网关配置了DNS，优先使用网关的DNS
	if gateway != nil && len(gateway.DNSServers) > 0 {
		for _, dns := range gateway.DNSServers {
			if ip := net.ParseIP(dns); ip != nil {
				dnsServers = append(dnsServers, ip.To4())
			}
		}
		log.Printf("为客户端分配网关专用DNS: %v", gateway.DNSServers)
	}

	// 合并网络配置中的DNS（避免重复）
	if len(s.config.Network.DNSServers) > 0 {
		for _, dns := range s.config.Network.DNSServers {
			if ip := net.ParseIP(dns); ip != nil {
				// 检查是否已经存在
				exists := false
				for _, existingIP := range dnsServers {
					if existingIP.Equal(ip.To4()) {
						exists = true
						break
					}
				}
				if !exists {
					dnsServers = append(dnsServers, ip.To4())
				}
			}
		}
	}

	if len(dnsServers) > 0 {
		resp.UpdateOption(dhcpv4.OptDNS(dnsServers...))
	}

	// 域名
	if s.config.Network.DomainName != "" {
		resp.UpdateOption(dhcpv4.OptDomainName(s.config.Network.DomainName))
	}

	// 续租时间 (T1) - 使用租期的一半
	// resp.UpdateOption(dhcpv4.OptRenewalTime(s.config.Server.LeaseTime / 2))

	// 重绑定时间 (T2) - 使用租期的7/8
	// resp.UpdateOption(dhcpv4.OptRebindingTime(s.config.Server.LeaseTime * 7 / 8))
}

// selectGateway 选择合适的网关
func (s *Server) selectGateway(lease *IPLease) *config.Gateway {
	if lease == nil {
		// 没有租约信息，返回健康的默认网关
		return s.healthChecker.GetHealthyGateway("")
	}

	if lease.IsStatic && lease.Gateway != "" {
		// 静态绑定指定了网关，优先使用指定的网关（如果健康的话）
		return s.healthChecker.GetHealthyGateway(lease.Gateway)
	}

	// 动态分配或静态绑定没有指定网关，使用默认网关
	return s.healthChecker.GetHealthyGateway("")
}

// getServerIP 获取服务器IP地址
func (s *Server) getServerIP() net.IP {
	// 这里应该返回服务器在指定接口上的IP地址
	// 简化实现，可以从配置中获取或自动检测
	interfaces, err := net.Interfaces()
	if err != nil {
		log.Printf("获取网络接口失败: %v", err)
		return net.ParseIP("192.168.1.1") // 默认值
	}

	for _, iface := range interfaces {
		if iface.Name == s.config.Server.Interface {
			log.Printf("检查接口 %s 的IP地址", iface.Name)
			addrs, err := iface.Addrs()
			if err != nil {
				log.Printf("获取接口 %s 地址失败: %v", iface.Name, err)
				continue
			}
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
					if ipnet.IP.To4() != nil {
						log.Printf("检测到服务器IP: %s", ipnet.IP.To4())
						return ipnet.IP.To4()
					}
				}
			}
		}
	}

	// 如果无法自动检测，返回网络配置中的第一个网关作为服务器IP
	if len(s.config.Gateways) > 0 {
		serverIP := net.ParseIP(s.config.Gateways[0].IP)
		log.Printf("使用默认网关作为服务器IP: %s", serverIP)
		return serverIP
	}

	log.Printf("使用默认服务器IP: 192.168.1.1")
	return net.ParseIP("192.168.1.1") // 最后的默认值
}

// isOurServerIP 检查是否是我们的服务器IP
func (s *Server) isOurServerIP(ip net.IP) bool {
	serverIP := s.getServerIP()
	return ip.Equal(serverIP)
}

// GetPoolStats 获取地址池统计信息
func (s *Server) GetPoolStats() map[string]interface{} {
	return s.pool.GetPoolStats()
}

// GetGatewayStatus 获取网关状态
func (s *Server) GetGatewayStatus() map[string]bool {
	return s.healthChecker.GetGatewayStatus()
}

// GetStartTime 获取服务器启动时间
func (s *Server) GetStartTime() time.Time {
	return s.startTime
}

// GetPool 获取IP地址池
func (s *Server) GetPool() *IPPool {
	return s.pool
}

// GetChecker 获取健康检查器
func (s *Server) GetChecker() *gateway.HealthChecker {
	return s.healthChecker
}

// GetAllLeases 获取所有租约
func (s *Server) GetAllLeases() []*IPLease {
	return s.pool.GetAllLeases()
}

// GetActiveLeases 获取活跃租约
func (s *Server) GetActiveLeases() []*IPLease {
	return s.pool.GetActiveLeases()
}

// GetHistory 获取历史记录
func (s *Server) GetHistory(limit int, macFilter, ipFilter string) []HistoryRecord {
	s.historyMutex.RLock()
	defer s.historyMutex.RUnlock()

	var filtered []HistoryRecord

	// 从最新记录开始遍历
	for i := len(s.history) - 1; i >= 0 && len(filtered) < limit; i-- {
		record := s.history[i]

		// 应用过滤条件
		if macFilter != "" && record.MAC != macFilter {
			continue
		}
		if ipFilter != "" && record.IP != ipFilter {
			continue
		}

		filtered = append(filtered, record)
	}

	return filtered
}
