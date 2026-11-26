# AGENTS.md

本文档面向 AI 编程助手、书库管理 Agent 以及参与二次开发与 PR 贡献的开发者。
- 若你是参与代码修改的 **贡献者 / AI 编程助手**，开始修改前请先阅读 `CONTRIBUTING.md`、`README.md`、`SECURITY.md`、`docs/ARCHITECTURE.md`、`docs/METADATA_SOURCES.md` 与 `docs/AGENT_API.md`。
- 若你是通过 HTTP 接口管理运行中书库的 **运维 / 整理 Agent**，请直接阅读 `docs/AGENT_API.md` 并请求 `GET /api/agent/v1/manifest`。

## 项目目标与核心约束

这是一个轻量、纯 Go（`CGO_ENABLED=0`）自包含的 Komga / OPDS 兼容漫画服务端，内置响应式书架、简体中文网页阅读器、Bangumi 元数据刮削与单向进度追踪器、完整多用户分权隔离体系与 Agent 管理接口，支持 115 Open API、服务器本地 / SMB 挂载目录、WebDAV（CloudDrive2 / OpenList / NAS）与联合库（OverlayFS 分层叠加）四类书库形态。核心约束：

1. **零隐式拆包与扫描只读目录结构 + 空挂载点保护（核心铁律，所有后续开发必须严格遵守）**：
   - **扫库阶段**只列出 115 / 本地（含 SMB）/ WebDAV 的目录和文件名、大小、时间等元数据，绝不读取归档内部字节。
   - **初始元数据与封面**一律只从离线数据库（Bangumi 离线索引、Komga `database.sqlite`、开放离线包等）及已缓存记录获取。
   - **严禁在扫库、离线元数据刮削、书架浏览或详情加载过程中隐式打开或读取任何漫画归档字节（包括本地与 SMB 挂载书库）**。一切必须读取归档内部字节才能获得的信息（页索引、`ComicInfo.xml`、归档第 1 页封面等），只允许在「**用户真正打开/阅读该文件时顺带按需缓存**」，或「**管理员显式触发批量任务 / 主动开启的可选定时选项**」中执行。
   - 对于已有书籍记录的本地/SMB 与 WebDAV 书库，若根目录扫描返回 `0` 项（如 SMB 断连未挂载），必须拒绝提交扫描以保护历史数据。
2. **严格按需流式读取（含加密归档）**：115 与 WebDAV 仅在用户真正打开阅读具体漫画归档（或显式批量维护任务）时通过 HTTP Range 分块读取（WebDAV 同时对 `PROPFIND` 与 `Range` 实施令牌桶限速）；本地与 SMB 挂载目录同样遵循上述零隐式拆包铁律，真正读文件时直接通过 `*os.File`（`io.ReaderAt`）随机读取，自动跳过 Range 分块磁盘缓存。ZIP（ZipCrypto / WinZip AES）与非固实 RAR4/RAR5 加密包同样保持纯 Go 按页流式解密，严禁整包下载。
3. **多用户分权与强制鉴权（1 主管理员 + N 普通用户）**：
   - 未完成首次主管理员设密前，除 `/health`、`/setup`、公开静态资源与初始 Claim 端点外拒绝一切业务请求；设密完成后永久关闭初始化入口。
   - API 统一支持 Session Cookie、`HTTP Basic Auth`、`Authorization: Bearer <key>` 与 `X-API-Key: <key>`；状态变更请求对浏览器 Session 与 Basic Auth 实施同源 CSRF 校验，SPA 与内置阅读器写请求附带 `X-FK115-User-Id` 校验防切号错写。
   - 普通用户仅由管理员在 `/admin` 创建（禁止自助注册、禁止自行改用户名）；所有后台管理接口、元数据写接口、书库/存储源/任务/日志、明文加密密码本及 `/api/agent/v1/*` 必须受 `requireAdmin` 严格 `403` 拦截。
   - 新增或修改任何与用户相关的 SQL/HTTP/SSE 逻辑时，必须严格遵守 `database.UserScope`（`user_id`、`AllowsLibrary`、`AllowNSFW` / `ShowNSFW`）隔离：阅读进度、进度重置墓碑、阅读历史、在读列表、收藏夹、阅读列表、客户端设置、内置阅读器 `localStorage`、SSE 推送、会话、API Key、油猴配对与 Bangumi 追踪器均按用户完全隔离；空书库白名单必须 fail-closed（返回空结果，严禁回退为不过滤）。
