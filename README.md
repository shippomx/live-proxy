# live-proxy

把直播间号翻译成电视盒子能直接播放的流地址。

**为什么存在**：盒子的播放器只会播 URL，不会执行各平台的取流逻辑。而公网上的同类跳板
（如部署在 Vercel 上的）在国内被 DNS 污染 + IP 阻断——实测 `vercel.app` 被解析到
Facebook/Dropbox 的 IP，连 AAAA 记录都是伪造的 `face:b00c` 段，盒子根本连不上。
本服务跑在内网，全程只访问国内可直连的平台接口。

## 接口

```
GET /{平台}/{房间号}              → 302 跳转到最佳线路（播放器直接跟随）
GET /{平台}/{房间号}?format=json  → 返回全部线路
GET /{平台}/{房间号}?cdn=TX       → 指定线路
GET /{平台}/{房间号}?relay=1      → FLV 中转：由本服务拉流，上游断开自动续接（见下）
GET /healthz                      → 健康检查
GET /login                        → 扫码登录页（B站、斗鱼）
GET /                             → 302 到 /browse（默认进浏览页）
GET /browse                       → 浏览直播间、收藏、导入 m3u（见下）
GET /playlist.m3u                 → 收藏 + 导入的频道合并成一个播放列表
```

平台标识：`huya` `bilibili` `douyu` `kuaishou` `cc`

盒子里填 `http://<路由器IP>:9000/huya/11342394` 即可。

## 浏览与收藏

浏览器打开 `http://<路由器IP>:9000/browse`：按平台看推荐、分区、搜索，点 ☆ 收藏；
「我的频道」里可以按房间号添加、导入 m3u、删除频道。电视订阅 `http://<路由器IP>:9000/playlist.m3u`。

- 浏览只做了虎牙、B站、斗鱼（接口取自 dart_simple_live）；快手、CC 只能取流。
- 收藏和导入的频道存在 `-data` 目录下的 `live-proxy.db`（SQLite）。没设 `-data` 时只在内存里。
- **导入只新增不覆盖**：库里已有的频道（包括已收藏的直播间）名称、分组、台标都不动；
  同一个文件导两次，第二次全部计为"已存在"。
- 指向本服务的地址（`/{平台}/{房间号}`）按平台+房间号识别，不看主机名；输出时用电视访问
  `/playlist.m3u` 时的地址重新拼，换 IP 不用重新导入。导入的同一房间也算"已收藏"。
- `#EXTINF` 里的 `catchup-source` 等其他属性原样保留，组播频道的回看不受影响。
- 按房间号添加时会查一次房间信息：主播名作频道名，短号换成规范房间号（B站 `6` → `7734200`），
  避免同一房间收藏两次。
- B站分区列表用的是老版 `room/v3/area/getRoomList`：网页现用的 `second/getList`
  匿名请求恒返回 -352（2026-10-31 实测）。
- 不做鉴权，与 `/login` 一样只应在局域网内开放。

命令行导入：`curl -F file=@scdx.m3u http://<路由器IP>:9000/api/import`

## 中转模式（解决"播十分钟左右卡住"）

各平台 CDN 不允许一条 FLV 长连接一直推下去。实测（2026-10-30，成都电信）：

| 平台 | 上游断开时间 | 断开方式 |
|---|---|---|
| 斗鱼 | 300 秒整 | 正常关闭（与签名里的 `expire=300` 吻合） |
| B站 | 约 470 秒 | RST，断开前约 1 分钟码率已掉到 200kbps 左右 |

302 模式下播放器直连 CDN：ExoPlayer 把"正常关闭"当成直播结束停在最后一帧，
或者重连期间缓冲耗尽——表现就是"播十分钟左右卡住"。

`?relay=1` 时由本服务拉流转给播放器，上游断开或 10 秒无数据就重新取流（拿新签名），
把新一段 FLV 接到原连接上：丢掉重复的文件头和 onMetaData，丢掉第一个关键帧之前的帧，
补发 sequence header，时间戳整体平移接上一段。播放器那条连接从头到尾不断。

