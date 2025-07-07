package config

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Config 主配置结构
type Config struct {
	Server      ServerConfig  `yaml:"server"`
	Network     NetworkConfig `yaml:"network"`
	Gateways    []Gateway     `yaml:"gateways"`
	Bindings    []MACBinding  `yaml:"bindings"`
	Devices     []DeviceInfo  `yaml:"devices"` // 设备信息配置
	HealthCheck HealthConfig  `yaml:"health_check"`
	Scanner     ScannerConfig `yaml:"scanner"` // 网络扫描器配置
}

// ServerConfig DHCP服务器配置
type ServerConfig struct {
	Interface        string        `yaml:"interface" json:"interface"`
	Port             int           `yaml:"port" json:"port"`
	LeaseTime        time.Duration `yaml:"lease_time" json:"lease_time"`
	APIPort          int           `yaml:"api_port" json:"api_port"`
	APIHost          string        `yaml:"api_host" json:"api_host"` // API监听地址
	LogLevel         string        `yaml:"log_level" json:"log_level"`
	LogFile          string        `yaml:"log_file" json:"log_file"`
	Debug            bool          `yaml:"debug" json:"debug"`
	AllowAnyServerIP bool          `yaml:"allow_any_server_ip" json:"allow_any_server_ip"` // 允许响应任意ServerIP
}

// NetworkConfig 网络配置
type NetworkConfig struct {
	Subnet           string   `yaml:"subnet" json:"subnet"`
	Netmask          string   `yaml:"netmask" json:"netmask"`
	StartIP          string   `yaml:"start_ip" json:"start_ip"`
	EndIP            string   `yaml:"end_ip" json:"end_ip"`
	DNSServers       []string `yaml:"dns_servers" json:"dns_servers"`
	DomainName       string   `yaml:"domain_name" json:"domain_name"`
	DefaultGateway   string   `yaml:"default_gateway" json:"default_gateway"`
	DNS1             string   `yaml:"dns1" json:"dns1"`
	DNS2             string   `yaml:"dns2" json:"dns2"`
	LeaseTime        int      `yaml:"lease_time" json:"lease_time"`
	RenewalTime      int      `yaml:"renewal_time" json:"renewal_time"`
	RebindingTime    int      `yaml:"rebinding_time" json:"rebinding_time"`
	BroadcastAddress string   `yaml:"broadcast_address" json:"broadcast_address"`
}

// Gateway 网关配置
type Gateway struct {
	Name        string   `yaml:"name"`
	IP          string   `yaml:"ip"`
	IsDefault   bool     `yaml:"is_default"`
	Description string   `yaml:"description"`
	DNSServers  []string `yaml:"dns_servers" json:"dns_servers"` // 网关专用DNS服务器
}

// MACBinding MAC地址绑定配置
type MACBinding struct {
	Alias    string `yaml:"alias"`
	MAC      string `yaml:"mac"`
	IP       string `yaml:"ip"`
	Gateway  string `yaml:"gateway"`
	Hostname string `yaml:"hostname"`
}

// HealthConfig 健康检查配置
type HealthConfig struct {
	Interval   time.Duration `yaml:"interval" json:"interval"`
	Timeout    time.Duration `yaml:"timeout" json:"timeout"`
	RetryCount int           `yaml:"retry_count" json:"retry_count"`
	Method     string        `yaml:"method" json:"method"` // ping, tcp, http
	TCPPort    int           `yaml:"tcp_port" json:"tcp_port"`
	HTTPPath   string        `yaml:"http_path" json:"http_path"`
}

