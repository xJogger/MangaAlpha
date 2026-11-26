# Architecture

## 1. 总体数据与请求流（v3）

```text
浏览器（书架 / 历史 / 简体中文阅读器 / /account / /admin） · Mihon / Koharia · KOReader (OPDS) · AI Agent
       │  Session Cookie / HTTP Basic Auth / Authorization: Bearer / X-API-Key
       ▼
internal/httpserver (authMiddleware + UserScope RBAC + SPA + /reader + /admin/api + /api/agent/v1 + /api/v1,/v2 + OPDS)
       ├── SQLite 主库（WAL + _txlock=immediate；多用户、书库/联合库、FTS5、进度/墓碑、多源元数据）─ internal/database
       ├── 繁简与日文异体字折叠字表（OpenCC 4,543 组） ─────────────────── internal/cjk
       ├── Bangumi 离线索引库 + 实时授权客户端 + 代理 + 单向追踪器 ──────── internal/bangumi
       ├── 多源元数据市场、纯 Go 流式解压、只读 Sidecar、映射包与 SSRF 防护 ─ internal/extmeta
       ├── 多源递归扫描器（含 115 / 本地与 SMB / WebDAV 空根目录保护） ──── internal/scanner
       ├── 单库与联合库自动化定时流水线 ────────────────────────────────── internal/pipeline
       ├── 统一归档读取器（普通及非固实加密 ZIP / RAR4 / RAR5 按页流式） ── internal/archive
       │     ├── 115 Open API（downurl + Range 分块缓存） ─────────────── internal/oneonefive + internal/cache
       │     ├── WebDAV（令牌桶限速 + HTTP Range + 分块缓存） ─────────── internal/webdav + internal/cache
       │     └── Local / SMB FS（*os.File 原生直读，跳过 Range 缓存） ─── internal/localfs
       ├── 系列封面（外部源 / Bangumi 懒加载缓存与首卷回退） ────────────── internal/thumbnail
       ├── 单部/系列归档维护、全平台脱敏诊断与 24h 跨平台更新检测 ───────── internal/maintenance + diagnostics + updater
       └── Android 原生封装（gomobile bind + SAF + 可选局域网服务） ────── mobile + android
```

核心铁律（所有后续开发必须严格遵守）：**扫库只读目录与文件名，初始元数据与封面只从离线数据库与已缓存数据获取；严禁在启动、扫库、离线元数据刮削或书架/详情浏览过程中隐式打开或读取任何漫画归档内部字节（包括本地与 SMB 挂载书库）**。一切必须读取归档内部字节才能获得的信息（页表索引、`ComicInfo.xml`、归档第 1 页封面等），只在用户真正打开/阅读该文件时顺带按需读取并缓存，或由管理员显式触发批量任务 / 主动开启可选定时选项时才执行，以保障超大型网络书库的入库与浏览速度。

## 2. 鉴权、多用户分权与会话安全

1. **初始化与强制设密**：服务端通过 `auth_admin` / `users` 表判定是否已完成主管理员初始化（`HasAdmin`）。未初始化时仅 `GET /health`、`/setup`、公开静态资源与 `POST /api/v1/claim` 可访问；初始化完成（或通过 `FK115_ADMIN_USERNAME` + `FK115_ADMIN_PASSWORD` 首次启动自动预设）后永久关闭。
2. **1 主管理员 + N 普通用户（`UserScope`）**：
   - 普通用户仅由管理员在 `/admin` 创建与授权（禁止自助注册与自行改名）；后台管理接口、元数据写操作、任务/诊断、明文加密密码本与 `/api/agent/v1/*` 均由 `requireAdmin` 拦截返回 `403`。
   - 所有目录、搜索、详情、封面、进度、墓碑、历史、收藏夹、阅读列表、客户端设置、SSE 与 Bangumi 追踪查询均通过 `database.UserScope`（`user_id`、书库白名单 fail-closed、`allow_nsfw` / `show_nsfw`）严格隔离。
   - SPA 与内置阅读器状态变更请求附带 `X-FK115-User-Id` 校验防切号错写；受保护的漫画页图与封面响应强制携带 `Cache-Control: private, no-store`；未登录态与公开接口不暴露版本号或代号。