- 只对 FLV 线路生效；HLS（网易CC）本来就是分片拉取，不受此问题影响，仍走 302。
- 重新取流优先沿用原 CDN；20 秒内取不到、主播下播、或上游连续 3 次一连就断时结束响应，交给播放器自己重试。
- 想让所有 FLV 默认走中转而不用改节目单：启动参数加 `-relay`（`?relay=0` 可单独关闭）。
- 代价：流量经过路由器。单路 1–4 Mbps，R5S 转几路没有压力。

## 扫码登录（高画质）

浏览器打开 `http://<路由器IP>:9000/login`，用对应 App 扫码。登录后立即生效于取流，无需重启。

| 平台 | 不登录 | 登录后 |
|---|---|---|
| B站 | 原画被静默降级到超清/高清 | 原画 |
| 斗鱼 | 请求原画（`rate=0`）被静默降级成高清，最高只能拿到蓝光/超清 | 原画 |
| 虎牙 | 本来就是原画（源流） | 不需要登录 |

- 流程：B站参考 dart_simple_live（`passport.bilibili.com/x/passport-login/web/qrcode`），
  斗鱼取自 passport.douyu.com 登录页的 `app-login-all.js`（`/scan/generateCode` → `/japi/scan/auth` → 访问回调地址种 Cookie）。
- 斗鱼另支持**微信扫码**：走微信开放平台标准网页扫码（`open.weixin.qq.com/connect/qrconnect`，斗鱼 appid
  `wx8628eaef85e3a193`），长轮询 `lp.open.weixin.qq.com` 拿到 `wx_code` 后，用同一个 Cookie 罐访问斗鱼回调
  （`state` 与斗鱼种下的 `acf_devid` 绑定），所以服务端按二维码保存会话。微信须已关联斗鱼账号。
- Cookie 存在 `-data` 目录（容器里是 `/opt/live-proxy/data`，0600），重启自动加载并校验是否失效。
- 扫码后先校验能否真正登录，再替换正在使用的 Cookie：一次失败的扫码不会顶掉原本有效的登录。
- 页面与接口都不回显 Cookie，但局域网内的设备都能打开该页、扫码或退出登录。
- 斗鱼的 `rate=0` 是原画而不是"自动"——早先按"自动"跳过了它，于是登录后也拿不到原画。

## 状态码含义

错误码按"请求断在哪一层"划分，不压平成 500——未开播、房间号写错、平台接口改版
是三件完全不同的事，压成一个码会让排障失去方向。

| 码 | 含义 | 该查什么 |
|---|---|---|
| 302 | 正常，跳转到流地址 | — |
| 404 | 未开播 / 房间号无效 / 平台不支持 | 房间号是否写对，主播是否在播 |
| 401 | 需要登录凭据 | 配置对应平台的 Cookie |
| 502 | 上游请求失败或响应结构变了 | 平台接口是否改版；看日志里的具体错误 |

## 各平台取流方式

| 平台 | 请求数 | 签名 | 输出 | 备注 |
|---|---|---|---|---|
| 虎牙 | 1 | MD5 重算 anticode | FLV | 服务端下发的 `wsSecret` 不能直接用，密钥前缀藏在 `fm`（base64）里 |
| B站 | 1 | 无 | FLV + HLS | 取流不需要 wbi 签名。匿名会被静默降级到超清，已如实标注 |
| 斗鱼 | 4 | **必须跑 JS** | FLV | 见下 |
| 快手 | 1 | 无 | FLV | 页面 `__INITIAL_STATE__` 正则提取 |
| 网易CC | 2 | 无 | HLS | 各清晰度靠 `tc1/tc2/tc4` 后缀区分 |

### 斗鱼为什么必须跑 JS

签名函数 `ub98484234` 由斗鱼下发，是一层自解密 VM（常量表 → 位运算 → TEA 解密 →
eval 出真身），而且**每天重新生成**——TEA 常量、轮数表、运算次序都会变。今天把它
翻译成 Go，明天就签不出来，且失败形式是"本地算得出签名但服务端不认"，没有任何报错
指向根因。

但它不需要浏览器：整段脚本里 `window` / `document` / `navigator` 出现次数为 0，
外部依赖只有 `CryptoJS.MD5` 一个函数。所以用 goja（纯 Go JS 引擎）+ 五行 MD5 桩即可，
不必打包 47KB 的 crypto-js，更不需要无头浏览器。

