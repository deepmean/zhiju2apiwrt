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
| `GET /health` | 运行时长、版本、token 状态、模型解析数、goroutine、堆占用 |
| `GET/POST /admin/tokens` | 查/换上游会话令牌（热生效，不用重启） |

为控制内存**未移植**：视频、音乐、语音、Anthropic 兼容层（`/v1/messages`）、工具调用协议、Web 面板。

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
- `model_id` 是**数字**，不是 `model_key`。网关按三级来源解析：
  `/v1/aimoss/repair/home_v2` 的 `model_group_key → model_group_id`（权威）、
  `/v1/account/get` 里「显示名 → id」回配、最后是 `models.map` 覆盖。
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

`GET /v1/models` 只列出**已解析出数字 id** 的图片模型，避免给出一个必然 404 的名字。

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
   手机号登录流程并立刻启用新令牌：

```sh
# ① 让上游发验证码到你手机
curl -X POST http://192.168.26.1:8080/admin/login/sms \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"phone":"136xxxxxxxx"}'

# ② 带上验证码登录；网关存下新令牌并立即使用（会自动刷新目录与价格缓存）
curl -X POST http://192.168.26.1:8080/admin/login \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"phone":"136xxxxxxxx","code":"123456"}'

# 随时查当前账号状态：余额、令牌健康、协议版本
curl http://192.168.26.1:8080/admin/session -H 'Authorization: Bearer sk-...'
```

多账号就往 `tokens.txt` 里多写几行，网关轮询使用；某个号被上游拒绝会自动跳过。

> 老实说：**路由器造不出新令牌**——要短信验证码，这一步必须有人参与（或由电脑侧常驻脚本代劳）。
> 本项目做的是让"失效"和"续期"都是可观测、可一键完成的，而不是静默地全站 401。

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

跨固件升级保留（`/etc/sysupgrade.conf`）：

```
/opt/zhiju2api/
/etc/init.d/zhiju2api
/etc/rc.d/S96zhiju2api
/etc/rc.d/K10zhiju2api
```

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

## 7. 使用

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

# 生图（升智-1：免费且无限，1K）
curl http://192.168.26.1:8080/v1/images/generations \
  -H 'Authorization: Bearer sk-...' -H 'Content-Type: application/json' \
  -d '{"model":"seedream4-image","prompt":"一只柯基犬在草地上奔跑，写实摄影","size":"1024x1024","image_size":"1K"}'

# 改图（OpenAI multipart 形状）
curl http://192.168.26.1:8080/v1/images/edits \
  -H 'Authorization: Bearer sk-...' \
  -F model=seedream4-image-edit -F 'prompt=把背景换成雪山，保留主体' -F image=@photo.png
```

任何 OpenAI 客户端都能直接接：把 base_url 指向 `http://<router>:8080/v1`，模型名用 `/v1/models` 里的。

### 实测结果（JCG-Q20 / OpenWrt 24.10.5 / mipsel_24kc）

| 链路 | 结果 |
| --- | --- |
| `/v1/models` | 39 个条目，57 个 id 已解析，名字+价格齐全 |
| 非流式对话 `molica41` | 200，3.0s，内容正确，`x-zhiju-estimated-cost: 0` |
| 流式对话 `molica41` | 200，首 token 1.3s，总 2.3s |
| 生图 `seedream4-image`（升智-1） | 200，32.9s，返回 PNG 直链 |
| 改图 `seedream4-image-edit` | 200，76.4s，**输出与输入不同**（背景草地→雪山，主体保留） |
| 进程内存 | 堆分配 ~0.9MB（`/health` 的 `mem_alloc_mb`） |


---

### 比例与分辨率（每个模型不一样，别猜）

支持的值由上游声明，网关照抄并**校验**：

- `/v1/models` 里每个图片模型都带 `aspect_ratios` / `default_aspect_ratio` / `image_sizes` / `default_image_size` / `prompt_limit`；
- `GET /v1/models/params` 是完整表（MJ 的 `stylize`/`chaos`/`quality`/`seed`/`no`/`tile`/`iw` 等旋钮连取值范围一起给）；
- 请求里可以显式传 `aspect_ratio` 与 `image_size`；不传就用模型默认值；**传了模型不支持的值会 400 并把可选值列出来**，而不是被上游静默忽略。

关键事实（实测）：**`image_size` 基本不是"可选档位"**，而是模型固有属性 ——