3. **认证凭证与防护**：
   - **Session Cookie**：`fk115_session` 与兼容 Cookie `KOMGA-SESSION`（`HttpOnly`, `SameSite=Lax`，HTTPS 下经可信代理 `FK115_TRUSTED_PROXIES` 自动启用 `Secure`），支持滑动续期；数据库仅存 `SHA-256(token)`。
   - **API Key**：支持 `Authorization: Bearer <key>` 与 `X-API-Key: <key>`，按用户独立管理与吊销；数据库仅存 `SHA-256(key)`。
   - **HTTP Basic Auth**：兼容 Mihon、Koharia 与 KOReader OPDS；内存维护短时凭据摘要缓存避免高并发图片流重复触发 `argon2id`；登录与 Basic Auth 均具备双层 Per-IP / Per-Account 限流与退避，状态变更校验同源 CSRF。

## 3. 多存储源、联合库、加密归档与隐藏隔离

- `libraries` 表通过 `provider_type`（`115` | `local` | `webdav` | `union`）、`webdav_account_id`、`root_path`、`enabled` 区分来源与可见性：
  - **115 网盘（`115`）**：完全保留 v1 的 `id.Library` / `id.Series` / `id.Book` 公式，支持分页增量遍历与空根目录防清空保护，旧库原地升级 100% 保留进度与缓存。
  - **本地与 SMB 挂载目录（`local`）**：`root_cid` 为 `"local:" + cleanRootPath`；优先复用 `DirEntry.Info()` 减少 SMB 元数据放大；根目录返回 `0` 项且已有书籍时拒绝提交扫描；归档读取直接使用 `*os.File`（`io.ReaderAt`）跳过 Range 分块磁盘缓存。
  - **WebDAV（`webdav`）**：支持多账号配置（CloudDrive2 / OpenList / NAS），令牌桶限速同时约束 `PROPFIND` 与 `FetchRange`，支持瞬断重试、重定向 Range 保留与空根目录保护。
  - **分层叠加联合库（`union`，OverlayFS 风格）**：按优先级将多个物理子库物化叠合为单一逻辑库（`union_layers` / `union_series_sources` / `union_book_sources`），按归一化相对路径对齐、卷级互补叠加、同名上层覆盖，读取失败透明回退下层副本；物理子库对前台隐藏，子库手动扫完自动静默刷新所属联合库；阅读进度双向回写底层单册，解散无损保留；任意物理源为 NSFW 时保守继承 `is_nsfw = 1`。
  - **普通与加密归档按页流式读取（`internal/archive`）**：纯 Go 支持普通及非固实加密 ZIP（ZipCrypto / WinZip AES-128/192/256）与 RAR4/RAR5（`-p` / `-hp`），保持 HTTP Range 按页流式解密；网页阅读器解锁后按同文件夹自动复用密码，明文密码本仅主管理员可在后台查看或清空。
  - **隐藏书库（`enabled = 0`）**：保留全部数据与缓存，在前台、Komga API、OPDS、阅读器直链（统一返回 `404`）及扫库/刮削任务中彻底隔离，隐藏瞬间自动取消进行中任务。
- **SQLite 并发写入策略**：主库启用 WAL 模式并在 DSN 中配置 `_txlock=immediate`，确保写事务在并发场景下（如后台 FTS5 回填与联合库刷新并行）正确等待 `busy_timeout` 而非立即抛出 `SQLITE_BUSY`。

## 4. 多源元数据刮削中心（`internal/bangumi` + `internal/extmeta`）与 NSFW 隔离

1. **多规则漫画目录名清洗器（`parser.go` + `internal/cjk`）**：
   - 基于 OpenCC 4,543 组繁简/台湾异体字/日文新字体折叠字表做统一归一化；
   - 支持双序多方括号提取、`x` 合著切分、括号内外双标题、中英边界切分、扫图组/压制/平台尾部噪点清洗与 `台版 / 特别篇 / 单行本` 子目录父级回溯。
   - `+` / `&` 合集标题仅在完整标题吻合或各部分同属一部作品正式标题/别名时才自动匹配，防串书。