JS 执行有 10 秒硬超时——执行的是对方每天下发的代码，不能假设它一定终止。

## 部署

```bash
HOST=r5s PORT=9000 ./deploy.sh
```

交叉编译 arm64 静态二进制 → 上传 → 装 procd 服务 → 健康检查。
目标机 overlay 分区不足 500MB，所以走单文件而非 Docker。

服务由 procd 托管：崩溃自动重启、开机自启、优雅停止。

### 容器方式（与 procd 方式二选一）

```bash
PORT=9001 ./deploy-docker.sh                     # 新端口并行试运行，不影响现有服务
REPLACE_NATIVE=1 RELAY=true ./deploy-docker.sh   # 正式切换：停掉 procd 服务，容器接管 9000
```

本机交叉编译 → 上传到路由器 `/opt/live-proxy` → 在路由器上 `docker build`（`FROM scratch`，
不拉任何基础镜像，国内拉 Docker Hub 不稳的问题不存在）→ `docker compose up -d`。

- 用 host 网络：R5S 的 dockerd 配了 `"iptables": false`，bridge 网络出网不一定通，
  且 docker-proxy 转发后日志里看不到真实的电视 IP。
- 以 nobody 运行、只读根文件系统、去掉全部 capabilities。
- 每次部署打时间戳 tag，旧镜像保留用于回滚。
- 切回 procd：`docker compose -f /opt/live-proxy/docker-compose.yml down && /etc/init.d/live-proxy enable && /etc/init.d/live-proxy start`

```bash
ssh r5s docker logs -f live-proxy     # 看日志
```

> 早期这里写过"overlay 分区不足 500MB 所以不用 Docker"。2026-10-30 复查 R5S 的 overlay
> 已有 60GB+ 可用，docker 数据目录在 `/opt/docker`，该约束已不成立。

```bash
ssh r5s /etc/init.d/live-proxy {start|stop|restart}
ssh r5s logread -e live-proxy          # 看日志
```

## 已知缺口

- **抖音未实现**。取流主路径需要 `a_bogus` 签名（SM3 + RC4 + 自定义 Base64 的纯函数，
  可用 Go 重写）。另有两条不需要签名的路径：`webcast.amemv.com/webcast/room/reflow/info`
  （需 19 位 room_id）和房间页 SSR 正则（需 webRid）。**有意暂缺**：验证这些路径需要
  伪造 `ttwid` 等反爬 cookie，本次未做端到端验证，不做未验证就上线的实现。

- **B站匿名只能拿到超清**。请求 `qn=10000`（原画）服务端会静默降级成 250，
  `accept_qn` 里却照常列着 10000。已在 `Quality` 字段标注"(已降级)"——
  不标注的话界面写着原画、播的却是超清，无从发现。要真原画需给 `Bilibili{Cookie: ...}`
  配置登录 Cookie（SESSDATA + bili_jct + buvid3），目前没有配置入口。

- **网易CC 只登记了 hs 一条拉流线路**。实测 `ali` 走 `alipullhls` 返回 403，
  `fws` 的域名没试出来。**有意如此**：没验证过的域名写进表里，用户点了会拿到 403，
  而错误显示为"上游拉流失败"，排查方向会偏向 CC 服务端故障而不是我们域名拼错。

- **快手跳过了"埋点激活 did"那一步**。原始实现声称不激活拿到的 `playUrls` 会是空的，
  实测（2026-09-06，房间 kglkgl520）不做那步同样拿到完整 playUrls。少一个无法验证
  是否生效的伪造埋点请求。若将来出现大面积空 playUrls，这里是第一个要回看的地方。

- **没有告警**。服务挂了 procd 会拉起，但平台接口改版导致的持续 502 无人知晓。
  目前只能靠 `logread` 事后查。

## 测试

```bash
go test ./...              # 含联网端到端测试
go test -short ./...       # 跳过联网
```

端到端测试不只验"拿到 URL"，而是真去拉流确认 FLV 魔数 / `#EXTM3U` 头——
报价能出不代表能交付，只验到中途就宣告可用是最常见的事故来源。

在播房间号从平台的推荐列表动态获取，不写死——写死的房间号会在那个主播下播的当天
开始永久失败，然后被当成"这个测试一直红，忽略它"，守卫就此失效。
