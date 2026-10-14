# zhiju2api

把 [升智](https://agent.zhijuai.com)（底层是 molica 平台）网页版封装成 **OpenAI 兼容 API** 的极简网关：
对话（流式 SSE）+ 生图 + 改图，交叉编译为单一静态二进制，跑在 **OpenWrt 路由器**上
（目标机型 JCG-Q20 / MediaTek MT7621 / ramips / mipsel_24kc / 250MB RAM）。

**本地不需要 Go 工具链** —— 二进制全部由 GitHub Actions 云编译产出。

---

## 1. 接口范围

| 接口 | 说明 |
| --- | --- |
| `GET /v1/models` | 实时拉取上游目录，文本模型出裸名，图片模型出 `-image` / `-image-edit` 变体 |
| `GET /v1/models/prices` | **生成前**的服务端报价表：每个模型的标价 + 本账号实付 + 是否无限 |
| `GET /v1/models/params` | 每个模型支持的比例 / 分辨率 / 提示词上限（含 MJ 的 stylize 等旋钮范围） |
| `POST /v1/chat/completions` | `stream: true/false` 都支持；思考内容进 `reasoning_content` |
| `POST /v1/images/generations` | OpenAI 生图接口，返回 `data[].url`（或 `b64_json`） |
| `POST /v1/images/edits` | OpenAI multipart 改图接口（`image` + `prompt`） |
| `GET /v1/models/refresh` | 强制刷新模型目录缓存 |
| `GET /health` | 运行时长、版本、token 状态、模型解析数、goroutine、堆占用、**每日任务状态** |
| `GET/POST /admin/tokens` | 查/换上游会话令牌（热生效，不用重启） |
| `GET /admin/account` | 账号信息：昵称/mid、打码手机号、会员与到期、订阅、灵力余额、额度统计 |
| `GET/POST /admin/session` | 查令牌健康与剩余天数 |
| `POST /admin/login/sms` `…/code` `…/login` | 短信验证码续期（也支持写文件，见 §4） |
| `GET /admin/records` | 本网关创建的上游会话台账；`?upstream=1` 附带账号里其它会话以便对照 |
| `POST /admin/records/delete` | 按台账删除上游会话（`{"ids":…}` / `{"items":…}` / `{"all":true}`） |
| `GET /admin/tasks` `POST /admin/tasks/run` | 每日签到/积分任务的状态与手动触发（见 §9） |
| `GET /panel` | 浏览器面板（见 §7） |

为控制内存**未移植**：视频、音乐、语音、Anthropic 兼容层（`/v1/messages`）、工具调用协议。

---

## 2. 上游协议（实测结论，不是照抄注释）

所有上游请求打 `https://agent.zhijuai.com/quanling`，鉴权只靠一个头：

```
x-token-id: Bearer <JWT>
x-common-params: channel=molica_web_browser
plat: web_browser        # 生图/改图路由要 iOS
x-platform: web_browser
ver: <每个路由族不同>     # 见下
```

### 2.1 `ver` 是分路由族的，写错会伪装成"服务不可用"

这是本项目最容易踩的坑：

| 路由 | `ver` |
| --- | --- |
| `/v2/ai/chat/turns` | `3.2.0`（= `channelConfig.wholeApiVersion`） |
| `/v1/aimoss/*` | `2.2.11` |
| `/v1/aimoss/repair/*` | `1.1.10` |

把对话接口的 `ver` 写成 `2.2.11`，**不是报错**，而是**每个模型**都返回
`event: turn.failed / error_code: price_unavailable` —— 看起来像上游挂了，其实只是版本头不对。
网关启动时会调 `/v1/aimoss/cfg/list` 自动读 `wholeApiVersion`（`DISCOVER_VERSION=true`），
`CHAT_VER` 只是兜底。

### 2.2 对话：`POST /v2/ai/chat/turns`

请求体（字段一个都不能少，少 `client_request_id`/`operation` 同样会掉进 `price_unavailable`）：

```json
{
  "client_request_id": "<uuid4>",
  "operation": "create",
  "fast_model": true,
  "capacities": [],
  "ty": 0,
  "data": {"prompt": "你好", "content_type": "text", "urls": [], "seq": 0},
  "max_context_rounds": 5,
  "model_code": "molica41"
}
```

`operation: "create"` 时上游要求**不带** `session_id`；续聊才用 `follow_up` + `session_id` +
`active_leaf_message_id`。响应是命名事件 SSE：

```
turn.waiting / turn.started / model.started
message.delta   {"delta":"..."}      -> content
thinking.delta  {"delta":"..."}      -> reasoning_content
capability.started/completed/failed  -> 联网搜索等能力
turn.completed / turn.canceled / turn.failed {"error_code":"..."}
```

`fast_model: false` 时共享慢车道繁忙会返回 `ret=10033 您有任务正在执行`；网页端自己就是靠
切到快速模式绕过的，所以默认 `FAST_MODEL=true`。

### 2.3 生图：两条链路

| 模型族 | 创建接口 | 轮询接口 |
| --- | --- | --- |
| `draw`（`mj2` = Midjourney V7） | `POST /v1/aimoss/draw/create`（`plat: iOS`） | `POST /v1/aimoss/draw/message/page` |
| `repair`（香蕉 / GPT-Image / Seedream 等） | `POST /v1/aimoss/repair/async/create`（`plat: iOS`） | `POST /v1/aimoss/repair/web/list_msg` |

repair 的真实请求体（**来自网页端源码，字段名与直觉不同**）：

```json
{
  "fast_model": true,
  "urls": [],
  "prompt": "一只柯基犬在草地上奔跑",
  "model_id": 78,
  "model_code": "nanobanana",
  "session_id": "",
  "tag": "",
  "aspect_ratio": "1:1",
  "image_size": "2K"
}
```

- 传的是 `prompt`（不是 `input_text`），并且必须带 `model_code` + `image_size`；
  只发 `input_text` 会创建成功但结果是 `图片生成失败`。
- `model_id` 是**数字**，不是 `model_key`。网关按四级来源解析（先到先得）：
  `/v1/aimoss/cfg/list` 的 `model_key → model_id`（最全，57 个 id）、
  账号修复历史里的 `model_group_key → model_group_id`、`repair/home_v2` 的分组 id、
  `/v1/account/get` 里「显示名 → id」回配；`models.map` 覆盖一切，并且能**创造**上游没声明的模型。
- 图片族（索引里的 `-image` / `-image-edit`）来自快照的 draw/repair 列表，**加上** `cfg/list` 里
  `draw: true` 的条目 —— 后者是 4K 档唯一的来源（快照只有 2K），见 §8 的模型清单。
- 结果图 URL 出现在 `items[].messages{...}[].draw_content.url`。

### 2.4 改图：先上传再喂 URL

网页端把输入图上传到 OSS，再把对象 URL 交给模型。网关做了同样的事：

```
POST /v2/ai/uploads/qr/create                       -> qr_payload(内含一次性 qr_token)
POST /v2/ai/uploads/qr/authorize {qr_token}         -> mobile_token
POST /v2/ai/uploads/qr/authorize {mobile_token,...} -> {upload_url, fields}
POST <upload_url>  multipart 表单（fields 原样 + 二进制最后）
POST /v2/ai/uploads/qr/complete {finalize:true}
```

---

## 3. 模型命名

沿用 `qwen2api` 的约定：**裸名是对话模型，后缀选择同一模型的图片能力**。

```
molica41                 -> 对话（升智-5.1）
banana21-image           -> 生图
banana21-image-edit      -> 改图（走同一个 model_id，带上输入图）
mj2-image                -> Midjourney V7 生图
image-78                 -> 直接按数字 id 调用（排障用）
```

`GET /v1/models` 只列出**有 id 可用**的图片模型（id 为 0 也算，只要 `models.map` 显式给了它），
避免给出一个必然 404 的名字。清单见 §8 的图片模型表。

---

## 4. 会话保活（这一节是重点）

上游**没有** `refresh_token`，**没有** cookie，**没有**滑动续期：JWT 里是固定 `exp`
（约 100 天），过期后只能手机号 + 短信验证码重新登录。所以"自动刷新登录"在本项目里落地为四件事：

1. **热加载**：`tokens.txt` 每 `TOKEN_RELOAD_INTERVAL`（默认 5s）检查 mtime，改了立刻生效，**不用重启**。
2. **心跳**：每 `HEARTBEAT_INTERVAL`（默认 30 分钟）对每个 token 调一次 `/v1/account/get`；
   上游回 `ret=20001 登录状态无效` 就把该 token 标记为 `invalid`，`/health` 立刻可见，
   后续请求自动换用其它 token。
3. **一键同步**：`tools/zhiju_token_sync.py` 直接读浏览器 profile 里的 `SECRET_TOKEN`
   并 `POST /admin/tokens` 推给路由器 —— 你在电脑上登录一次，网关这边就永远是新的：

```sh
python3 tools/zhiju_token_sync.py --gateway http://192.168.26.1:8080 --key sk-... --push
# 常驻同步（例如 Windows 任务计划 / cron，每 10 分钟）
python3 tools/zhiju_token_sync.py --gateway http://192.168.26.1:8080 --key sk-... --watch 600
```

4. **路由器自己完成登录**：令牌彻底失效时，不必改文件、不必重启，网关可以自己走完
   手机号登录流程并立刻启用新令牌。**验证码有三条回填通道**，按自动化程度从高到低：

```sh
# ① 让上游发验证码（会记住这个号码，10 分钟内有效）
curl -X POST http://192.168.26.1:8080/admin/login/sms \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"phone":"136xxxxxxxx"}'

# ② 只回填验证码（给手机上的短信转发 App 用：它只拿得到验证码，拿不到号码）
curl -X POST http://192.168.26.1:8080/admin/login/code \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"code":"123456"}'

# ③ 或者写文件（连 JSON 都不用；网关只在有待处理登录时读，读完即删）
ssh router 'echo 123456 > /tmp/zhiju-sms-code'

# ④ 号码+验证码一起给（不依赖 ①，可独立使用）
curl -X POST http://192.168.26.1:8080/admin/login \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"phone":"136xxxxxxxx","code":"123456"}'

# 随时查当前账号状态：余额、令牌健康、剩余天数、协议版本
curl http://192.168.26.1:8080/admin/session -H 'Authorization: Bearer sk-...'
```

- 通道 ② 是给 **Android 短信转发 App**（SmsForwarder 之类）准备的：App 里配一条
  Webhook 指向 `/admin/login/code`，带 `Authorization` 头，正文 `{"code":"{{短信内容}}"}`，
  这样**验证码一到手机就自动完成续期**，全程无人值守；
- 通道 ③ 适合你人在 SSH 里的时候，一条命令搞定；
- 所有 `/admin/login*` 都在 API Key 之后，号码在日志与响应里只显示 `136****2016`。

多账号就往 `tokens.txt` 里多写几行，网关轮询使用；某个号被上游拒绝会自动跳过。

> 老实说：**路由器造不出新令牌**——要短信验证码，这一步必须有人参与（或由电脑侧常驻脚本、
> 手机短信转发 App 代劳）。本项目做的是让"失效"和"续期"都是可观测、可一键完成的，
> 而不是静默地全站 401。到期前 14 天起，日志每天一条预警。

---

## 5. 云编译（无需本地 Go）

工作流：[`.github/workflows/build.yml`](.github/workflows/build.yml)

| 触发 | 行为 |
| --- | --- |
| push `main` | gofmt + vet + build + test，刷新滚动预发布 `latest` |
| push `v*` 标签 | 同上，并发布正式 release |
| Pull Request | 仅测试与编译 |
| 手动 | Actions → build → Run workflow |

产物矩阵：`mipsle`（softfloat，目标机型）、`mips`、`arm64`、`armv7`、`amd64`。

编译后由 [`.github/verify_elf.py`](.github/verify_elf.py) **解析 ELF 断言**，任一条不符即失败：

| 断言 | 不满足的后果 |
| --- | --- |
| ELF32 | 内核拒绝执行 |
| 小端（mipsle） | 内核拒绝执行 |
| `Machine: MIPS` | 内核拒绝执行 |
| `.MIPS.abiflags` 的 `fp_abi == 3`（soft float） | **能启动，一碰浮点就崩** |
| 无 `/lib/ld*` 字样（静态） | 路由器上没有对应解释器 |

固定下载地址：

```sh
wget -O /tmp/zhiju2api.new \
  https://github.com/deepmean/zhiju2apiwrt/releases/download/latest/zhiju2api-linux-mipsle
```

---

## 6. 部署到 OpenWrt

| 路径 | 内容 |
| --- | --- |
| `/opt/zhiju2api/zhiju2api` | 静态二进制 |
| `/opt/zhiju2api/zhiju2api.conf` | 配置（`chmod 600`，含 API_KEY） |
| `/opt/zhiju2api/tokens.txt` | 会话令牌（`chmod 600`） |
| `/opt/zhiju2api/models.map` | 可选：`model_key=model_id` 覆盖 |
| `/etc/init.d/zhiju2api` | procd 服务脚本 |

```sh
mkdir -p /opt/zhiju2api
wget -O /opt/zhiju2api/zhiju2api \
  https://github.com/deepmean/zhiju2apiwrt/releases/download/latest/zhiju2api-linux-mipsle
chmod +x /opt/zhiju2api/zhiju2api

# 配置与令牌
cp /opt/zhiju2api/zhiju2api.conf.example /opt/zhiju2api/zhiju2api.conf   # 或手工创建
vi /opt/zhiju2api/zhiju2api.conf      # 改 API_KEY
vi /opt/zhiju2api/tokens.txt          # 贴 JWT
chmod 600 /opt/zhiju2api/zhiju2api.conf /opt/zhiju2api/tokens.txt

cp deploy/init.d-zhiju2api /etc/init.d/zhiju2api
chmod +x /etc/init.d/zhiju2api
/etc/init.d/zhiju2api enable
/etc/init.d/zhiju2api start
logread -e zhiju2api
```

跨固件升级保留：

```sh
ssh jcg 'sh -s' < tools/keep-on-sysupgrade.sh
```

脚本幂等地往 `/etc/sysupgrade.conf` 追加四行，并逐条断言路径确实存在（不存在的条目等于没保）：

```
/opt/zhiju2api/               # 二进制、conf、tokens、台账
/etc/init.d/zhiju2api
/etc/rc.d/S96zhiju2api
/etc/rc.d/K10zhiju2api
```

> 只写 `/opt/zhiju2api/` 是不够的：升级后文件回来了，但没有 init 脚本和 `rc.d` 链接，
> 服务不会自启 —— 这台路由器上其它服务（qwen2api、wb-proxy、mimo-proxy…）都是四条一起登记的。
>
> 保留的是**二进制本身**，所以升级固件后跑的还是升级前那版；要换版本就再 `deploy-router.sh` 一次。

> **时区**：init 脚本里设的是 `TZ=CST-8`（POSIX 形式）。OpenWrt 没有 tzdata，
> 写 `TZ=Asia/Shanghai` 会被 musl 静默退回 UTC。
>
> **procd 只在启动时读 conf**：改完 `zhiju2api.conf` 必须 `restart`；
> 改 `tokens.txt` 不用（热加载）。

### 升级（换二进制）

```sh
tools/deploy-router.sh jcg --addr 192.168.26.1
```

**默认只换二进制，不动路由器上的 `zhiju2api.conf`** —— 那份配置是你在设备上维护的
（API key、分辨率档、路径），部署覆盖它就会变成"没人改过却报 API key 无效"。
确实要连配置一起推才加 `--conf`。

脚本做了三件实测踩过的事：先 `stop` 再传（运行中的二进制会被占用，直接 scp 报
`dest open ... Failure`）、传完比对 **md5**、启动后打印 conf 里 `API_KEY` 的长度并检查 8080 是否在听。

---

## 7. 面板

打开 `http://<router>:8080/panel`（访问 `/` 会重定向到这里）。

面板 HTML 里**没有任何密钥**（谁打开都能看源码），所以进面板要输一次密码：

- **面板密码**（`PANEL_PASSWORD`，默认 `root`）—— 和 API Key 分开，改一个不影响另一个；
- 也可以带密码/API Key 直接打开：`http://<router>:8080/panel#key=root` —— **fragment 不会发给服务器**，也不进访问日志；
- 没填时页面**弹门禁**并聚焦输入框，而不是渲染成空壳（早期版本正是这样被误认为"面板坏了"）。

`/admin/*` 同时接受面板密码与 API Key，所以浏览器里不必存 API Key（脚本照旧用 Key）。
默认密码是 `root`，**别把 8080 暴露到公网**。

| 区块 | 内容 |
| --- | --- |
| 接入 | API Key 输入、网关地址、连接状态 |
| 登录并取回令牌 | 一键打开站点 + 书签（见 7.1） |
| 账号状态 | 余额、令牌状态、剩余天数、协议版本、已解析模型数 |
| 账号信息 | 昵称/mid、打码手机、会员与到期、订阅、灵力余额、额度统计（不限/已用完/总数） |
| 续期 | 手机号 → 验证码 → 完成登录；也可写 `/tmp/zhiju-sms-code` |
| 云端记录 | 本网关创建的上游会话，勾选删除 / 全删（见 10.2） |
| 每日签到 / 积分任务 | 状态 + 立即执行（见 9） |
| 模型 | 全模型表：真实名字、标价、**本账号实付**、额度、分辨率档、比例、上游 key/id、价格策略原文 |
| 手工替换令牌 | 粘 JWT 立即生效 |

### 7.1 一键取回令牌（书签）

站点是 HTTPS、网关是 HTTP，书签在站点页面上 `fetch` 网关会被浏览器按**混合内容**拦掉。
所以书签走**顶层导航**：带着令牌跳到面板（令牌放在 URL fragment，不经过服务器），
面板与网关同源，再正常推送。书签由面板**现场按自身地址生成**（点"拖我到书签栏"），
换路由器地址就重新拖一次。

### 7.2 复制即用的插件配置

模型表下方三个按钮，各自标明粘到哪里：

| 按钮 | 产物 | 粘到 |
| --- | --- | --- |
| 复制插件配置 JSON | 完整字段对象 | `cordis.patch.yml` 里 `config:` 的值（JSON 是合法 YAML），或照它填 GUI |
| 复制分辨率表 JSON | 只有比例表 | GUI 的「自定义分辨率表（JSON）」这一个框 |
| 复制 cordis.patch.yml 片段 | 整段（缩进已对好） | 直接替换现有 `image-gen` 条目 |

生成的内容带 `openaiCompatEditSizeMode: "selected"`：插件默认的 `auto` 在改图时**不发分辨率**，
网关只能回退到配置的 `IMAGE_SIZE`。这与"选 2K 出 1K"是同一类坑，见 §8 的比例与分辨率一节。

---

## 8. 使用

```sh
# 模型列表：真实名字 + 服务端报价（生成前就能看到花多少）
curl http://192.168.26.1:8080/v1/models -H 'Authorization: Bearer sk-...'
```

```json
{"id":"seedream4-image","name":"升智-1","upstream_key":"seedream4","upstream_id":148,
 "family":"repair","capability":"image","unlimited":true,"change_num":80,"effective_change_num":0}
{"id":"nanobanana2-image","name":"香蕉 Pro","upstream_key":"nanobanana2","upstream_id":97,
 "unlimited":false,"change_num":420,"effective_change_num":416}
{"id":"mj2-image","name":"Midjourney V7（高稳版）","upstream_key":"mj2","upstream_id":139}
```

- `change_num` = 上游标价（灵力），`effective_change_num` = **本账号实付**；
- `unlimited:true` 的模型（如升智-1）不扣灵力；
- `?prices=0` 跳过报价查询（只列名字，最快）；
- 完整报价表按实付价排序：`GET /v1/models/prices`。

每次对话/生图请求前，网关都会先问一次报价，写进日志并回在响应头上：

```
x-zhiju-list-cost: 10          # 标价
x-zhiju-estimated-cost: 0      # 本账号实付
```

设 `MAX_COST_PER_REQUEST=200` 可以再加一道闸门：超过就 402，并在错误信息里点名该类型最便宜的模型。

```sh
# 对话（流式）
curl -N http://192.168.26.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"model":"molica41","stream":true,
       "messages":[{"role":"user","content":"用一句话介绍你自己"}]}'

# 生图（升智-1：免费且无限。分辨率由请求的像素反推：2048 → 2K，1024 → 1K）
curl http://192.168.26.1:8080/v1/images/generations \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"model":"seedream4-image","prompt":"一只柯基犬在草地上奔跑，写实摄影","size":"2048x2048"}'

# 改图（OpenAI multipart 形状）
curl http://192.168.26.1:8080/v1/images/edits \
  -H 'Authorization: Bearer sk-...' \
  -F model=seedream4-image-edit -F 'prompt=把背景换成雪山，保留主体' -F image=@photo.png
```

任何 OpenAI 客户端都能直接接：把 base_url 指向 `http://<router>:8080/v1`，模型名用 `/v1/models` 里的。

### 实测结果（JCG-Q20 / OpenWrt 24.10.5 / mipsel_24kc）

| 链路 | 结果 |
| --- | --- |
| `/v1/models` | 39 个条目，56 个 id 已解析，名字+价格齐全 |
| 非流式对话 `molica41` | 200，3.0s，内容正确，`x-zhiju-estimated-cost: 0` |
| 流式对话 `molica41` | 200，首 token 1.3s，总 2.3s |
| 生图 `seedream4-image`（升智-1） | 200，32.9s，返回 PNG 直链 |
| 生图 2K `size=2048x2048` | 200，40s，**实测出图 2048x2048**（同 prompt 的 1K 请求为 1024x1024） |
| 生图 3:2 `size=2048x1365` | 200，39s，**2496x1664**（比例 1.500，连跑 3 次一致） |
| 改图 `seedream4-image-edit` | 200，76.4s，**输出与输入不同**（背景草地→雪山，主体保留） |
| 每日签到 | **+50 灵力**，一天一次（重复调用 `ret=2016 已经签到`） |
| 云端记录清理 | 6 条测试会话删除后，上游列表 72 → 66，网页端自己的会话不受影响 |
| 进程内存 | 堆分配 ~0.9MB（`/health` 的 `mem_alloc_mb`） |


---

### 比例与分辨率（每个模型不一样，别猜）

支持的值由上游声明，网关照抄并**校验**：

- `/v1/models` 里每个图片模型都带 `aspect_ratios` / `default_aspect_ratio` / `image_sizes` / `default_image_size` / `prompt_limit`；
- `GET /v1/models/params` 是完整表（MJ 的 `stylize`/`chaos`/`quality`/`seed`/`no`/`tile`/`iw` 等旋钮连取值范围一起给）；
- 请求可以显式传 `aspect_ratio` 与 `image_size`。

### 分辨率是怎么定的（这条踩过坑）

OpenAI 风格的客户端只能用**像素**表达分辨率（dsh 的 image-gen 插件把"2K"下拉变成
`size: "2048x2048"`）。所以网关**从像素反推档位**（长边 ÷ 1024 取整：1024→1K、2048→2K、4096→4K），
优先级是：

```
显式 image_size  →  请求像素推出的档位  →  配置的 IMAGE_SIZE  →  模型默认档位
```

早期版本只从像素推**比例**、把档位丢了，于是每次都落到配置的 `IMAGE_SIZE=1K` ——
这就是"选了 2K 出来还是 1K"。日志会把来源打出来：

```
image request seedream4: aspect=3:2 (requested pixels 2048x1365 (snapped)), image_size=2K (requested pixels 2048x1365)
```

**比例**同理：像素推出的比例（`2048:1365`）不是任何模型声明过的值，会先**吸附**到最近的
支持比例（这里 → `3:2`）。吸附是便利，不是限制。

至于"模型不支持的值"：默认（`STRICT_PARAMS=false`）**照发**，让上游自己给答案 —— 没有任何证据
表明上游会拒绝未声明的值，自造 400 只会掩盖它的真实行为；`STRICT_PARAMS=true` 才 400 并列出可选值。

### 图片模型清单（实付 = 本账号价格，来自服务端报价）

每个模型都另有 `-image-edit` 变体（改图走同一条链，多带一张输入图）。
**额度**列来自账号自己的 `models_remain`：有数字 = 账号可用，`不在额度表` = 上游没给这个 id 额度。

| OpenAI 模型名 | 上游展示名 | 标价 | 本账号实付 | 档位 | 额度 | 上游 key / id |
|---|---|---:|---:|---|---:|---|
| `seedream4-image` | 升智-1 | 80 | **0**（会员不限） | 1K / 2K | 2 | `seedream4` / 148 |
| `seedream452k-image` | 即梦4.5 | 160 | 152 | 2K | 2 | `seedream452k` / 101 |
| `banana31FLite-image` | 香蕉 2-Lite（快速版） | 200 | 200 | 1K | 2 | `banana31FLite` / 152 |
| `seedream502k-image` | 即梦5.0 | 240 | 240 | 2K | 2 | `seedream502k` / 126 |
| `gptimage2-image` | 升智-2 | 280 | 280 | 2K | 2 | `gptimage2` / 142 |
| `nanobananaFlash22k-image` | 香蕉 2 | 300 | 304 | 2K | 2 | `nanobananaFlash22k` / 132 |
| `gptimage25flare2k-image` | 升智-2.5创作版 | 304 | 304 | 2K | 2 | `gptimage25flare2k` / 172 |
| `gptimage25sunburst2k-image` | 升智-2.5编辑版 | 304 | 304 | 2K | 2 | `gptimage25sunburst2k` / 174 |
| `nanobananaFlash24k-image` | 香蕉 2（4K高清） | 350 | 352 | **4K** | 2 | `nanobananaFlash24k` / 133 |
| `gptimage25flare4k-image` | 全灵-2.5创作版（4k超清） | 352 | 352 | **4K** | 2 | `gptimage25flare4k` / 173 |
| `gptimage25sunburst4k-image` | 全灵-2.5编辑版（4k超清） | 352 | 352 | **4K** | 2 | `gptimage25sunburst4k` / 175 |
| `nanobanana2-image` | 香蕉 Pro | 420 | 416 | 2K | 2 | `nanobanana2` / 97 |
| `nanobanana4k-image` | 香蕉 Pro（4K超清） | 520 | **520** | **4K** | 2 | `nanobanana4k` / 98 |
| `banana21-image` | 香蕉2.1 | — | —（无价目） | 未声明 | — | `banana21` / 0 |
| `seedream454k-image` | 即梦4.5（4K超清） | — | —（无价目） | 未声明 | **不在额度表** | `seedream454k-draw` / 111 |
| `seedream503k-image` | 即梦5.0（3K超清） | — | —（无价目） | 未声明 | **不在额度表** | `seedream503k-draw` / 131 |
| `mj-image` | 绘画MJ-V7 | — | —（按次结算） | 无档位 | -1 | `mj` / 8 |
| `mj2-image` | Midjourney V7（高稳版） | — | —（按次结算） | 无档位 | — | `mj2` / 139 |

**"网页上没有"不等于无效**：图片创作选择器只列 12 个模型，而账号额度表里有 107 个 id。
`gptimage25flare4k`（升智-2.5创作 4K）就是例子 —— 选择器不列它，但账号额度是 2，可用。
反过来，`seedream454k-draw` / `seedream503k-draw` 只存在于选择器里、额度表里没有，**存疑**。
`mj`（id 8）额度写着不限，但实测任务会卡在 `waiting`（要用 `mj2` / 139）。

**4K 是独立模型，不是 2K 模型的档位**，而且更贵：

| 家族 | 2K 实付 | 4K 实付 | 差 |
|---|---:|---:|---:|
| 香蕉 Pro | 416 | 520 | +104 |
| 香蕉 2 | 304 | 352 | +48 |
| 全灵-2.5 创作版 | 304 | 352 | +48 |
| 全灵-2.5 编辑版 | 304 | 352 | +48 |

所以在插件里用 4K 要**换「模型」字段**（填 `nanobanana4k-image`），不是换分辨率下拉。
这些 4K 条目并不在上游快照的 draw/repair 列表里（那两张表只有 2K 变体），是网关从
`cfg/list` 补进索引的 —— 判据是"账号能用"，见 §14 的目录构建说明。

`banana21`（香蕉2.1）是另一种情况：快照里有它、名字就叫"香蕉2.1"，但**没有 `model_id`**，
网页端发 `model_id: 0`，所以只能靠 `models.map` 手工登记（见 §12）。

关键事实（实测）：**`image_size` 基本不是"可选档位"**，而是模型固有属性 ——

| 模型 | 比例 | 分辨率 |
|---|---|---|
| 升智-1 `seedream4` | `""`(自动) `2:3` `3:2` `3:4` `4:3` `4:5` `5:4` `1:1` `9:16` `16:9` `21:9` | **`1K` `2K`**（唯一双档） |
| 香蕉 Pro `nanobanana2` / 香蕉 2 `nanobananaFlash22k` | 香蕉 2 另含 `1:4` `4:1` `1:8` `8:1` | 只有 `2K`（4K 见上表，是另一个模型） |
| 升智-2 `gptimage2` | 默认 `auto` | 只有 `2K` |
| 升智-2.5创作/编辑版 | 默认 `auto` | 只有 `2K` |
| Midjourney V7 `mj2` | 只有 `4:3` `1:1` `3:4` | 无档位概念（**默认禁用，见下**） |

所以 `IMAGE_SIZE` 配置项是**偏好**不是开关：请求带像素（或显式 `image_size`）时以请求为准，
都没有才用它，模型不支持就退到模型默认值。

### 插件的分辨率表（`openaiCompatSizes`）

插件没配这张表时会退回内置默认：**只有 `1:1` / `3:2` / `2:3`，而且只有 1K 一档** ——
下拉里根本没有真正的 2K。表本身要满足插件校验：比例键必须是 `数字:数字`（不能有 `auto`/空），
档位键是数字或 `NK`，值是非空尺寸串。面板的"复制分辨率表 JSON"按上游声明直接生成，
"复制插件配置 JSON"还会带上 `openaiCompatEditSizeMode: "selected"`（否则**改图**不发分辨率、
回退到 `IMAGE_SIZE`）。

配好之后可以用 `node tools/verify/config_table_diff.mjs` 逐项 diff：既比对你的表与网关声明，
也检查每一档的像素是否落在它自己那一档（写错会让"2K"实际请求成 1K）。

### Midjourney 默认禁用（`MJ_ENABLED=false`）

MJ **不走灵力计量**：计费模块里没有它的价目行（getpolicy 恒回 `ret=500`），账号目录里也是
`remain_count=-1`。但**"没有灵力价目"不等于免费** —— 它是这个账号上最贵的一档，只是按次而不是
按灵力结算。

所以默认关掉：请求 `mj2-image` 会得到

```json
{"error":{"type":"model_disabled","message":"Midjourney is disabled (MJ_ENABLED=false). ..."}}
```

`/v1/models` 里 MJ 条目会带 `"enabled": false` 和 `disabled_reason`，`/v1/models/prices` 里带
`billing: "not metered in 灵力 ..."`。确实要用就显式打开 `MJ_ENABLED=true`。

```sh
# 看某个模型支持什么
curl -s http://192.168.26.1:8080/v1/models/params -H 'Authorization: Bearer sk-...' | jq '.data[] | select(.model_key=="seedream4")'

# 按比例生图（MJ 的比例参数键是 aspect，实测 4:3 出图 1024x772）
curl http://192.168.26.1:8080/v1/images/generations \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"model":"seedream4-image","prompt":"一只柯基犬","aspect_ratio":"16:9","image_size":"1K"}'

# MJ 的额外旋钮
-d '{"model":"mj2-image","prompt":"a red apple","aspect_ratio":"4:3","mj_params":{"stylize":250,"chaos":10,"seed":42}}'
```

## 9. 每日签到与积分任务

这些是 **App 侧**接口（网页端前端完全不调用它们，`psychic_sign` 只出现在 iOS 配置里），
但接受网页头：

| 路由 | 行为 |
| --- | --- |
| `POST /v1/aimoss/task/sign` | 签到：当天首次 `ret=1`，重复调用 `ret=2016 已经签到` |
| `POST /v1/aimoss/task/list` | 任务列表 `{task_list, all_task_max_integral, all_task_finish_status}` |
| `POST /v1/aimoss/task/finish` | 完成一个任务 |

实测：**签到一天一次，到账 +50 灵力**（流水里写"签到赠送"）。另外探测了 task / integral /
reward / box / account 五族约 50 个候选路由，**只有这三个存在**；本账号任务列表当前为空
（`all_task_finish_status=1`），所以 `finish` 的入参**未经实测** —— 网关会把任务项原文打进日志，
任务第一次出现时就能看到它要什么参数。

调度：

- 每天 `SIGNIN_AT`（默认 `00:05`，本地时区）跑一次；
- **启动后 20 秒也补跑一次** —— 幂等（重复调用返回"已经签到"），所以重启不会漏签；
- 同时扫任务列表，出现什么就做完什么；
- 手动触发：面板"立即执行"或 `POST /admin/tasks/run`；状态见 `GET /admin/tasks`。

> **时区是个坑**：`TZ=CST-8` 是 musl 认识的 POSIX 写法，**Go 不认**（Go 只读 IANA zoneinfo
> 文件，而 OpenWrt 没有 tzdata），进程会静默退回 UTC —— 签到就跑到 08:05，日志也全早 8 小时。
> 二进制内嵌了 tzdata，并按 `TZ_NAME`（默认 `Asia/Shanghai`）显式解析。

---

## 10. 计费对账与云端记录

### 10.1 报价 ≠ 实扣

`/v1/models/prices` 的 `effective_change_num` 是**计费模块的预测**，不是实际扣款。实测反例：

| 模型 | 报价 | 实际 |
| --- | --- | --- |
| `deepseek-base`（对话） | 0（界面标 free） | **每轮 -48 灵力** |
| `seedream4` 升智-1（生图） | 0 | 0（确实免费：前后余额未动） |
| `nanobanana2` 香蕉 Pro | 416 | 416 |
| `mj2` MJ-V7 | 无灵力价目 | **-720 灵力/次**（按次结算） |

所以面板**不显示"免费"徽章**：每个请求前后各读一次余额，把**报价和实扣两个数都记进台账**
（`cost_quoted` / `cost_actual`），日志里也能看到：

```
measured cost for seedream4: quoted=0 灵力, actual=0 灵力 (balance 233479 -> 233479)
```

上游另有一条独立消费流水（`POST /v1/integral/log/query`），带 `integral_change_num` 与
`change_desc`，是最终对账依据。注意它部分记录的 `integral_balance` 来自另一个池子，
**账号余额以 `/v1/integral/balance/query` 为准**。

### 10.2 云端记录清理

API 调用会在账号里留下会话（对话、生图、MJ）。删它们**不能按"最近 N 条"** —— 那会把你在
网页端自己的会话一起删掉。网关因此维护一份台账（`RECORDS_FILE`，默认
`/opt/zhiju2api/api-records.jsonl`），记录自己创建的每个会话。

- 面板"云端记录"区块：勾选删除 / 全删；
- `GET /admin/records` 只看台账；`?upstream=1` 列出账号里所有会话并标出哪些是网关建的
  （`tracked: true/false`），用于清理台账出现之前的旧记录；
- `POST /admin/records/delete`：`{"ids":[…]}`、`{"items":[{id,kind}]}`、`{"all":true}`；
  `all` **只删台账里的**，绝不动其它会话。

`tools/verify/clean_test_records.mjs` 是按**提示词精确匹配**清理测试残留的示例（默认只列出，
加 `--delete` 才删）。

---

## 11. 配置项

| 环境变量 / conf 键 | 默认 | 说明 |
| --- | --- | --- |
| `LISTEN` | `0.0.0.0:8080` | 监听地址 |
| `API_KEY` | *空* | 逗号分隔的 Bearer Token；为空则端口开放（启动会告警） |
| `PANEL_PASSWORD` | `root` | 面板密码；`/admin/*` 同时接受它和 API Key（见 §7）。空 = 关闭 |
| `UPSTREAM` | `https://agent.zhijuai.com` | 上游地址 |
| `TOKENS_FILE` | `/opt/zhiju2api/tokens.txt` | 会话令牌文件 |
| `MODEL_MAP_FILE` | `/opt/zhiju2api/models.map` | 模型 id 覆盖 |
| `CHAT_VER` / `AIMOSS_VER` / `REPAIR_VER` | `3.2.0` / `2.2.11` / `1.1.10` | 分路由族版本头 |
| `DISCOVER_VERSION` | `true` | 启动时从 `cfg/list` 读 `wholeApiVersion` |
| `FAST_MODEL` | `true` | 走快速车道，避免 `ret=10033` |
| `MAX_CONTEXT_ROUNDS` | `5` | 上游上下文轮数 |
| `CHAT_MAX_CONCURRENT` | `4` | 并发对话上限，超出 `429` |
| `IMAGE_MAX_CONCURRENT` | `1` | 并发生图上限（上游同账号一次只接一个任务） |
| `CHAT_TIMEOUT` / `IMAGE_TIMEOUT` | `5m` | 单请求总超时 |
| `POLL_INTERVAL` | `4s` | 生图轮询间隔 |
| `MAX_BODY_BYTES` | `8MiB` | 请求体上限，超出 `413` |
| `MAX_IMAGE_BYTES` | `8MiB` | `b64_json` 下载上限 |
| `MAX_UPLOAD_BYTES` | `10MiB` | 改图输入图上限 |
| `HEARTBEAT_INTERVAL` | `30m` | token 心跳 |
| `TOKEN_RELOAD_INTERVAL` | `5s` | 令牌文件检查间隔 |
| `MODEL_CACHE_TTL` | `30m` | 模型目录缓存 |
| `RECORDS_FILE` | `/opt/zhiju2api/api-records.jsonl` | 本网关创建的上游会话台账（见 §10.2） |
| `PROMPT_LIMIT_MODE` | `off` | 上游声明的 `prompt_limit` 怎么对待：`off` 忽略 / `warn` 记日志照发 / `enforce` 400 |
| `STRICT_PARAMS` | `false` | `false` 时比例/档位不合模型声明也**照发**（让上游给答案）；`true` 则 400 并列出可选值 |
| `SIGNIN_ENABLED` | `true` | 每日签到开关（见 §9） |
| `SIGNIN_AT` | `00:05` | 签到时间（本地时区） |
| `TZ_NAME` | `Asia/Shanghai` | 进程时区；二进制内嵌 tzdata，不依赖设备有 zoneinfo |
| `TZ_OFFSET_HOURS` | `8` | `TZ_NAME` 认不出来时的兜底偏移 |
| `SMS_CODE_FILE` | `/tmp/zhiju-sms-code` | 验证码文件（见 §4） |
| `GOGC` | `30`（代码内置） | 路由器上 CPU 比内存便宜 |

---

## 12. 资源占用与已知边界

- 单二进制、仅标准库、静态链接，mipsle 产物约 6MB（内嵌 tzdata 后约 6.5MB）。
- `GOGC=30` 内置默认：堆涨到存活量约 1.3 倍就回收，代价是 GC 更频繁。
- 生图闸门默认 1：上游同账号**同时只允许一个图片任务**，并发提交会拿到 `ret=10033`。
- 对话是**无状态**的：OpenAI 的 `messages` 会被折叠成「系统提示 + [对话历史] 块 + 最后一轮」
  发给上游的一次 `create`。好处是不用维护会话表、内存可控；代价是模型拿到的不是原生多轮。
- 未实现视频/音乐/语音，未实现工具调用。

### 上游行为（不是本项目的 bug，但会影响你）

| 现象 | 事实 |
| --- | --- |
| 声明的 `prompt_limit=1000` | **上游自己不强制**：1980 与 8000 字符的提示词都被接受，网页端图片页也从不读这个字段（只有视频页读，还带 `|| 1000` 兜底）。所以网关默认 `PROMPT_LIMIT_MODE=off`，不自造上限 |
| 比例偶尔不生效 | 同一个 3:2 请求：经网关连跑 3 次都是 2496x1664；**直连上游**出现过 2048x2048（比例被忽略）和直接失败。即上游自身抖动，网关侧稳定。要"不符就重试"需额外下载图片量尺寸并**再花一次任务额度**，默认不做 |
| 报价与实际扣费不一致 | 见 §10.1 |
| MJ 无灵力价目 | `getpolicy` 恒回 `ret=500`，但它是账号上最贵的一档（按次结算），默认禁用 |

### 有些模型没有数字 id（`banana21` / 香蕉2.1）

上游 snapshot 会声明一个模型却**不给 `model_id`**，网页端就发 `model_code=banana21` + `model_id=0`：

```json
{"model_key":"banana21","model_name":"香蕉2.1","model_type":"repair"}   // 没有 model_id 字段
```

而 `cfg/list`、`repair/params`、账号额度列表、`repair/home_v2` 里**都没有它**。网关原来要求
`id > 0` 才算解析成功，于是它进了目录却被静默丢弃 —— 面板里看不到，`/v1/models` 里也没有。

在 `models.map` 里写一行即可（id 填 `0`，与网页端一致）：

```
banana21=0     # 香蕉2.1，268 灵力/次（计费名"使用香蕉2.1修图"）
```

之后它以 `banana21-image` / `banana21-image-edit` 出现。这类"无 id 模型"只能手工登记，
因为没有任何一处声明能提供它的 id。

---

## 13. 排障

| 现象 | 原因 / 处理 |
| --- | --- |
| 所有模型都 `price_unavailable` | `CHAT_VER` 不对（应为 `3.2.0`），或该模型名上游不认（认不出的名字是 `invalid_configuration`） |
| `ret=10033 您有任务正在执行` | 慢车道繁忙或已有图片任务在跑；保持 `FAST_MODEL=true`，把 `IMAGE_MAX_CONCURRENT` 留成 1 |
| 生图 `ret=10027 没有可用模型` | `model_id` 无效；查 `/v1/models` 或写进 `models.map` |
| 任务成功但结果 `图片生成失败` | 请求缺 `model_code` / `image_size`（本项目已按网页端形状补齐） |
| 401 / `登录状态无效` | JWT 过期或失效：重新登录后跑 `tools/zhiju_token_sync.py --push` |
| 改图上传报 OSS 错误 | 输入图超过 `MAX_UPLOAD_BYTES`，或对象授权过期（重试即可） |
| 选了 2K 还是出 1K | 客户端只能用像素表达分辨率；网关从长边反推档位（2048→2K）。先看日志的 `image_size=… (requested pixels …)`；插件侧确认 `openaiCompatSizes` 里真有 2K 档，改图还要 `openaiCompatEditSizeMode=selected` |
| 日志时间比路由器早 8 小时 / 签到跑到 08:05 | Go 不认 POSIX `TZ=CST-8`（OpenWrt 无 tzdata）。已内嵌 tzdata，检查 `TZ_NAME`；另外 `procd_set_param env` 是**设置**语义，写两行会覆盖，两个变量要写在一行 |
| 云端记录删不掉 | `{"all":true}` 只删台账里（网关建的）会话；旧记录用 `?upstream=1` 列出后按 `{"items":[…]}` 指定删 |
| 面板打开一片空白 | 没填 API Key 时现在会弹门禁；带 key 打开用 `#key=…`，或用 `tools/verify/panel_states.mjs` 复现三态 |

---

## 14. 代码结构

| 文件 | 职责 |
| --- | --- |
| `main.go` | HTTP 服务、路由、鉴权、`/health`、并发闸门、时区解析 |
| `config.go` | 配置加载（env + KEY=VALUE 文件） |
| `tokens.go` | 令牌池：解析 JWT、热加载、轮询选取、心跳、失效标记 |
| `upstream.go` | 上游客户端：分路由族头构造、`{ret,msg,data}` 信封、版本发现、会话列举/删除 |
| `models.go` | 模型目录与数字 id 三级解析、OpenAI 名字映射 |
| `chat.go` | `/v1/chat/completions` ↔ `/v2/ai/chat/turns`，SSE 事件映射、实扣计量 |
| `image.go` | `/v1/images/generations` ↔ draw/repair 创建 + 轮询；档位反推与吸附 |
| `params.go` | 每个模型的比例/分辨率/提示词上限表与吸附逻辑 |
| `pricing.go` | 服务端报价、`x-zhiju-*-cost` 头、模型列表增强 |
| `records.go` | 本网关创建的上游会话台账（报价 vs 实扣也记在这里） |
| `tasks.go` | 每日签到与积分任务调度、`/admin/tasks` |
| `admin.go` | `/admin/*`：令牌、账号、短信登录、云端记录 |
| `panel.go` | 内嵌的浏览器面板（单页、无外部依赖、不含密钥） |
| `upload.go` | 改图输入图的 QR → OSS 直传流程 |
| `imageurl.go` | 上游 `content.urls` 的两种形状（字符串 / 对象）兼容 |
| `sse.go` | SSE 帧解析 |
| `tools/zhiju_token_sync.py` | 从浏览器 profile 同步令牌到网关 |
| `tools/keep-on-sysupgrade.sh` | 幂等登记 `/etc/sysupgrade.conf`，固件升级不丢部署 |
| `tools/verify/` | 对真网关的端到端检查（见 §15） |
| `.github/verify_elf.py` | 产物 ELF 断言（含 soft-float） |

---

## 15. 验证

- **单元测试**：`go test ./...`（CI 跑；失败时会把测试输出打成**注解**，因为 job log 需要 token，
  否则只能看到一句 `exit code 1`）。
- **端到端检查**：`tools/verify/` —— 对着真网关跑，覆盖面板三态、插件配置可粘贴性、
  分辨率档位是否真生效、计费对账、台账删除。用法见 `tools/verify/README.md`。
- **产物断言**：`.github/verify_elf.py` 校验 ELF class / 端序 / MIPS soft-float / 无 `PT_INTERP`，
  自身带 9 个用例的自测。

---

## 16. 免责

本项目只是把你自己浏览器里已有的会话搬到本地网关，供个人客户端调用。上游的账号条款、
额度和风控由你自己承担。