| 模型 | 比例 | 分辨率 |
|---|---|---|
| 升智-1 `seedream4` | `""`(自动) `2:3` `3:2` `3:4` `4:3` `4:5` `5:4` `1:1` `9:16` `16:9` `21:9` | **`1K` `2K`**（唯一双档） |
| 香蕉 Pro `nanobanana2` | 同上 | 只有 `2K` |
| 香蕉 2 `nanobananaFlash22k` | 另含 `1:4` `4:1` `1:8` `8:1` | 只有 `2K` |
| 升智-2 `gptimage2` | 默认 `auto` | 只有 `2K` |
| 升智-2.5创作/编辑版 | 默认 `auto` | 只有 `2K`（4K 是另一个模型 id） |
| Midjourney V7 `mj2` | 只有 `4:3` `1:1` `3:4` | 无档位概念（**默认禁用，见下**） |

所以 `IMAGE_SIZE` 配置项是**偏好**不是开关：模型支持才用，否则用模型自己的默认值。

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

## 8. 配置项

| 环境变量 / conf 键 | 默认 | 说明 |
| --- | --- | --- |
| `LISTEN` | `0.0.0.0:8080` | 监听地址 |
| `API_KEY` | *空* | 逗号分隔的 Bearer Token；为空则端口开放（启动会告警） |
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
| `GOGC` | `30`（代码内置） | 路由器上 CPU 比内存便宜 |

---

## 9. 资源占用与已知边界

- 单二进制、仅标准库、静态链接，mipsle 产物约 6MB。
- `GOGC=30` 内置默认：堆涨到存活量约 1.3 倍就回收，代价是 GC 更频繁。
- 生图闸门默认 1：上游同账号**同时只允许一个图片任务**，并发提交会拿到 `ret=10033`。
- 对话是**无状态**的：OpenAI 的 `messages` 会被折叠成「系统提示 + [对话历史] 块 + 最后一轮」
  发给上游的一次 `create`。好处是不用维护会话表、内存可控；代价是模型拿到的不是原生多轮。
- 未实现视频/音乐/语音，未实现工具调用。

---

## 10. 排障

| 现象 | 原因 / 处理 |
| --- | --- |
| 所有模型都 `price_unavailable` | `CHAT_VER` 不对（应为 `3.2.0`），或该模型名上游不认（认不出的名字是 `invalid_configuration`） |
| `ret=10033 您有任务正在执行` | 慢车道繁忙或已有图片任务在跑；保持 `FAST_MODEL=true`，把 `IMAGE_MAX_CONCURRENT` 留成 1 |
| 生图 `ret=10027 没有可用模型` | `model_id` 无效；查 `/v1/models` 或写进 `models.map` |
| 任务成功但结果 `图片生成失败` | 请求缺 `model_code` / `image_size`（本项目已按网页端形状补齐） |
| 401 / `登录状态无效` | JWT 过期或失效：重新登录后跑 `tools/zhiju_token_sync.py --push` |
| 改图上传报 OSS 错误 | 输入图超过 `MAX_UPLOAD_BYTES`，或对象授权过期（重试即可） |

---

## 11. 代码结构

| 文件 | 职责 |
| --- | --- |
| `main.go` | HTTP 服务、路由、鉴权、`/health`、`/admin/tokens`、并发闸门 |
| `config.go` | 配置加载（env + KEY=VALUE 文件） |
| `tokens.go` | 令牌池：解析 JWT、热加载、轮询选取、心跳、失效标记 |
| `upstream.go` | 上游客户端：分路由族头构造、`{ret,msg,data}` 信封、版本发现 |
| `models.go` | 模型目录与数字 id 三级解析、OpenAI 名字映射 |
| `chat.go` | `/v1/chat/completions` ↔ `/v2/ai/chat/turns`，SSE 事件映射 |
| `image.go` | `/v1/images/generations` ↔ draw/repair 创建 + 轮询 |
| `upload.go` | 改图输入图的 QR → OSS 直传流程 |
| `sse.go` | SSE 帧解析 |
| `tools/zhiju_token_sync.py` | 从浏览器 profile 同步令牌到网关 |
| `.github/verify_elf.py` | 产物 ELF 断言（含 soft-float） |

---

## 12. 免责

本项目只是把你自己浏览器里已有的会话搬到本地网关，供个人客户端调用。上游的账号条款、
额度和风控由你自己承担。