2. **内置 Bangumi 离线 SQLite 索引与实时授权客户端（`internal/bangumi`）**：
   - 默认指向与官方 100% 同构的中文简介增强镜像（`zh_cn`：`xJogger/bangumi-archive-zh`），可随时切换 `official`（`bangumi/Archive`）或 `custom` 源；支持每周/每月定时检查更新并自动增量 diff 同步未锁定字段。
   - 启动时若 `terms_schema_version` 落后自动在事务内重建 `bgm_terms` 词表；支持匹配评分明细与 Top-3 近失候选展示。
   - `api.bgm.tv/v0` 客户端支持按关键词或 Subject ID 实时搜索绑定；当当前用户（或回退借用主管理员）已绑定 Bangumi OAuth / PAT 且满足 `EffectiveShowNSFW()` 时，携带授权检索 NSFW 条目并拉取封面（落实 5 道防泄露防线）；每用户独立 Bangumi 单向进度追踪器支持 OAuth 2.0 自动续期。
   - 独立网络代理配置统一作用于 Bangumi、元数据市场/离线源/封面抓取与 GitHub 更新检测，不影响 115 / WebDAV 直连流量。
3. **多源扩展元数据引擎（`internal/extmeta`）与存储模型**：
   - **声明式元数据市场与只读 Sidecar**：支持订阅 `mangaalpha-repo-v1`（`repo.json`）或接入本地/远程 SQLite（纯 Go 流式解压 `.zst`/`.gz`/`.zip`），以只读模式打开 `mangaalpha_meta_v1` 单表或 `gallery_v1` 四表上游库并在旁构建 FTS5 + 封面 SHA-1 Sidecar 索引；支持标签翻译词典（`tag_translation_text_v1`，`zh` / `bilingual` / `raw`）、已缓存的归档内嵌元数据线索（`ComicInfo.xml`，严禁刮削阶段隐式拆包读取归档）与社区脱敏映射包（`mangaalpha_mapping_v1`，兼容空包零报错同步）。
   - **三张配置表与统一任务历史**：`metadata_repos`、`metadata_sources`、`library_metadata_sources`（按顶层书库配置 `#1 → #2 → ...` 顺延链，跳过联合子库）；所有离线源未命中时由已启用映射包兜底，默认纯离线极速匹配，显式开启后才允许单线程限速防封熔断在线补全；批量刮削与任务记录统一持久化在 `bangumi_scrape_runs`（扩展多源运行以 `ext_` 前缀标识），无已就绪离线源时预检拦截。
   - **`series_metadata` 扩展字段与 `match_origin` 保护**：记录 `ext_source_id`、`ext_entry_id`、`ext_cover_url`、`ext_cover_path` 与 `match_origin`（`auto` / `mapping` / `manual` / `agent`），所有自动批量刮削与增量同步（含 `force`）均严格保护 `manual`、`agent`、升级存量绑定及 `*Lock` 锁定字段。
   - **防 SSRF 网络客户端与脱敏诊断（v3.2.1）**：统一拦截字面量私网/回环/链路本地/CGNAT/Fake-IP URL 及内网主机名；直连模式下对合法公网域名的本地 DNS 解析跳过 `0.0.0.0`/`::` 占位，放行标准透明代理 Fake-IP（`198.18.0.0/15`、`fc00::/18`，双栈优先拨号非 Fake-IPv6），并拒绝混合真实私网 DNS 防 Rebinding；外部封面具备限速、并发上限 2、魔数校验、URL 版本化缓存、指数退避熔断与失败自动回退已缓存首卷缩略图或占位图（绝不在封面请求中隐式拆包）；仓库订阅、源同步与封面异常均记录脱敏 `diagnostics` 事件并返回清晰中文分类提示。详见 [docs/METADATA_SOURCES.md](METADATA_SOURCES.md)。
4. **NSFW 分级规则与按账号控制**：
   - Bangumi 命中条目严格依据官方 `nsfw == true` 标记；`gallery_v1` 仅当 `category`（忽略大小写）严格等于 `"Non-H"` 时判为非 NSFW；未匹配条目通过 `InferPathNSFW` 辅助识别显式成人目录词；支持 `nsfwLock` 手动锁定。

## 5. 内置书架、简体中文阅读器、OPDS、客户端生态与 Agent 接口