// ScannerConfig 网络扫描器配置
type ScannerConfig struct {
	Enabled         bool          `yaml:"enabled" json:"enabled"`                   // 是否启用扫描器
	ScanInterval    int           `yaml:"scan_interval" json:"scan_interval"`       // 扫描间隔（秒）
	MaxConcurrency  int           `yaml:"max_concurrency" json:"max_concurrency"`   // 最大并发数
	PingTimeout     int           `yaml:"ping_timeout" json:"ping_timeout"`         // Ping超时时间（毫秒）
	InactiveTimeout int           `yaml:"inactive_timeout" json:"inactive_timeout"` // 非活跃超时时间（小时）
	StartIP         string        `yaml:"start_ip" json:"start_ip"`                 // 扫描起始IP
	EndIP           string        `yaml:"end_ip" json:"end_ip"`                     // 扫描结束IP
	AutoConflict    bool          `yaml:"auto_conflict" json:"auto_conflict"`       // 自动检测IP冲突
	ConflictTimeout time.Duration `yaml:"conflict_timeout" json:"conflict_timeout"` // 冲突IP超时时间
	LogLevel        string        `yaml:"log_level" json:"log_level"`               // 日志级别
}

// DeviceInfo 设备信息
type DeviceInfo struct {
	MAC         string    `json:"mac" yaml:"mac"`
	DeviceType  string    `json:"device_type" yaml:"device_type"` // 设备类型：Android、iPhone、Windows、MacOS、Linux等
	Model       string    `json:"model" yaml:"model"`             // 型号
	Description string    `json:"description" yaml:"description"` // 描述
	Owner       string    `json:"owner" yaml:"owner"`             // 所有者
	Hostname    string    `json:"hostname" yaml:"hostname"`       // 主机名
	Gateway     string    `json:"gateway" yaml:"-"`               // 配置的网关名称
	LastSeen    time.Time `json:"last_seen" yaml:"-"`             // 最后见到时间
	FirstSeen   time.Time `json:"first_seen" yaml:"-"`            // 首次见到时间
	IsActive    bool      `json:"is_active" yaml:"-"`             // 是否活跃
	StaticIP    string    `json:"static_ip" yaml:"-"`             // 静态IP地址（如果有的话）
	HasStaticIP bool      `json:"has_static_ip" yaml:"-"`         // 是否有静态IP绑定
}

// LoadConfig 加载配置文件
func LoadConfig(configPath string) (*Config, error) {
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %v", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("配置验证失败: %v", err)
	}

	return &config, nil
}

// Validate 验证配置有效性
func (c *Config) Validate() error {
	// 验证网络配置
	if _, _, err := net.ParseCIDR(c.Network.Subnet); err != nil {
		return fmt.Errorf("无效的子网配置: %s", c.Network.Subnet)
	}

	if net.ParseIP(c.Network.StartIP) == nil {
		return fmt.Errorf("无效的起始IP: %s", c.Network.StartIP)
	}

	if net.ParseIP(c.Network.EndIP) == nil {
		return fmt.Errorf("无效的结束IP: %s", c.Network.EndIP)
	}

	// 验证网关配置
	hasDefault := false
	for _, gw := range c.Gateways {
		if net.ParseIP(gw.IP) == nil {
			return fmt.Errorf("无效的网关IP: %s", gw.IP)
		}
		if gw.IsDefault {
			if hasDefault {
				return fmt.Errorf("只能有一个默认网关")
			}
			hasDefault = true
		}
	}

	if !hasDefault {
		return fmt.Errorf("必须配置一个默认网关")
	}

	// 验证MAC绑定配置
	for _, binding := range c.Bindings {
		if _, err := net.ParseMAC(binding.MAC); err != nil {
			return fmt.Errorf("无效的MAC地址: %s", binding.MAC)
		}
		if net.ParseIP(binding.IP) == nil {
			return fmt.Errorf("无效的绑定IP: %s", binding.IP)
		}
	}

	// 验证API监听地址
	if c.Server.APIHost != "" {
		// 如果配置了APIHost，验证其有效性
		if c.Server.APIHost != "0.0.0.0" && net.ParseIP(c.Server.APIHost) == nil {
			return fmt.Errorf("无效的API监听地址: %s", c.Server.APIHost)
		}
	}

	return nil
}

// GetDefaultGateway 获取默认网关
func (c *Config) GetDefaultGateway() *Gateway {
	for _, gw := range c.Gateways {
		if gw.IsDefault {
			return &gw
		}
	}
	return nil
}

