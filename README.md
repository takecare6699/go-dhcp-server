# 🌐 企业级DHCP服务器管理系统

一个功能完整的现代化DHCP服务器管理系统，采用Go语言开发，提供强大的Web管理界面、完整的API接口和企业级特性。

[![Go Version](https://img.shields.io/badge/Go-1.19+-00ADD8?style=flat&logo=go)](https://golang.org/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Tests](https://img.shields.io/badge/Tests-42/42_Pass-brightgreen.svg)](./dhcp_comprehensive_test.go)

## ✨ 核心特性

### 🚀 DHCP服务核心功能
- ✅ **标准DHCP协议** - 完整支持DHCP DISCOVER/OFFER/REQUEST/ACK流程
- ✅ **IP地址池管理** - 灵活的IP地址范围配置和自动分配
- ✅ **静态IP绑定** - 为特定MAC地址预留固定IP地址
- ✅ **租约管理** - 自动租约续期、过期清理和历史追踪
- ✅ **多网关支持** - 智能网关分配和故障切换

### 🎯 高级企业特性
- ✅ **网关健康检查** - 实时监控网关状态，支持ping/tcp/http检测
- ✅ **智能故障切换** - 网关故障时自动切换到备用网关
- ✅ **设备识别** - 自动识别设备类型和制造商信息
- ✅ **配置热重载** - 无需重启即可应用配置更改
- ✅ **配置备份恢复** - 自动配置备份和一键恢复功能

### 🖥️ 现代化Web管理界面
- ✅ **响应式设计** - 完美适配桌面和移动设备
- ✅ **多主题支持** - 亮色、粉色、暗色三种主题可选
- ✅ **实时数据更新** - 自动刷新统计数据和状态信息
- ✅ **直观的数据可视化** - 统计图表和状态指示器
- ✅ **完整的管理功能** - 设备、网关、绑定、配置一站式管理

### 🔧 API和集成
- ✅ **完整的REST API** - 支持所有管理操作的API接口
- ✅ **实时日志流** - WebSocket实时日志推送
- ✅ **配置验证** - 智能配置语法检查和验证
- ✅ **批量操作** - 支持设备批量导入和管理

### 🧪 质量保证
- ✅ **全面测试覆盖** - 42个测试用例，100%通过率
- ✅ **自动化测试脚本** - 一键运行所有测试用例
- ✅ **详细的测试报告** - 完整的测试文档和报告

## 🚀 快速开始

### 📋 系统要求

- **操作系统**: Linux、macOS、Windows
- **Go版本**: 1.19或更高版本
- **网络权限**: 需要绑定UDP 67端口（建议以管理员权限运行）
- **内存**: 最少64MB RAM
- **磁盘**: 最少100MB可用空间

### 🔧 安装部署

#### 1. 获取源代码
```bash
# 克隆仓库
git clone https://github.com/your-repo/dhcp-server.git
cd dhcp-server

# 或直接下载发布版本
wget https://github.com/your-repo/dhcp-server/releases/latest/download/dhcp-server-linux.tar.gz
tar -xzf dhcp-server-linux.tar.gz
```

#### 2. 编译安装
```bash
# 下载依赖
go mod tidy

# 编译
go build -o dhcp-server .

# 验证安装
./dhcp-server -version
```

#### 3. 配置系统
```bash
# 复制示例配置
cp config.yaml my-config.yaml

# 编辑配置文件
vim my-config.yaml
```

#### 4. 运行服务
```bash
# 使用默认配置（建议以root权限运行）
sudo ./dhcp-server

# 使用自定义配置
sudo ./dhcp-server -config my-config.yaml

# 后台运行
sudo nohup ./dhcp-server > dhcp.log 2>&1 &
```

#### 5. 访问管理界面
服务启动后，访问 `http://localhost:8080` 进入Web管理界面。

## ⚙️ 配置指南

### 🌐 基础网络配置

```yaml
# 服务器基础配置
server:
  interface: "eth0"           # 监听网络接口
  port: 67                   # DHCP服务端口
  lease_time: 24h            # 默认租期时间
  api_port: 8080             # Web管理端口

# 网络参数配置
network:
  subnet: "192.168.1.0/24"              # 子网CIDR
  netmask: "255.255.255.0"              # 子网掩码
  start_ip: "192.168.1.100"             # IP池起始地址
  end_ip: "192.168.1.200"               # IP池结束地址
  dns_servers:                          # DNS服务器列表
    - "8.8.8.8"
    - "8.8.4.4"
    - "1.1.1.1"
  domain_name: "local"                  # 默认域名
```

### 🚪 网关配置

```yaml
gateways:
  # 主网关
  - name: "main_gateway"
    ip: "192.168.1.1"
    is_default: true
    description: "主要出口网关"
    
  # 备用网关
  - name: "backup_gateway"
    ip: "192.168.1.2"
    is_default: false
    description: "备用出口网关"
    
  # 专用网关
  - name: "dmz_gateway"
    ip: "192.168.1.3"
    is_default: false
    description: "DMZ专用网关"
```

### 📱 设备管理

```yaml
devices:
  # 服务器设备
  - name: "web-server-01"
    mac: "aa:bb:cc:dd:ee:01"
    device_type: "Server"
    description: "Web服务器"
    
  # 工作站设备
  - name: "workstation-01"
    mac: "aa:bb:cc:dd:ee:02"
    device_type: "Workstation"
    description: "办公电脑"
```

### 🔗 静态IP绑定

```yaml
bindings:
  # Web服务器静态绑定
  - alias: "web-server"
    mac: "aa:bb:cc:dd:ee:01"
    ip: "192.168.1.10"
    gateway: "main_gateway"
    hostname: "web.local"
    
  # 打印机静态绑定
  - alias: "printer"
    mac: "aa:bb:cc:dd:ee:02"
    ip: "192.168.1.20"
    gateway: "main_gateway"
    hostname: "printer.local"
```

### 🏥 健康检查配置

```yaml
health_check:
  interval: 30s              # 检查间隔
  timeout: 5s                # 超时时间
  retry_count: 3             # 重试次数
  method: "ping"             # 检查方法: ping/tcp/http
  tcp_port: 80               # TCP端口（method=tcp时）
  http_path: "/"             # HTTP路径（method=http时）
```

## 🖥️ Web管理界面

### 🎨 主要功能页面

1. **📊 统计概览**
   - 实时IP池使用情况
   - 活跃租约统计
   - 网关健康状态
   - 系统运行时间

2. **📋 租约管理**
   - 查看所有活跃租约
   - 租约详情和历史
   - 手动释放租约
   - 租约转静态绑定

3. **📜 历史记录**
   - DHCP操作历史
   - 按时间/设备筛选
   - 详细操作日志
   - 数据导出功能

4. **🌐 网关状态**
   - 实时网关健康监控
   - 网关添加/编辑/删除
   - 故障切换日志
   - 健康检查配置

5. **📱 设备管理**
   - 设备信息维护
   - 设备类型识别
   - 批量设备导入
   - 设备搜索筛选

6. **⚙️ 配置管理**
   - 在线配置编辑
   - 配置语法验证
   - 配置备份恢复
   - 热重载应用

7. **🎨 界面配置**
   - 主题切换（亮色/粉色/暗色）
   - 自动刷新设置
   - 紧凑模式切换
   - 界面个性化

8. **📄 程序日志**
   - 实时日志查看
   - 日志级别筛选
   - 历史日志下载
   - 错误日志高亮

### 🎯 使用技巧

- **快速搜索**: 在任何列表页面都可以使用搜索框快速筛选
- **批量操作**: 选择多个项目进行批量删除或修改
- **自动刷新**: 启用自动刷新功能保持数据最新
- **主题切换**: 根据使用环境选择合适的界面主题
- **配置备份**: 修改重要配置前先创建备份

## 🔌 API接口文档

### 📡 REST API端点

| 方法 | 端点 | 描述 | 参数 |
|------|------|------|------|
| GET | `/api/health` | 服务器健康检查 | - |
| GET | `/api/stats` | 获取服务器统计信息 | - |
| GET | `/api/leases` | 获取所有租约 | `active`=true/false |
| GET | `/api/leases/active` | 获取活跃租约 | - |
| GET | `/api/leases/history` | 获取操作历史 | `limit`, `mac`, `ip` |
| GET | `/api/gateways` | 获取网关状态 | - |
| POST | `/api/gateways` | 添加网关 | JSON body |
| PUT | `/api/gateways/{name}` | 更新网关 | JSON body |
| DELETE | `/api/gateways/{name}` | 删除网关 | - |
| GET | `/api/devices` | 获取设备列表 | - |
| POST | `/api/devices` | 添加设备 | JSON body |
| PUT | `/api/devices/{name}` | 更新设备 | JSON body |
| DELETE | `/api/devices/{name}` | 删除设备 | - |
| GET | `/api/bindings` | 获取静态绑定 | - |
| POST | `/api/bindings` | 添加静态绑定 | JSON body |
| DELETE | `/api/bindings/{mac}` | 删除静态绑定 | - |
| GET | `/api/config` | 获取配置 | - |
| POST | `/api/config/save` | 保存配置 | JSON body |
| POST | `/api/config/reload` | 重载配置 | - |

### 📝 API响应示例

#### 获取活跃租约
```bash
curl http://localhost:8080/api/leases/active
```

```json
[
  {
    "ip": "192.168.1.100",
    "mac": "aa:bb:cc:dd:ee:01",
    "hostname": "web-server",
    "start_time": "2024-01-01T10:00:00Z",
    "lease_time": "24h0m0s",
    "remaining_time": "23h45m12s",
    "is_static": true,
    "gateway": "main_gateway",
    "gateway_ip": "192.168.1.1",
    "is_expired": false
  }
]
```

#### 获取服务器统计
```bash
curl http://localhost:8080/api/stats
```

```json
{
  "pool_stats": {
    "total_ips": 101,
    "dynamic_leases": 25,
    "static_leases": 8,
    "available_ips": 68,
    "utilization_rate": "32.67%"
  },
  "gateway_status": {
    "main_gateway": true,
    "backup_gateway": true,
    "dmz_gateway": false
  },
  "server_info": {
    "version": "v1.0.0",
    "start_time": "2024-01-01T08:00:00Z",
    "uptime": "2h30m15s"
  }
}
```

## 🧪 测试系统

### 🔍 测试覆盖

系统包含42个全面的测试用例，覆盖所有核心功能：

- **配置管理测试** (3个用例)
- **IP地址池测试** (5个用例)  
- **DHCP服务器测试** (7个用例)
- **网关健康检查测试** (7个用例)
- **HTTP API测试** (13个用例)
- **高级功能测试** (7个用例)

### 🚀 运行测试

```bash
# 运行所有测试用例
./run_tests.sh

# 运行Go单元测试
go test -v ./...

# 运行特定测试
go test -run TestDHCPServerCreation

# 生成测试报告
go test -coverprofile=coverage.out
go tool cover -html=coverage.out
```

### 📊 测试结果

```
==========================================
DHCP服务器综合测试报告
==========================================
测试开始时间: 2024-01-01 10:00:00
测试用例总数: 42
通过用例数: 42
失败用例数: 0
测试通过率: 100.00%
==========================================
```

## 🚀 生产环境部署

### 🔧 系统服务配置

创建systemd服务文件 `/etc/systemd/system/dhcp-server.service`:

```ini
[Unit]
Description=DHCP Server Management System
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/dhcp-server
ExecStart=/opt/dhcp-server/dhcp-server -config /etc/dhcp-server/config.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

启动服务:
```bash
# 重载systemd配置
sudo systemctl daemon-reload

# 启动服务
sudo systemctl start dhcp-server

# 设置开机自启
sudo systemctl enable dhcp-server

# 查看服务状态
sudo systemctl status dhcp-server
```

### 🔒 安全配置

#### 防火墙设置
```bash
# UFW防火墙
sudo ufw allow 67/udp
sudo ufw allow 8080/tcp

# iptables防火墙
sudo iptables -A INPUT -p udp --dport 67 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 8080 -j ACCEPT
```

#### 访问控制
建议通过反向代理（如Nginx）来控制Web界面访问，并启用HTTPS：

```nginx
server {
    listen 443 ssl;
    server_name dhcp.yourdomain.com;
    
    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;
    
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

### 📈 监控和日志

#### 日志配置
```bash
# 设置日志轮转
sudo tee /etc/logrotate.d/dhcp-server << EOF
/var/log/dhcp-server.log {
    daily
    missingok
    rotate 30
    compress
    notifempty
    create 644 root root
}
EOF
```

#### 监控脚本
```bash
#!/bin/bash
# 健康检查脚本
curl -f http://localhost:8080/api/health > /dev/null 2>&1
if [ $? -ne 0 ]; then
    echo "DHCP Server health check failed" | mail -s "DHCP Alert" admin@company.com
    systemctl restart dhcp-server
fi
```

## 🤝 开发贡献

### 🔧 开发环境设置

```bash
# 克隆项目
git clone https://github.com/your-repo/dhcp-server.git
cd dhcp-server

# 安装开发依赖
go mod tidy

# 运行开发服务器
go run main.go

# 代码格式化
go fmt ./...

# 静态检查
go vet ./...
```

### 📝 提交规范

- 使用清晰的提交信息
- 每个功能使用独立的分支
- 提交前运行所有测试
- 更新相关文档

### 🐛 问题报告

提交Issue时请包含：
- 操作系统和Go版本
- 详细的错误信息
- 重现步骤
- 相关配置文件

## 📄 许可证

本项目采用MIT许可证 - 详见 [LICENSE](LICENSE) 文件。

## 🆘 支持和帮助

- **文档**: 访问Web界面的"使用说明"页面获取详细帮助
- **Issues**: [GitHub Issues](https://github.com/your-repo/dhcp-server/issues)
- **讨论**: [GitHub Discussions](https://github.com/your-repo/dhcp-server/discussions)

## 🎯 路线图

- [ ] IPv6支持
- [ ] DHCP中继代理
- [ ] 集群部署支持
- [ ] Prometheus监控集成
- [ ] Docker容器化
- [ ] Kubernetes Helm Charts

---

<p align="center">
  <strong>🌟 如果这个项目对您有帮助，请给我们一个Star！</strong>
</p> 