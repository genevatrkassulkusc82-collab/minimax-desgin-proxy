# minimax-desgin-proxy

（工程内部名 `minimax-2api`）MiniMax 海螺（MiniMax Design / Hailuo03）**多账号 2API 服务**：
把多个海螺账号的 JWT 组成账号池，对外暴露 OpenAI 兼容的 REST API——
**异步视频生成**（Sora 风格 `/v1/videos`）与**图片生成**（Images 风格 `/v1/images/generations`、`/v1/images/edits`），
内部完成选号、计价预检、素材上传、提交、轮询、下载、失败换号、取消、断点恢复、
token 自动续期与余额巡检，并内嵌中文 Web 管理台。

单二进制 + SQLite + go:embed 前端，无任何其他运行时依赖。

## 特性总览

| 能力 | 说明 |
|---|---|
| 多账号池 | 深链/JWT 批量导入（网页端无 deviceID 的 token 亦可）、按 user_id/device_id 去重、每账号独立设备指纹档案（终身不变）、分组备注 |
| 账号生命周期 | 12h 错峰自动续期、15min 余额巡检、401 先续期重试再标失效、单账号/批量健康测试（失效自动禁用）、免费试用一键领取 |
| 账号恢复 | 生成官方深链 `minimax-hub-cn://auth-callback?accessToken=...`，在装有 MiniMax Design 的机器上一键把账号还原回官方客户端（可选先续期） |
| 视频生成 | MiniMax-H3 / H3-Max（768P/2K/480P，4-15s，首尾帧/参考图≤9/参考视频≤3/参考音频≤3），Sora 风格异步任务 API，幂等键 |
| 图片生成 | nano_banana（1K/2K/4K，十档比例，参考图≤10），OpenAI Images 兼容同步返回（url/b64_json，n≤4），multipart edits |
| 预计算额度选号 | 选号前先计价（参数指纹缓存 10min，同规格零额外云调用），只在「余额 ≥ 预估×安全系数」的账号里选；余额耗尽才全局摘除，跑不起单任务仅本任务排除 |
| 任务管道 | 11 阶段状态机、worker 池（热更）、每账号并发上限、401/1033/5xx 分类换号（≤3 次）、SUBMIT_UNKNOWN 防重复扣费、重启断点恢复、60s reconcile 兜底、取消传播（云端 404 视为已取消） |
| 兼容生态 | new-api 等 OpenAI 风格网关可直接挂渠道；infinite-canvas 无限画布开箱即用（含 seconds/size 字段别名垫片）；管理台「在线测试」页可直接提交并预览 |
| 管理台 | 仪表盘/账号/任务/在线测试/密钥/设置 六 Tab，中文界面，任务事件时间线，媒体内嵌预览（Range 拖动） |
| 安全 | 默认仅监听 127.0.0.1、Host 校验防 DNS Rebinding、API Key sha256+常量时间比较、登录失败限流、token `json:"-"` 不外泄、日志脱敏 |

> ## 风险声明（必读）
> 1. 本项目基于对 MiniMax Design 桌面客户端的**逆向工程协议**构建，未经官方授权。使用本系统
>    **很可能违反 MiniMax / 海螺的服务条款**，可能导致账号被风控、限权或封禁，甚至法律追责。
>    使用者必须自行评估并承担全部风险。
> 2. 仅可接入你**本人拥有或获得书面授权**的账号；禁止接入他人账号、禁止批量注册/接码、禁止商业转售。
> 3. 建议专号专用：接入本系统的账号不要再在官方桌面端/网页端登录（避免登录态互踢与行为异常）。
> 4. 仅供学习交流。

---

## 1. 编译

环境要求：Go 1.23+（go.mod 声明 `go 1.26.0`，`GOTOOLCHAIN=auto` 时会自动使用缓存的 1.26 工具链）。

```bash
cd /d/minimax-2api
PATH="/d/Go/go/bin:$PATH" go build -o minimax-2api.exe .
```

依赖仅两个（均已进 go.sum）：`modernc.org/sqlite`（纯 Go，无 CGO）、`golang.org/x/crypto`（bcrypt）。

## 2. 启动与配置

