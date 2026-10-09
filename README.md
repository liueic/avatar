# avater

轻量自托管 **Gravatar 兼容**头像代理。客户端以标准 Gravatar URL 请求头像，avater
在本地缓存、审核上游内容，未通过审核或尚未审核时返回由哈希确定性生成的本地
几何图案头像（DiceBear identicon，无生成式 AI）。

```text
GET /avatar/<md5-or-sha256-hex>?s=80&d=identicon
```

把应用里的 `https://secure.gravatar.com/avatar/` 前缀换成
`https://your-host/avatar/` 即完成迁移，下游零改动。

设计目标：**Gravatar 兼容 / 安全优先（SSRF 防护、解炸弹防护、内容审核）/
单机 2 vCPU·2 GB 可稳定运行 / CDN 友好 / 审核引擎可替换**。完整设计见
[SPEC.md](SPEC.md)。

---

## 工作方式

```
请求 ──► 缓存命中(approved) ──► 返回真图
         缓存未命中 ──► 仅从 Gravatar 白名单域名拉取
                      ──► 魔数/尺寸/解炸弹校验 + 重编码（剥离 EXIF）
                      ──► 进入审核队列（ONNX 三分类：SFW/NSFW/NSFL）
                      ──► 本次请求先返回默认头像
审核通过 ──► 写入缓存，后续命中
审核拒绝 / 待审核 / 审核服务不可用 ──► 永远返回确定性默认头像（原图绝不外发）
```

- 状态机：`absent → pending_fetch → pending_review → approved / rejected`，
  另有负缓存 `rejected_fetch`（上游无此头像）。
- 默认头像：DiceBear `thumbs`（橘色小手手，`d=retro` 映射到 `lorelei`
  插画面孔），seed 即哈希本身——同一哈希跨重启、跨实例输出一致。