// FindGatewayByName 根据名称查找网关
func (c *Config) FindGatewayByName(name string) *Gateway {
	for _, gw := range c.Gateways {
		if gw.Name == name {
			return &gw
		}
	}
	return nil
}

// FindBindingByMAC 根据MAC地址查找绑定
func (c *Config) FindBindingByMAC(mac string) *MACBinding {
	for _, binding := range c.Bindings {
		if binding.MAC == mac {
			return &binding
		}
	}
	return nil
}

// FindDeviceByMAC 根据MAC地址查找设备信息
func (c *Config) FindDeviceByMAC(mac string) *DeviceInfo {
	for i, device := range c.Devices {
		if device.MAC == mac {
			return &c.Devices[i]
		}
	}
	return nil
}

// AddOrUpdateDevice 添加或更新设备信息
func (c *Config) AddOrUpdateDevice(device DeviceInfo) {
	for i, existing := range c.Devices {
		if existing.MAC == device.MAC {
			c.Devices[i] = device
			return
		}
	}
	c.Devices = append(c.Devices, device)
}

// SaveConfig 保存配置到文件
func (c *Config) SaveConfig(configPath string) error {
	// 创建备份
	if err := c.BackupConfig(configPath); err != nil {
		return fmt.Errorf("创建备份失败: %v", err)
	}

	// 序列化配置为YAML
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %v", err)
	}

	// 写入文件
	if err := ioutil.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %v", err)
	}

	return nil
}

// BackupConfig 备份配置文件
func (c *Config) BackupConfig(configPath string) error {
	// 检查原文件是否存在
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil // 原文件不存在，无需备份
	}

	// 创建备份目录
	backupDir := filepath.Join(filepath.Dir(configPath), "config_backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("创建备份目录失败: %v", err)
	}

	// 生成备份文件名（带时间戳）
	timestamp := time.Now().Format("20060102_150405")
	backupFile := filepath.Join(backupDir, fmt.Sprintf("config_%s.yaml", timestamp))

	// 读取原文件
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("读取原配置文件失败: %v", err)
	}

	// 写入备份文件
	if err := ioutil.WriteFile(backupFile, data, 0644); err != nil {
		return fmt.Errorf("写入备份文件失败: %v", err)
	}

	return nil
}

// ValidateYAML 验证YAML格式
func ValidateYAML(yamlData string) error {
	var tempConfig Config
	if err := yaml.Unmarshal([]byte(yamlData), &tempConfig); err != nil {
		return fmt.Errorf("YAML格式错误: %v", err)
	}

	// 验证配置有效性
	if err := tempConfig.Validate(); err != nil {
		return fmt.Errorf("配置验证失败: %v", err)
	}

	return nil
}

// GetBackupList 获取备份文件列表
func GetBackupList(configPath string) ([]BackupInfo, error) {
	backupDir := filepath.Join(filepath.Dir(configPath), "config_backups")

	files, err := ioutil.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil // 备份目录不存在，返回空列表
		}
		return nil, fmt.Errorf("读取备份目录失败: %v", err)
	}

	var backups []BackupInfo
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".yaml") {
			// 解析时间戳
			name := file.Name()
			if strings.HasPrefix(name, "config_") && len(name) >= 21 {
				timestampStr := name[7:22] // config_20060102_150405.yaml -> 20060102_150405
				if timestamp, err := time.Parse("20060102_150405", timestampStr); err == nil {
					backups = append(backups, BackupInfo{
						Filename:  file.Name(),
						Timestamp: timestamp,
						Size:      file.Size(),
						Path:      filepath.Join(backupDir, file.Name()),
					})
				}
			}
		}
	}

	return backups, nil
}

// BackupInfo 备份文件信息
type BackupInfo struct {
	Filename  string    `json:"filename"`
	Timestamp time.Time `json:"timestamp"`
	Size      int64     `json:"size"`
	Path      string    `json:"path"`
}

// LoadConfigFromString 从字符串加载配置
func LoadConfigFromString(yamlData string) (*Config, error) {
	var config Config
	if err := yaml.Unmarshal([]byte(yamlData), &config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("配置验证失败: %v", err)
	}

	return &config, nil
}