```bash
./minimax-2api.exe                 # 默认 -config config -db（取配置文件 db_path）
./minimax-2api.exe -config /path/to/config -db /path/to/data.db
```

- 默认监听 `127.0.0.1:8787`（仅本机可访问，含 Host 校验中间件防 DNS Rebinding）。
- Web 管理台：`http://127.0.0.1:8787/web`，默认账号 **admin / admin123**。
- 配置文件 `config/config.json`（不存在时自动生成默认值，30s 热加载）：

| 配置项 | 默认 | 说明 |
|---|---|---|
| listen_addr | 127.0.0.1:8787 | 监听地址 |
| region | domestic | domestic=design.minimaxi.com/hailuoai.com；overseas=design.minimax.io/hailuoai.video |
| cloud_gateway_url / account_api_url | 空 | 显式覆盖域名（优先于 region） |
| version_code | 3.0.11 | 公共参数版本号，与逆向的桌面端一致 |
| db_path / video_dir | data/minimax-2api.db / data/videos | 数据库与成片目录 |
| admin_username / admin_password | admin / admin123 | 管理台初始凭据 |
| worker_count | 4 | 任务 worker 数（管理台可热更，≤32） |
| per_account_concurrency | 2 | 每账号并发任务上限（热更） |
| poll_interval_sec / poll_backoff_factor / poll_max_interval_sec | 5 / 1.3 / 20 | 轮询节奏（间隔热更） |
| poll_max_wait_min | 90 | 单任务轮询总时限（分钟） |
| balance_safety_factor | 1.2 | 余额需 ≥ 预估积分 × 该系数才派单（热更） |
| renew_interval_hour / balance_interval_min | 12 / 15 | token 续期 / 余额巡检周期（自动错峰+抖动） |
| reconcile_interval_sec | 60 | 卡死任务兜底重排周期 |

运行期四个热更项（poll_interval_sec / worker_count / per_account_concurrency / balance_safety_factor）
以数据库 settings 表为准，可在管理台「设置」页修改；配置文件值仅作首次种子。

## 3. 导入账号

官方无账号密码 API，登录只能在 Web 页完成，登录成功后浏览器会尝试跳转深链：

```
minimax-hub-cn://auth-callback?accessToken=<JWT>
```

浏览器会提示"无法打开该地址"，此时**从地址栏复制完整 URL**（或直接复制裸 JWT）。

导入步骤：

1. 打开管理台 `http://127.0.0.1:8787/web`，用 admin/admin123 登录；
2. 「账号管理」→「+ 导入账号」，把深链 URL 或 JWT 粘贴进文本框（**每行一个，支持批量**），可填备注前缀；
3. 服务端逐行：解析 accessToken → base64 解码 JWT 提取 `user.deviceID` 生成设备档案
   （os_name/cpu_core_num/device_memory 随机定格、终身不变，保证指纹一致）→
   调 `user/info` 验证并回填 user_id/用户名 → 调 `credit/balance` 拉余额；
4. 同一 device_id 重复导入 = 更新 token（不会重复建档）。

后台会自动：每 ~12h 续期 token（错峰）、每 ~15min 巡检余额、401 时先续期重试再标失效、
余额耗尽自动摘除（充值回升后自动恢复）。

### 网页端 token（无 deviceID）导入行为

桌面端登录签发的 JWT payload 携带 `user.deviceID`，导入时按 device_id 建档/去重。
**网页端**签发的 token 常见不携带 deviceID，此时导入流程为：

1. 先用临时设备档案调 `user/info` 验证，成功则按 `realUserID` 去重——同一账号重复导入
   只更新 token，已建档的设备指纹保持不变；
2. 验证失败（网络问题/无效 token）退化为按 token 全文去重；
3. 全新账号生成 UUID v4 设备档案建档（等价官方桌面端首启 `generateDeviceID` 行为）。

云端请求遵循桌面端语义：token 解析不出 deviceID 时整体省略 `device_id`/`uuid` 公共参数
（而非发送空值），网页端 token 可正常参与任务调度。

### 账号恢复链接（还原回官方桌面端）