4. **隐藏书库、联合库与 NSFW 分级隔离**：
   - **隐藏书库（`libraries.enabled = 0`）**：保留全部数据库记录与缓存，但在前台书架、Komga 客户端、OPDS、阅读器直链（统一返回 `404`）及扫描/刮削任务中完全隔离，隐藏瞬间自动取消该库进行中任务。
   - **联合库（`provider_type = 'union'`）**：成员物理库对前台隐藏；对无成员库授权的普通用户不得在响应中暴露受限成员库元信息；任意物理源为 NSFW 时（未锁定 `nsfw_lock`），联合库系列必须保守继承 `is_nsfw = 1`。
   - **NSFW 分级与按账号控制**：Bangumi 命中条目**仅**依据官方布尔字段 `nsfw == true` 标记 `is_nsfw`（严禁将 `nsfw == false` 但带用户标签的普通条目误判为 NSFW），`gallery_v1` 离线图库源仅当 `category`（忽略大小写）严格等于 `"Non-H"` 时判为非 NSFW，未匹配条目辅助识别显式成人路径词；所有自动批量刮削与增量同步（含全量重刮）均须保护 `match_origin IN ('manual', 'agent')`、存量升级绑定与 `*Lock` 字段锁；普通用户仅在 `allow_nsfw = 1 && show_nsfw = 1` 时可见 NSFW 作品。
5. **保持 100% 纯 Go 无 CGO + 跨平台兼容**：必须同时兼容 Linux、macOS、Windows（`-H=windowsgui` 系统托盘模式）与 Android `gomobile` 原生封装。
6. **数据库迁移与大库性能约束**：
   - 所有 Schema 变更必须在 `internal/database/migrations.go` 中做到幂等、事务安全、保留既有主管理员数据与 `collections.rowid` 等兼容约束，并补充存量旧库升级单测；SQLite 主库 DSN 保持 WAL + `_txlock=immediate` 使并发写事务正确遵守 `busy_timeout`。
   - 真实部署可达 10 万系列 / 50 万+单册，避免在热路径引入 N+1 查询、全表扫描或未分批的大 `IN (...)`。

## 开发与构建命令

```bash
bash scripts/build-reader.sh
node --check internal/httpserver/static/ui/app.js
node --test scripts/*.test.cjs
gofmt -w cmd internal
go test -count=1 -race ./...
go vet ./...
GOOS=windows GOARCH=amd64 go vet ./...
CGO_ENABLED=0 go build -o /tmp/mangaalpha ./cmd/server
```

注意：`internal/httpserver/static/reader_dist/` 的编译产物已被 `.gitignore` 忽略（仅保留 `placeholder.txt` 使无前端产物的纯 Go 单测可直接编译）。改动 `web/reader/` 或正式打包二进制/镜像前必须先运行 `bash scripts/build-reader.sh`（或 `make build`）。

## 目录职责

