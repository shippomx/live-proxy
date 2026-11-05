# live-proxy

将直播平台的房间号转换成电视、机顶盒和播放器可直接播放的直播流地址。

## 功能

- 支持虎牙、哔哩哔哩、斗鱼、快手和网易 CC 取流
- 浏览直播间、搜索、收藏和导入 M3U 播放列表
- 生成统一的 `playlist.m3u` 供电视或播放器订阅
- 支持 B 站、斗鱼扫码登录以获取更高画质
- 支持 FLV 中转，上游断流时自动重新连接

## 部署

需要 Go 1.26.1 或更高版本。

### 直接运行

```bash
go build -o live-proxy .
./live-proxy -addr :9000 -data ./data
```

启用默认 FLV 中转：

```bash
./live-proxy -addr :9000 -data ./data -relay
```

### 部署到 OpenWrt

本机需要能够通过 SSH 访问目标路由器。脚本会交叉编译 ARM64 二进制，并安装为 procd 服务：

```bash
HOST=r5s PORT=9000 ./deploy.sh
```

### Docker 部署到 OpenWrt

```bash
HOST=r5s PORT=9000 RELAY=false ./deploy-docker.sh
```

如果要替换同端口上的 procd 服务：

```bash
HOST=r5s PORT=9000 RELAY=true REPLACE_NATIVE=1 ./deploy-docker.sh
```

## 使用

服务启动后，在浏览器打开：

```text
http://<服务器IP>:9000/browse
```

常用地址：

```text
http://<服务器IP>:9000/<平台>/<房间号>  获取直播流
http://<服务器IP>:9000/playlist.m3u       订阅收藏和导入的频道
http://<服务器IP>:9000/login              B站、斗鱼扫码登录
http://<服务器IP>:9000/healthz            健康检查
```

平台标识：`huya`、`bilibili`、`douyu`、`kuaishou`、`cc`。

例如：

```text
http://192.168.1.1:9000/huya/11342394
```

仅建议在可信的局域网中运行，本服务不提供访问鉴权。