导入的逆过程：把池内账号还原到官方 MiniMax Design 客户端。「账号管理」每行的
**「恢复链接」**按钮（或 `GET /api/accounts/{id}/restore-link[?renew=1]`）生成官方深链：

```
minimax-hub-cn://auth-callback?accessToken=<当前存储的JWT>
```

- 在**装有 MiniMax Design 的机器**上打开（弹窗内「打开链接」或复制到浏览器地址栏），
  桌面端会拦截该深链 → 调 `user/info` 验证 token → 直接以该账号登录，无需再走网页登录；
- 弹窗展示 token 有效期（JWT exp）与用户 ID；token 临期或桌面端验证不过时，
  勾选「生成前先续期」再重新生成（服务端先调 renewal 换新 token 再拼链接）；
- region=overseas 时协议名自动切为 `minimax-hub://`；
- **安全**：链接包含完整登录凭据，端点仅 session 认证可达，日志只打 token 前 8 位，
  请勿把链接发给他人或贴到公开场所。

## 4. 创建 API Key 并调用 2API

管理台「API 密钥」→「+ 创建密钥」，明文 `sk-...` **只显示一次**。

### 提交视频任务

```bash
curl -X POST http://127.0.0.1:8787/v1/videos \
  -H "Authorization: Bearer sk-xxxxxxxx" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-001" \
  -d '{
    "model": "hailuo-03",
    "prompt": "一只猫在月球上跳舞，电影感光影",
    "duration": 5,
    "resolution": "768P",
    "ratio": "16:9",
    "generate_audio": true
  }'
# → 202 {"id":"task_...","object":"video","status":"queued","model":"MiniMax-H3","created_at":1757...}
```

图生视频（首帧 / 首尾帧 / 参考素材，元素支持 http(s) URL 或 data: URI）：

```bash
curl -X POST http://127.0.0.1:8787/v1/videos \
  -H "Authorization: Bearer sk-xxxxxxxx" -H "Content-Type: application/json" \
  -d '{
    "model": "MiniMax-H3",
    "prompt": "让画面中的人物挥手",
    "first_frame_image": "https://example.com/first.png",
    "reference_images": ["data:image/png;base64,iVBORw0K..."]
  }'
```

参数限制（对齐 MiniMax-H3 云端硬校验，违规直接 400）：

- `model`：`MiniMax-H3`（别名 hailuo-03）/ `MiniMax-H3-Max`（别名 hailuo-03-max）
- `duration`：H3 为 4-15，Max 为 5-15（整数秒，默认 5）
- `resolution`：H3 支持 `768P|2K`；Max 支持 `480P|768P`（默认 768P）
- 参考图 ≤9、参考视频 ≤3、参考音频 ≤3；首尾帧模式与参考素材**互斥**
- 素材大小：图 jpeg/png ≤10MB、视频 ≤50MB、音频 ≤15MB

### 查询 / 下载 / 取消 / 列表

```bash
# 任务详情（status: queued|processing|succeeded|failed|cancelled）
curl http://127.0.0.1:8787/v1/videos/task_xxxx -H "Authorization: Bearer sk-xxx"

# 成片下载：本地文件存在则流式返回（支持 Range），否则 302 到 CDN 直链
curl -L -o out.mp4 http://127.0.0.1:8787/v1/videos/task_xxxx/content -H "Authorization: Bearer sk-xxx"

# 取消（POLLING 中会同步调云端 cancel）；已终态返回 409
curl -X DELETE http://127.0.0.1:8787/v1/videos/task_xxxx -H "Authorization: Bearer sk-xxx"

# 列表（仅本 Key 的任务）
curl "http://127.0.0.1:8787/v1/videos?page=1&limit=20&status=processing" -H "Authorization: Bearer sk-xxx"

# 模型列表
curl http://127.0.0.1:8787/v1/models -H "Authorization: Bearer sk-xxx"
```

`Idempotency-Key` 请求头：同一 Key 下重复提交返回原任务（200 + `Idempotent-Replay: true`）。

### new-api 对接说明

new-api（及同类 OpenAI 风格网关）接入视频渠道时：