- `cmd/server`：CLI 参数、Windows 系统托盘（`tray_windows.go`）、信号处理和进程生命周期。
- `internal/app`：组装数据库、115、WebDAV、Bangumi、扫描器、流水线、缓存、归档、缩略图、维护与 HTTP 服务，暴露 `Start()` / `ListenAddr()` / `Shutdown()` / `Close()`。
- `internal/buildinfo`：版本号与 3.x 大版本代号元信息。
- `internal/database`：SQLite schema、幂等升级迁移、多用户与权限表（`users.go`）、鉴权哈希（`argon2id` + `SHA-256`）、多存储源、联合库（`union.go`）、目录搜索与无内容 FTS5 索引（`catalog.go` / `search.go`）、多源元数据配置与顺延链表（`metadata_sources.go`）、收藏夹与阅读列表（`collections.go`）、阅读进度与历史墓碑（`progress.go`）、OPDS 映射（`opds.go`）、书架缓存（`shelf_cache.go`）。
- `internal/cjk`：基于 OpenCC 开放词典生成的 4,543 组繁简/异体字/日文新字体搜索折叠字表。
- `internal/oneonefive`：115 Open SDK 封装、Token 自动刷新、限速、目录列表与 `downurl` 获取。
- `internal/localfs`：本地与 SMB 挂载目录校验、免多余 `stat` 高效遍历、系统垃圾文件过滤与目录浏览。
- `internal/webdav`：多账号 WebDAV `PROPFIND` 目录遍历、连接测试、全局请求限速（含 `FetchRange`）与 HTTP `Range` 读取。
- `internal/scanner`：多存储源统一递归扫描、空挂载点防清空保护、普通模式与 One-Shots 事务暂存转换。
- `internal/pipeline`：单库与联合库定时/手动自动化流水线调度（扫库 → 联合刷新 → 多源增量刮削 → 补封面）。
- `internal/archive`：统一归档打开器（115/WebDAV 走 `RemoteReaderAt` + Range 缓存；本地/SMB 文件走原生 `*os.File`）及普通/加密 ZIP、RAR 索引与单页流式解压。
- `internal/bangumi`：目录名清洗器、`bangumi/Archive` 多源离线 SQLite 索引库与定时更新、`api.bgm.tv` 实时授权客户端（含 NSFW 5 道防泄露防线）、独立代理与每用户 Bangumi OAuth/PAT 单向进度追踪器。
- `internal/extmeta`：声明式元数据市场（`mangaalpha-repo-v1`）、纯 Go 流式解压（`.zst`/`.gz`/`.zip`）、只读双规范离线 SQLite（`mangaalpha_meta_v1` / `gallery_v1`）+ Sidecar FTS5/SHA-1 索引、标签翻译词典、社区脱敏映射包、包内 `ComicInfo.xml`/`galleryinfo.txt` 解析、防 SSRF（兼容透明代理 Fake-IP）客户端与限速熔断外部封面缓存。
- `internal/cache`：Range block 与已解压页面的磁盘缓存与 LRU 淘汰。
- `internal/thumbnail`：封面服务（`auto`/`bangumi`/`book` 优先级与按需 `singleflight` 懒缓存）及批量首卷封面任务。
- `internal/maintenance`：Series / Book 手动索引与封面维护任务。
- `internal/diagnostics`：全平台脱敏诊断日志缓冲与导出。
- `internal/updater`：跨平台 24 小时版本更新检测。
- `internal/httpserver`：鉴权与多用户 RBAC 中间件（`auth.go` / `security.go` / `users.go`）、内置书架与 `/admin` / `/account` SPA（`static/ui/`）、内置阅读器静态托管（`static/reader_dist/`）、管理 API（`/admin/api`）、Agent API（`/api/agent/v1`）、Komga v1/v2 兼容 API（`komga*.go` / `collections.go` / `sse.go`）与双入口 OPDS v1.2 / OPDS-PSE（`opds.go`）。
- `web/reader` + `scripts/build-reader.sh`：基于上游 `Suwayomi-WebUI`（MPL-2.0）的简体中文 `/reader/*` Komga REST 宿主适配层（`KomgaHostBridge.ts` + `apply-host.py`）与确定性构建脚本。
- `mobile` + `android`：基于 `gomobile` 的 Android ARM64 封装与原生设置页。

## 安全与红线规则

- 绝不把真实 Token、WebDAV 密码、代理密码、压缩包明文密码、Session Cookie、API Key、真实 `downurl` 或私人共享目录路径写入日志、代码或测试夹具。
- 绝不在本公开仓库中引入任何私有仓库域名、内网 IP、本机用户名路径或内部业务标识。
- 已有 `115` 书库的 `Library` / `Series` / `Book` ID 生成公式不可更改，确保旧版数据库无缝原地升级。
- 受权限保护的漫画页图与封面响应必须携带 `Cache-Control: private, no-store`，防止同浏览器切号命中缓存绕过权限检查；未登录态与公开只读接口不得暴露服务端版本号或代号。
- 前端 UI 改动必须与 `internal/httpserver/static/ui/app.css` 视觉体系保持一致：全部颜色走现有暗/亮双主题 CSS 变量（不硬编码色值）、复用既有 `.card`/`.btn`/`.badge`/`.data-table`/`openModal`/`toast` 组件、不引入外部 UI 库或 CDN，且保证桌面与手机竖屏（`360px ~ 390px`）双主题无横向溢出。
- 前端任务进度严禁在空闲状态下无条件轮询刷屏；仅当存在 `queued`/`running` 任务且页面处于前台可见状态时才按需轮询。