- 审核：默认使用
  [OwenElliott/image-safety-classifier-xs](https://huggingface.co/OwenElliott/image-safety-classifier-xs)
  （SwiftFormer-xs，3.5M 参数，官方 ONNX 变体，预处理已烘焙进图），CPU 单张
  推理几十毫秒；阈值与灰区策略可配置，灰区默认拒绝并进入人工复核通道。
  引擎是接口抽象（`onnx` / `none`），可替换为外部 API。
- 持久化：SQLite（WAL，纯 Go 驱动）存元数据 + 文件系统存图片 blob；
  无 Redis、无外部队列。审核队列进程内有界，重启后按 `pending_review`
  全量重扫，任务不丢。

### 与 Gravatar 的行为差异（重要声明）

- **上游有图但尚未审核通过**期间，本服务返回本地默认头像（HTTP 200），
  Gravatar 此时可能返回 404 或官方默认图。这是审核流水线的预期行为。
- `d=404` 仅在**上游确实不存在该头像**（`rejected_fetch`）时返回 404；
  审核拒绝的条目**永远不返回原图**（含 `d=404` 请求），而是返回默认头像。
- `d=` 自定义 URL **不支持**（防开放重定向/SSRF），未知取值回退为本地默认头像。
- `r=/rating` 被忽略（审核口径只有通过/拒绝）。
- 其余 Gravatar 兼容行为：`s/size`（1–2048）、`f=y` 强制默认图、`.jpg/.png`
  后缀、`Access-Control-Allow-Origin: *`、确定性 ETag 与 304。

### DiceBear 样式署名

内置五种样式，均为 **CC0 1.0（公有领域）**，法律上无需署名；出于礼貌仍致谢
[DiceBear](https://www.dicebear.com)（代码 MIT）：

| 样式 | 长相 | 用途 |
| --- | --- | --- |
| `thumbs` | 橘色/蓝色小手手表情（默认） | 默认 & 未知 `d=` 回退 |
| `lorelei` | 插画人物面孔 | `d=retro` 映射 |
| `open-peeps` | 手绘人物 | 可选（配置启用） |
| `identicon` | 经典几何方块 | `d=identicon`（协议语义固定） |
| `pixel-art` | 8-bit 像素风 | 可选（配置启用） |

用 `default_avatar.style` / `default_avatar.retro_style` 配置切换。若自行接入
其它 DiceBear 样式，注意多数人物类样式为 CC-BY 等署名许可，需在页脚/文档署名。

---

## 快速开始

### Docker（推荐）

```bash
docker build -t avater .
docker run -d --name avater \
  -p 8080:8080 -p 127.0.0.1:8081:8081 \
  -e AVATER_ADMIN_TOKEN=change-me \
  -v avater-data:/data \
  avater
curl -I http://127.0.0.1:8080/avatar/205e460b479e2e5b48aec07710c08d50
```

镜像内含 onnxruntime 共享库与审核模型（构建期下载并校验 SHA-256）。
镜像外的构建参数：`ORT_VERSION`（onnxruntime 版本）、`HF_ENDPOINT`
（镜像源，如 `https://hf-mirror.com`）。

### 裸机

```bash
# 依赖：Go ≥ 1.25、C 编译器（cgo）；运行时需 onnxruntime 动态库 + 模型文件
make model ort      # 下载模型与本地开发用动态库（macOS arm64 示例）
make build
cp avater.toml.example /etc/avater/avater.toml   # 设置 admin_token
sudo cp deploy/avater.service /etc/systemd/system/
sudo systemctl enable --now avater
```

环境变量 `GOMEMLIMIT=1500MiB GOGC=50` 由 systemd unit / Dockerfile 预置。

### 不带审核引擎先跑起来

```bash
AVATER_ADMIN_TOKEN=dev-token \
AVATER_MODERATION_ENGINE=none \
AVATER_MODERATION_NONE_POLICY=approve \
make run
```

`engine=none` 时全部图片按策略直接通过/拒绝，用于调试或有意关闭审核的场景。

---

## 配置

环境变量优先，TOML 文件（`AVATER_CONFIG` 或 `-config`）其次；完整默认值见
[avater.toml.example](avater.toml.example)。关键项：

| 配置 | 默认 | 说明 |
| --- | --- | --- |
| `listen` / `admin_listen` | `:8080` / `:8081` | 公网口与管理口（管理口勿暴露公网） |
| `admin_token` | 必填 | 管理面 Bearer token，为空则启动失败 |
| `upstream.allowed_hosts` | gravatar 五个域名 | 严格相等白名单 + 拨号时 IP 校验 + 禁重定向 |
| `moderation.engine` | `onnx` | `onnx` / `none` |
| `moderation.workers` | `1` | 1–2；推理 intra-op 线程固定 1 |
| `cache.max_bytes` | 1 GB | 超限按 last_accessed LRU 淘汰 approved |
| `cdn.provider` | `none` | `cloudflare` / `fastly` 启用 tag purge |

---

## HTTP 面

| 路由 | 说明 |
| --- | --- |
| `GET /avatar/{hash}` | 主入口（`/{hash}` 亦兼容；`.jpg/.png` 后缀被忽略；`.png` 同时作为默认头像的格式协商） |
| `GET /healthz` | 存活探针（无鉴权） |
| `GET /readyz` | 就绪探针：审核模型加载完成 |
| `GET /metrics` | Prometheus 文本格式（`metrics.enabled` 时） |

管理面（独立端口 + `Authorization: Bearer <admin_token>`）：

```
GET    /admin/stats
GET    /admin/entries?status=&limit=&cursor=
POST   /admin/entries/{hash}/approve[?force=true]
POST   /admin/entries/{hash}/reject[?force=true]
POST   /admin/entries/{hash}/remoderate[?force=true]
POST   /admin/remoderate?model_ver=&status=
DELETE /admin/entries/{hash}
POST   /admin/purge?status=expired|all
GET    /admin/health
```

人工 approve/reject 记录 `manual_override`，自动重审不覆盖，除非 `force=true`。
所有管理面变更会同步触发 CDN tag purge（失败只记日志异步重试）。

---

## CDN 集成

源站按**响应状态**输出精确缓存头（§16.1）：

| 响应 | Cache-Control（默认） |
| --- | --- |
| approved 头像 | `public, max-age=86400, s-maxage=604800, stale-while-revalidate=86400, stale-if-error=604800`（不用 `immutable`——重审会换内容） |
| `f=y` / 显式 `d=` 的默认头像 | `public, max-age=2592000, immutable` |
| pending/rejected 的默认头像 | `public, max-age=60, s-maxage=300`（保证审核通过后尽快回源换真图） |
| 上游无图 404 | `public, max-age=300` |
| 429 / 5xx、health/metrics/admin | `no-store` |

内容撤回：每个头像响应带 `Cache-Tag: av-<hash>`（Cloudflare）或
`Surrogate-Key`（Fastly）；管理面变更自动按 tag purge。

> **Cloudflare 注意**：按 tag purge 需 Enterprise 档。免费档只有单 URL purge，
> 而 `s` 取值空间无法枚举——请把 `ttl_approved_cdn` 调小到可接受窗口用 TTL 兜底。

源站保护（§16.4）：防火墙只放行 CDN IP 段，或启用 Authenticated Origin Pull；
源站限流（全局 200 rps + 每 IP 20 rps）保留，miss 风暴仍会回源。

---

## 安全清单（§17 摘要）

- **SSRF**：上游 URL 由服务端按模板构造；主机白名单严格相等；DNS 解析后逐 IP
  校验（loopback/链路本地/私网/CGNAT/ULA/保留段全部拒绝）并 pin 连接；禁重定向。
- **解炸弹**：响应体 10 MB 上限 + `DecodeConfig` 头部尺寸校验 + 像素总数上限，
  之后才完整解码；GIF/WebP 动图只取首帧。
- **恶意文件**：魔数白名单（不信 Content-Type）+ 解码后重编码，剥离 EXIF 与
  恶意 chunk。
- **违规内容**：默认拒绝策略——只有 approved 可被服务；灰区默认拒绝；原图对外
  永不提供于 rejected 状态。
- **隐私**：系统任何层面不接触原始邮箱（协议只有哈希）；日志白名单
  （hash/status/latency/verdict/reason），不记查询串，客户端 IP 默认不记录。
- **供应链**：模型文件启动时 SHA-256 校验；`go.sum` 锁定；CI 跑 `govulncheck`。

---

## 开发

```bash
make test    # 单测 + 集成测试（httptest 假上游、恶意用例表、goleak）
make vet
make model ort   # 本地跑 ONNX 审核引擎所需（模型 + 动态库）
```

ONNX 模型与 onnxruntime 动态库只在**运行时** dlopen——构建不需要它们，
`engine=none` 时运行也不需要。

## 许可

- 本项目代码：见 LICENSE（DiceBear 代码 MIT；identicon/pixel-art 样式 CC0 1.0）。
- 审核模型 `image-safety-classifier-xs`：MIT（作者注明 NSFL 样本偏少，
  血腥类可能漏检——这正是灰区默认拒绝 + 人工复核通道的设计原因）。