- 渠道类型选「自定义/OpenAI 兼容」，Base URL 填 `http://127.0.0.1:8787`，Key 填 `sk-...`；
- 模型名映射：`hailuo-03` → 本服务 `MiniMax-H3`，`hailuo-03-max` → `MiniMax-H3-Max`
  （本服务对常见别名做了归一，直接填别名亦可）；
- 本服务响应格式对齐 Sora 风格视频对象（`object:"video"`，`GET /v1/videos/{id}` 查询，
  `GET /v1/videos/{id}/content` 取片），与 new-api 的视频任务转发链路兼容；
- 任务为异步 202 语义，轮询/回调由 new-api 侧负责。

### infinite-canvas（无限画布）对接说明

[tigerowo/infinite-canvas](https://github.com/tigerowo/infinite-canvas) 可直接把本服务当作
OpenAI 兼容渠道使用，视频/图片创作链路已实测打通：

1. 系统设置 → 聊天方式 → 添加渠道：**协议选默认 openai**（不要选 metaso，那是秘塔专用通道）；
2. BaseURL 填 `http://<本机IP>:8787`（其会自动补 `/v1`，直接填 `.../v1` 亦可）；
   infinite-canvas 跑在 Docker 里时不能用 127.0.0.1，要用宿主机 IP 或 `host.docker.internal`；
3. APIKey 填本服务密钥页创建的 `sk-...`；模型名手动填 `MiniMax-H3` / `MiniMax-H3-Max`
   （图片模型 `nano_banana_2_flash`）；
4. 兼容细节：本服务接受其 Sora 风格请求体别名（`seconds`/`n_seconds`→duration，
   `size`→resolution+ratio 自动吸附十档比例）；任务状态词表（queued/processing/succeeded/failed）
   与其 NormalizeVideoTaskStatus 完全对齐；取片走 `GET /v1/videos/{id}/content`（Bearer 鉴权代理）。

未覆盖端点（其对话助手/Agent/TTS 功能依赖）：`/v1/chat/completions`、`/v1/responses`、
`/v1/audio/speech`——见文末「已知缺口」。

### 图片生成 2API（nano_banana / OpenAI Images 兼容）

图片生成走云端 `nano_banana` 后端（v2 异步任务，与视频共用状态机管道，落盘
`data/images/{task_id}.{png|jpg|webp}`）。对外暴露 OpenAI Images 兼容接口：

```bash
# 文生图（同步等待，成功直接返回图片 URL；n 1-4 每张独立云任务）
curl -X POST http://127.0.0.1:8787/v1/images/generations \
  -H "Authorization: Bearer sk-xxxxxxxx" -H "Content-Type: application/json" \
  -d '{
    "model": "nano_banana_2_flash",
    "prompt": "a cute orange cat sitting on a windowsill, watercolor",
    "size": "1024x1024",
    "resolution": "1K",
    "n": 1
  }'
# → 200 {"created":1757...,"data":[{"url":"http://127.0.0.1:8787/v1/images/task_xxxx/content"}]}

# 图生图（参考图 https URL 或 data URI，≤10 张、单图 ≤7MB）
curl -X POST http://127.0.0.1:8787/v1/images/generations \
  -H "Authorization: Bearer sk-xxx" -H "Content-Type: application/json" \
  -d '{"model":"general-image-2","prompt":"make it night","image":"https://example.com/ref.png","aspect_ratio":"3:2"}'

# 直接取图片二进制（本地文件流式 / 302 CDN 直链）
curl -L -o out.png http://127.0.0.1:8787/v1/images/task_xxxx/content -H "Authorization: Bearer sk-xxx"
```

参数（对齐官方 nano_banana 硬限制，违规直接 400）：

- `model`：`nano_banana_2_flash`（默认；别名 `general-image-2` / `gpt-image-nano` 均映射到它）
- `prompt`：必填，≤10000 字符
- `n`：1-4（每张图一个独立云任务，各自计价）
- `size`：OpenAI 尺寸 → `aspect_ratio` 映射（`1024x1024`→`1:1`、`1536x1024`→`3:2`、
  `1024x1536`→`2:3`，另兼容 256/512/1792 档）；或直接传 `aspect_ratio`（**优先于 size**）
- `aspect_ratio`：`16:9 / 9:16 / 1:1 / 4:3 / 3:4 / 3:2 / 2:3 / 5:4 / 4:5 / 21:9`，
  `auto`/空 = 自动
- `resolution`：`1K`（默认）/ `2K` / `4K`
- `response_format`：`url`（默认，返回本服务 content 绝对地址）/ `b64_json`（返回 base64）
- `image` / `images`：图生图参考（http(s) URL 或 data: URI，≤10 张、单图 ≤7MB，
  客户端解析为 data URI 内嵌提交体，不走 files/upload）

同步/异步语义：

- 请求**同步等待**内部任务管道完成，上限 **5 分钟**；全部成功 → 200 OpenAI 格式
  `{created,data:[{url|b64_json}]}`；任一失败/取消 → 500 `image_generation_failed`
  （`error.task_ids` 可继续查询）；**5 分钟未完成 → 202** `{status:"processing",task_ids}`，
  用 `GET /v1/images/{task_id}` 轮询（`object:"image"`，成功带 `url`/`width`/`height`）。
- `GET /v1/images/{task_id}/content`：图片二进制（本地文件流式，Content-Type 按扩展名；
  本地缺失则 302 到 CDN 直链）。`DELETE /v1/images/{task_id}`：取消。
- 提交体内嵌客户端生成的 `idempotency_key`（建任务时固化、重试复用），
  云端幂等去重防重复扣费；提交窗口网络中断置 `SUBMIT_UNKNOWN` 保守终态，绝不自动重试。
- 计价用 `media_type:"image"`（`char_count`=prompt 长度、`ref_count`=参考图数）。
- 后端为**注册表模式**（`imageBackends`）：本期只实现 `nano_banana`，
  其他厂商（openai/seedream/kling/qwen/…）按同构三路径注册即可扩展。
- `GET /v1/models` 已追加图片模型条目；`/v1/videos*` 端点只返回视频任务，
  图片任务经 `/v1/images*` 访问（互不串台）。

## 5. 管理台：在线测试 / 模型目录 / 账号测试

### 在线测试（Web 管理台 Tab）

「在线测试」页可直接在管理台提交视频/图片生成任务，无需创建 API Key。任务来源标记为
`admin_test`（任务列表新增「来源」列区分 API / 在线测试、「媒体」列区分 视频 / 图片），
顶部「媒体类型」切换视频/图片，使用步骤：

1. **选择模型**：下拉以任务管道支持的模型为主（MiniMax-H3 / MiniMax-H3-Max，
   与 `/v1/videos` 校验完全一致）；远端目录中管道不支持的模型追加为灰色
   「仅展示」项，不可选择；
2. **选择账号**：默认「自动选号」（走选号器过滤+打分）；也可钉住指定账号测试。
   钉账号任务**不换号**：并发槽满时排队等待；账号不可用（不存在/禁用/token 失效/
   冷却中/余额耗尽）时任务直接 `FAILED`，`error_code=pinned_account_unavailable`；
3. **填写参数**：prompt（实时字数提示，≤10000）、duration（随模型联动 H3=4-15 /
   Max=5-15 秒）、resolution（联动 H3=768P|2K / Max=480P|768P）、ratio
   （adaptive/16:9/9:16/1:1/4:3/3:4/21:9）、生成音频开关、首帧图 URL、尾帧图 URL、
   参考图 URLs（每行一个，≤9 张，与首尾帧模式互斥）；
4. **估算积分**：调用云端 calculate-cost，显示预计消耗与所用账号当前余额
   （无可用账号时返回 409 提示）；
5. **提交测试**：任务卡片实时展示状态徽章 + 进度条 + 预计剩余秒（每 3s 轮询
   `GET /api/tasks/{id}`）；成功后内嵌 `<video>` 预览与下载按钮；失败显示
   error_code/error_msg，可一键「重试」（重试保留来源与钉住的账号）；
6. **远端模型目录**：折叠面板展示云端 `GET /api/v1/models/config` 的全部
   videoModels（id/显示名/描述/HOT 徽章/promptMaxLength），服务端按账号内存缓存
   10 分钟，面板带手动刷新按钮（强制拉新）；无账号或拉取失败时自动降级展示
   内置 MiniMax-H3 / H3-Max 信息并提示原因（接口绝不 500）。

**图片模式**（媒体类型切到「图片」）：表单精简为 模型（nano_banana_2_flash）+
提示词 + 比例 aspect_ratio（auto/16:9/…/21:9）+ 分辨率（1K/2K/4K）+ 参考图 URLs
（图生图，≤10 张、单图 ≤7MB）；时长/音频/首尾帧等视频专属字段自动隐藏。「估算积分」
走 `media_type:"image"` 计价；「提交测试」建图片任务（`media_type=image`），任务卡片
成功后内嵌 `<img>` 预览与下载；任务列表「预览」按钮对图片任务弹出 `<img>`。

### 账号健康测试

- **单账号测试**（账号行「测试」按钮）：`user/info` token 验证 + `credit/balance`
  余额 + 试用活动状态三连，刷新账号状态/余额/用户名后弹窗展示 token 状态、
  用户 ID、用户名、余额、试用信息（claimed/claimable/freeCount/remainingCount/
  activityActive）与耗时；
- **批量测试**（工具栏「批量测试」）：对所有 enabled 账号**串行**测试，账号间隔
  500ms 防风控；token 明确失效（401/403）的账号**自动禁用**（结果表中标红说明），
  网络类失败只报告不禁用，避免误伤。

### 管理端新增端点（均需 session 认证）

| 端点 | 方法 | 说明 |
|---|---|---|
| `/api/models/supported` | GET | 管道支持模型清单及 duration/resolution/ratio 取值范围 |
| `/api/models/remote?account_id=&refresh=1` | GET | 云端模型目录；缺省用第一个 active 账号，按账号缓存 10 分钟；无账号/失败降级 `{models:[内置],source:"builtin",error}` |
| `/api/test/generate` | POST | 在线测试提交；body 同 `POST /v1/videos`，另加可选 `account_id`（钉账号）；202 `{id,status:"queued",...}` |
| `/api/estimate-cost` | POST | `{model,duration,resolution,ratio,generate_audio,account_id?}` → `{estimated_credits,balance,account_id}`；无可用账号 409 |
| `/api/accounts/{id}/test` | POST | 单账号健康测试三连 |
| `/api/accounts/test-all` | POST | 批量测试全部 enabled 账号（串行 + 500ms 间隔，token 失效自动禁用） |

任务对象新增 `source`（`api`/`admin_test`）与 `pinned_account_id` 字段，
`/api/tasks` 列表与详情均返回；旧库启动时自动做防御式加列迁移
（`PRAGMA table_info` 检查 + `ALTER TABLE ADD COLUMN`，幂等可重复执行）。

## 6. 任务状态机（内部）

视频任务：

```
QUEUED → [选号前预估] → PRECHECK(余额复核) → UPLOADING(素材→CDN) → SUBMITTING(提交)
      → POLLING(5s×1.3退避≤20s，上限90min) → RETRIEVING(取直链)
      → DOWNLOADING(落盘 data/videos/{task_id}.mp4) → SUCCEEDED
```

**选号前预估（预计算额度）**：任务出队后、首次选号前先预估所需积分（写入
`credits_estimated`），让选号器的余额过滤（`余额 ≥ 预估 × balance_safety_factor`）
从第一次选号就生效，避免"先选中余额不足的账号 → 预检失败 → 换号"的浪费轮次：

- 同规格（视频：模型+分辨率+时长；图片另含提示词长度+参考图数）的计价结果按参数
  指纹缓存 10 分钟，连续提交同规格任务**零额外云调用**；
- 缓存未命中时借钉住账号或任一 active 账号的 token 调一次 `calculate-cost`
  （计价为模型级价格，跨账号基本一致，会员价差异由实时余额复核兜底）；
  预估失败不阻断任务，退回"选中后 PRECHECK 计价"原路径；
- 重启恢复/重试的任务直接复用已持久化的 `credits_estimated`；
- PRECHECK 在预估已知时只复核该账号**实时余额**（省一次计价调用）；
- 余额不足分级处置：余额真正耗尽（≤0）才标 `empty`（全局摘除直到充值/刷新）；
  只是跑不起本任务的账号仅为本任务排除并刷新缓存余额——它仍可接更便宜的任务，
  且刷新后的余额让后续任务在选号阶段就能正确过滤它。

图片任务（media_type=image，nano_banana v2；同骨架但**跳过 RETRIEVING**——查询响应
直接含 image_url/width/height；UPLOADING 把参考图解析为 data URI 内嵌提交体而非上传）：

```
QUEUED → [选号前预估] → PRECHECK(media_type=image 余额复核) → UPLOADING(参考图→data URI，≤7MB/张)
      → SUBMITTING(v2 提交，内嵌 idempotency_key) → POLLING(同视频节奏)
      → DOWNLOADING(落盘 data/images/{task_id}.{png|jpg|webp}，按 magic bytes 定扩展名) → SUCCEEDED
```

失败/换号/恢复（视频图片共用）：

```
失败分支：FAILED(终态) / CANCELLED(终态) / SUBMIT_UNKNOWN(提交结果不明，绝不自动重试，
         对外映射 failed+error.code=submit_unknown，管理端可人工复核后「重试」)
换号规则：401/403→续期一次→仍失败标 token_expired 换号；1033/5xx→账号冷却60s换号；
         余额耗尽(≤0)→标 empty 换号；余额不足以跑本任务→仅本任务排除该账号；
         账号尝试 ≤3 次。钉账号任务(pinned_account_id)不换号：
         账号不可用→FAILED(pinned_account_unavailable)，账号级失败→FAILED(pinned_account_failed)。
取消传播：云端 cancel 返回 404(任务已不存在) 视为取消成功；轮询遇云端 failed 时
         若已请求取消则按 CANCELLED 收敛（避免误标失败）。取消置终态后管道立即结束
         （outcomeTerminal），不会再推进后续阶段覆盖终态。
重启恢复：非终态任务按阶段恢复；SUBMITTING 一律转 SUBMIT_UNKNOWN；
         reconcile 每 60s 兜底重排 updated_at 超时的无人持有任务。
```

## 7. 项目结构

```
main.go              入口/中间件链(hostGuard→bodyLimit→auth→mux)/go:embed web
config.go            config.json 加载+默认值+30s热加载；RuntimeSettings(settings表热更)
database.go          SQLite DSN pragma + 全部表 schema + 防御式加列迁移(migrate) + settings 键值
minimax_client.go    云协议防腐层：公共参数/双头鉴权/视频&图片端点/模型目录/宽容数字(flex)
account_manager.go   导入(深链/JWT/设备档案)、验证、续期、余额、选号器、健康测试(单/批)
task_worker.go       视频任务状态机 + worker池 + 钉账号 + 重启恢复 + reconcile
task_worker_image.go 图片任务阶段管道(nano_banana v2，跳 RETRIEVING)
api_video.go         对外视频 2API(/v1/videos*，API Key 认证，幂等)
api_images.go        对外图片 2API(/v1/images*，OpenAI Images 兼容，同步等待≤5min)
api_admin.go         管理 API(/api/*，session 认证；含在线测试/模型目录/计价/账号测试)
auth.go              session(登录限流/DB持久化) + API Key(sk-前缀/sha256/常量时间比较)
scheduler.go         token续期(12h错峰)/余额巡检(15min错峰)/reconcile(60s)
web/                 内嵌管理台(vanilla JS，六 Tab：仪表盘/账号/任务/在线测试/密钥/设置)
```

## 8. 安全说明

- 默认仅监听 127.0.0.1；Host 校验中间件拒绝非本机 Host 头（防 DNS Rebinding）。
- 请求体限制：/api/* 32MB、/v1/videos 与 /v1/images 64MB（素材 data URI）、其余 8MB。
- 登录失败限流：同 IP+用户名 5 次失败锁 15 分钟；session token 与 API Key 均只存 sha256。
- 账号 token 明文落库（SQLite 本地文件），接口层 `json:"-"` 绝不外泄；日志只打 JWT 前 8 位。
- 云端数字字段（余额/计价/剩余秒/状态码/试用计数）经 flexFloat/flexInt 宽容解析，
  兼容 string/number 双形态（官方网关同源语义），杜绝解析型崩溃。
- 若需公网部署，请自行加 TLS 反代与访问控制，并修改默认密码。

## 9. 对外 API 速查（/v1/*，Authorization: Bearer sk-...）

| 端点 | 方法 | 说明 |
|---|---|---|
| `/v1/videos` | POST | 提交视频任务 → 202 `{id,object:"video",status:"queued"}`；支持 `Idempotency-Key`、Sora 别名（seconds/n_seconds/size）、模型别名（hailuo-03 等） |
| `/v1/videos/{id}` | GET | 任务详情：status(queued/processing/succeeded/failed/cancelled)、progress、seconds、resolution、width/height/size、error{code,message}、estimated_remaining_seconds、credits_used |
| `/v1/videos/{id}/content` | GET | 成片：本地文件流式（支持 Range）/ 缺失时 302 CDN 直链 |
| `/v1/videos` | GET | 本 Key 任务列表（page/limit/status） |
| `/v1/videos/{id}` | DELETE | 取消（排队/轮询中有效；已终态 409） |
| `/v1/images/generations` | POST | 图片生成（OpenAI Images 兼容）：model/prompt/n(1-4)/size/aspect_ratio/resolution(1K/2K/4K)/response_format(url/b64_json)/image(s) 参考图；同步等待 ≤5min，超时 202+task_ids |
| `/v1/images/edits` | POST | 参考图编辑（multipart：image 可多值 + prompt 等字段；≤10 张、单图 ≤7MB、jpeg/png/webp） |
| `/v1/images/{id}` / `{id}/content` / DELETE | GET/DELETE | 图片任务查询 / 取图 / 取消 |
| `/v1/models` | GET | 模型列表（视频 MiniMax-H3、MiniMax-H3-Max + 图片 nano_banana_2_flash） |

管理端点（/api/*，session 认证）完整清单见 `api_admin.go` 头部注释；错误响应统一
`{"error":{"code","message","type"}}`（OpenAI 风格）。

## 10. 发行包

`dist/minimax-2api-v1.0.0-win-x64/`（另有同名 zip）为自包含发行目录：

```
minimax-2api.exe     发行构建（-trimpath -ldflags "-s -w"，约 12MB，内嵌 Web 资源）
config/config.json   默认配置模板
data/                空数据库 + videos/ images/ 产物目录
start.bat            双击启动
README.md            本文档
```

解压即用：双击 `start.bat` → 打开 `http://127.0.0.1:8787/web`（默认 admin/admin123，
**首次登录请立即修改密码**）。发行包数据库为空白初始态，不含任何账号数据。

自行构建发行版：

```bash
PATH="/d/Go/go/bin:$PATH" go build -trimpath -ldflags "-s -w" -o minimax-2api.exe .
```

## 11. 已知缺口 / 路线图

- **LLM 对话端点未实现**：`/v1/chat/completions`、`/v1/responses`（infinite-canvas 的画布
  助手/Agent 功能依赖）。云端存在 Anthropic Messages 兼容 `/api/v1/messages` 与 OpenAI
  Responses 兼容 `/api/v1/responses` 可转接，属后续增量；
- **TTS 未实现**：`/v1/audio/speech`（云端有 `/api/v1/audio/tts` 可转接）；
- **图片/视频厂商扩展位**：后端为 registry 模式，云端另有 seedream/kling/qwen/midjourney
  （图片）与 kling/veo3/seedance/jimeng/wan（视频）同构端点，可按需注册；
- **每账号独立出口代理**未实现（设计文档 Phase 2 项）：同机多账号存在 IP 关联风控风险；
- SUBMIT_UNKNOWN 任务无自动对账（云端未发现按会话列任务的接口），需管理台人工重试/忽略；
- 云端 cancel 是否退积分未严格验证（实测取消未扣费，样本有限）。

协议事实基线与逐行证据见逆向分析报告（接口路径/参数/响应结构均标注了桌面端源码行号）。