- **内置书架、个人账号页与管理后台 SPA（`internal/httpserver/static/ui/`）**：全局管理（`/admin`，7 个全局 Tab）与个人设置（`/account`，个人密码、API Key、NSFW 视图、书架/首页显示数量偏好、Bangumi 追踪器与油猴脚本）分离；支持自动昼夜主题与小鱼图标同步、阅读历史时间线（`/history`）、收藏夹（`/collections`）与阅读列表（`/readlists`）、自定义弹窗（`confirmModal`）、移动端适配与空闲零轮询。
- **内置简体中文阅读器（`/reader/*`，`web/reader/` + `scripts/build-reader.sh`）**：基于上游 `Suwayomi/Suwayomi-WebUI`（`MPL-2.0`，锁定 `r3592`）构建，默认简体中文与纯黑画布，支持浏览器端自动裁切白边/黑边（手机 `< 900px` 默认开启、桌面默认关闭）、加密包弹窗解锁、以及通过 `GET /api/v1/books/{bookID}/load-status` 实时展示网络串流加载进度。
- **大书库检索与 Koharia / Mihon 深度兼容**：
  - 独立无内容 FTS5 倒排索引配合 OpenCC 4,543 组字表，实现大库繁简/日文异体字毫秒级检索且非阻塞后台分批构建；
  - `series` 表通过触发器维护 `books_count` / `sort_modified_at` / `sort_created_at`，配合 `internal/database/shelf_cache.go` 版本化内存缓存与分块 `IN` 批查消除大库慢查询；
  - 递归下降检索解析器（`internal/database/search.go`）支持 `(<q>) OR tag:(<q>)`、`AND`/`OR`/`NOT` 与 `sort=relevance`；
  - `book_read_progress_tombstones`（90 天墓碑）+ HTTP `gzip` + Komga SSE（`/api/v1/sse/v1/events`）确保数十万册大库增量同步流畅。
- **双入口 OPDS v1.2 + OPDS-PSE（`internal/httpserver/opds.go`）**：提供面向 `opdsforcomic.koplugin` 的 `/api/opds/v1.2`（0-based `pse:lastRead`、零提前建索引、前后章节导航）与标准 Komga `/opds/v1.2/catalog`（1-based `pse:lastRead`、OpenSearch、`?convert=jpeg` 按需转码）。
- **定时自动化流水线（`internal/pipeline`）**：按服务器本机时区每日/每周串行执行「扫库（联合库可勾选子库）→ 联合刷新 → 多源元数据增量刮削 → 缺封面未匹配作品自动补首卷封面（自动跳过已绑定外部封面作品）」。
- **Agent 专用接口（`/api/agent/v1/*`，`internal/httpserver/agent.go`）**：支持一键设备授权流（`/api/agent/v1/guide`、`/auth/requests`、`/agent-auth/{id}`）、`/manifest`、`/overview`、书库 CRUD、紧凑分页 `/series` 与单次最多 200 条异构 `/series/batch`，详见 [docs/AGENT_API.md](AGENT_API.md)。
- **Bangumi 排名与同名合并**：网页版 `/admin/api/bookshelf/*` 按正数 `bgm_subject_id` 分组合并仅作用于网页书架（不修改真实 ID/进度，Komga v1/v2 与 OPDS 客户端保持独立展示）。

## 6. 脱敏诊断、跨平台更新、纯 Go 构建与 Android 独立版

- **全平台脱敏诊断（`internal/diagnostics`）**：白名单结构化事件本地轮转存储于 `<DataDir>/diagnostics/`（自动脱敏 URL、凭据、Token、Cookie、API Key、原始 IP 与私人路径；`ClassifyError` 分类前先剥离 URL 防文件名误判），覆盖扫描、归档、WebDAV、Bangumi、扩展元数据（`extmeta_repo` / `extmeta_sync` / `extmeta_cover`）、更新检测与启动失败。
- **跨平台版本更新检测（`internal/updater`）**：每 24 小时静默检测 GitHub Releases（复用网络代理），Android 端强制校验 `SHA256SUMS` 后经 `PackageInstaller` 拉起安装。
- **纯 Go 构建与 Android 独立版**：服务端保持 `100% CGO_ENABLED=0`（含纯 Go `modernc.org/sqlite`、`klauspost/compress/zstd`、`nwaples/rardecode`），支持 Windows 系统托盘（`-H=windowsgui`）。Android 独立版通过 `gomobile bind` 封装 `internal/app.App`，支持 SAF 随机读取、原生设置页与按需开启的 `0.0.0.0:<端口>` 局域网服务。详见 [docs/ANDROID.md](ANDROID.md)。
