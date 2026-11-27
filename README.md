# MangaAlpha

[![CI](https://github.com/xJogger/MangaAlpha/actions/workflows/test.yml/badge.svg)](https://github.com/xJogger/MangaAlpha/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/xJogger/MangaAlpha?display_name=tag)](https://github.com/xJogger/MangaAlpha/releases/latest)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/images/mangaalpha-banner.png">
    <img src="docs/images/mangaalpha-banner.svg" alt="MangaAlpha" width="680">
  </picture>
</p>

<p align="center">
  <img src="docs/images/showcase-bookshelf.jpg" alt="MangaAlpha 网页书架首页与手机端阅读历史时间线实景" width="920">
</p>

**MangaAlpha**（原名 `fake-komga-115`）是一个 **100% 纯 Go（`CGO_ENABLED=0`）单文件自包含**的轻量级 Komga 兼容漫画书库与阅读服务端，也提供可独立运行的 Android App。

它把 **115 网盘**、**服务器本地 / SMB 挂载目录** 与 **WebDAV（CloudDrive2 / OpenList / NAS）** 统一汇入同一个书库（支持多源分层叠加联合库），内置 **响应式网页书架与阅读历史时间线**、**支持自动裁切白边的简体中文双页网页阅读器**、**多源元数据刮削中心（内置 Bangumi 中文简介增强镜像与声明式元数据市场）**、**跨平台更新检测** 与 **AI Agent 书库治理接口**，并完整兼容 **Mihon（Komga 扩展）**、**Koharia** 与 **KOReader（OPDS v1.2 + OPDS-PSE）** 客户端。

```text
内置网页书架 (/) · 阅读历史 (/history) · 简体中文阅读器 (/reader/*) · Mihon / Koharia · KOReader OPDS · AI Agent (/api/agent/v1)
                       ↓ (Session Cookie / HTTP Basic Auth / Bearer & X-API-Key)
                                     MangaAlpha
               ├── 115 Open API ──────────────→ downurl + HTTP Range 分块按需流式解压
               ├── WebDAV (CloudDrive2/OpenList) ─→ PROPFIND + 令牌桶限速 + HTTP Range 按需解压
               └── 本地 / SMB 挂载目录 ────────→ 原生 io.ReaderAt 随机直读（免分块缓存 + 空挂载点保护）
```

> **按需流式核心承诺**：启动与目录扫描阶段仅读取目录层级及文件大小/修改时间元数据，**绝不提前下载整包压缩文件**；仅当读者打开某一卷并翻到具体页面时，才按需读取对应图片所需的字节区间。

---

## 核心特性

### 1. 内置简体中文阅读器（自动裁切白边 · 双页拼合 · 串流进度）

<p align="center">
  <img src="docs/images/showcase-reader.jpg" alt="MangaAlpha 桌面端双页对开与手机端自动裁切白边实拍" width="920">
</p>

- **纯前端自动裁切白边 / 黑边**：基于 `Suwayomi-WebUI`（无 GraphQL/WebSocket 纯 REST 宿主适配）打造，浏览器端本地采样边界并经 `OffscreenCanvas` 实时裁切缓存，**零服务端 CPU 占用**。手机/窄屏（`< 900px`）默认开启实现满屏沉浸阅读，桌面/宽屏（`>= 900px`）默认关闭保留原版比例，均可按全局默认或单部作品随时开关。
- **日漫友好默认体验**：默认简体中文界面、纯黑护眼画布（无初始触摸区与模式弹窗干扰）；桌面端默认日漫右翻双页对开（宽幅跨页图自动独占一屏），移动端默认单页自适应；支持条漫长图模式、跨卷无缝连读与页级阅读进度双向同步。
- **网络串流加载进度可视化**：在阅读 115 / WebDAV 远程归档时，实时展示首次建索引与单页拉取/解压的来源、阶段、已缓冲字节、百分比进度条与耗时；本地或缓存命中页面零额外干扰。

### 2. 响应式书架、阅读历史时间线与 Komga 客户端生态

- **全端自适应书架（`/`）与昼夜图标统一**：默认自动跟随系统日/夜模式（可手动固定），顶栏与浏览器标签页（含 `/reader/*` 阅读器）统一按书架主题切换黑底白鱼 / 白底线稿鱼标识；针对 `360px ~ 412px` 手机竖屏深度适配，零挤压裁切，空闲状态零后台轮询。
- **阅读历史时间线（`/history`）**：按本机自然日分组（`今天` / `1~6 天前` / `YYYY-MM-DD`）记录阅读中与已读完卷；同一天连读同一部作品的多卷自动折叠并可一键展开「当天另有 N 卷」，支持按书名/卷名检索与分页加载。
- **收藏夹（`/collections`）与阅读列表（`/readlists`）**：完整持久化存储与 Komga REST CRUD，支持手动排序、作品/单卷页快捷加入并与 Koharia 双向同步。
- **多版本切换与 Bangumi 油猴脚本**：支持按 Bangumi 排名排序与「合并相同 Bangumi ID」（仅网页书架生效）；作品页提供默认折叠的多版本切换面板，跨库卡片与详情页清晰标注书库与文件来源；内置 `/bgm/{subjectID}` 直达入口与个人账号页（`/account`）一键安装的 Bangumi 油猴脚本（`.user.js`），浏览 `bgm.tv` 时自动探测本库收录状态并直达。
- **深度兼容 Koharia 与 Mihon**：支持 `(<q>) OR tag:(<q>)` 联合检索、Komga 字段检索与 `relevance` 排序、90 天进度重置墓碑保护、轻量 Komga SSE 事件流（`/api/v1/sse/v1/events`）、`/api/v2/authors` 分页作者接口与 HTTP `gzip` 压缩，数十万册大库依然流畅同步。
- **墨水屏与 OPDS v1.2 + OPDS-PSE 双入口在线页流**：**封闭式墨水屏阅读器**（支持安装 KOReader 的设备）推荐使用 **[KOReader](https://koreader.rocks/) + [`opdsforcomic.koplugin`](https://github.com/hugo1120/opdsforcomic.koplugin/releases)**（[@hugo1120](https://github.com/hugo1120) 开发的漫画 OPDS 插件，接入 `/api/opds/v1.2`，支持零提前建索引、点击章节直读、章末自动切下一章、前后 5 章导航与双向同步进度）；**性能较强的 Android 墨水屏设备**推荐使用 **[Koharia](https://github.com/Mister-album/Koharia)** 并开启**无动画模式**；同时提供标准 Komga OPDS v1.2（`/opds/v1.2/catalog`）与 `?convert=jpeg` 按需转码。

### 3. 多存储源共存、分层联合库与自动化流水线

<p align="center">
  <img src="docs/images/showcase-admin.jpg" alt="MangaAlpha 管理后台多源联合书库与 Bangumi 中文刮削中心实景" width="920">
</p>

- **115 网盘直连**：基于 115 Open API 获取目录与短期 `downurl`，通过 `Range` 分块缓存按页流式读取；从 `v1.x` 原地升级可 **100% 保留原有书库 ID、系列记录、阅读进度、归档索引与封面缓存**。
- **本地磁盘与 SMB 挂载目录**：支持 Linux 本地盘 / CIFS 挂载及 Windows 盘符 / UNC 路径；通过 `io.ReaderAt` 原生随机直读（自动跳过 Range 分块磁盘缓存），去除 SMB 多余 `stat` 开销并自动忽略系统垃圾文件，内置 **空挂载点防误删保护**。
- **多账号 WebDAV（CloudDrive2 / OpenList / 群晖 / Nextcloud）**：支持多账号配置、令牌桶请求限速（同时约束 `PROPFIND` 与 `Range` 读取，平稳适配云盘挂载防风控）、根目录空条目保护与自签 TLS 选项。
- **分层叠加联合库（OverlayFS 风格）**：将多个物理库（如本地/SMB → WebDAV → 115）按优先级叠合为单一逻辑库，自动按归一化相对路径对齐、卷级互补叠加与同名上层覆盖，读取失败按层透明容灾回退；阅读进度双向回写底层副本，解散联合库无损保留进度。
- **定时流水线与一键隐藏书库**：支持按本机时区每日/每周定时串行执行「扫库（联合库可勾选指定子库）→ 联合库合并刷新 → 多源元数据增量刮削 → 缺封面未匹配作品自动补首卷封面」；支持一键隐藏任意书库（保留全部数据与缓存，前台与客户端全链路隔离返回 `404`）。

### 4. 多源元数据刮削中心、Bangumi 授权检索与 NSFW 隔离

- **声明式元数据市场与离线 Sidecar 引擎（v3.2 新增）**：
  - 支持订阅标准 `mangaalpha-repo-v1`（`repo.json`）元数据市场或手动接入本地 SQLite 源，通过纯 Go 流式解压（`.zst` / `.gz` / `.zip`）导入 `mangaalpha_meta_v1` 单表或 `gallery_v1` 通用四表离线图库，并在旁构建只读 Sidecar 索引（FTS5 + SHA-1 指纹召回）；同时支持解析归档内嵌的 `ComicInfo.xml` 与 `galleryinfo.txt` 元数据。
  - 内置中文标签翻译词典（`tag_translation_text_v1`，支持纯中文 `zh` / 双语 `bilingual` / 原文 `raw` 展示模式）；每个顶层书库（含联合书库）可自定义 `#1 → #2 → ...` **多源顺延优先级链**。
  - 所有离线源均未命中时自动由已启用的**社区脱敏映射包（`mangaalpha_mapping_v1`）**兜底，后台支持预览并导出严格剥离本地路径、私有 IP、凭据与 URL 的 `.ndjson` 映射包（默认严格排除 NSFW）；默认纯离线极速匹配，仅在显式开启时才允许单线程防封熔断低速在线补全。详见 [docs/METADATA_SOURCES.md](docs/METADATA_SOURCES.md)。
- **Bangumi 离线快照（默认中文简介增强镜像）+ 授权实时检索**：
  - 后台一键下载校验离线索引并本地秒级批量匹配，默认接入与官方 100% 同构的 **中文简介增强镜像（[`xJogger/bangumi-archive-zh`](https://github.com/xJogger/bangumi-archive-zh)）**，全站非 NSFW 外文简介已翻译为简体中文；也可随时切回 `bangumi/Archive` 官方原版或自定义兼容源，支持每周/每月定时检查更新并自动增量 diff 同步未锁定字段。
  - 作品详情页支持 `api.bgm.tv/v0` 实时搜索或粘贴 Subject ID / 链接精准绑定，并支持切换至已就绪的扩展离线源检索绑定；当已绑定 Bangumi OAuth / PAT 账号且开启 `show_nsfw` 时，自动携带用户授权检索 `api.bgm.tv` 的 NSFW 条目并拉取对应封面（若 Bangumi NSFW 封面因权限受限不可用且已配置就绪的扩展离线图库，可自动按只读方式从离线图库保底显示封面而不改写 Bangumi 绑定）；深度兼容双序方括号、`x` 合著作者、中英混排、扫图组噪音清洗与 `台版 / 港版 / 番外篇` 子目录父级回溯。
  - 支持独立专用代理（`http` / `https` / `socks5` / `socks5h`），统一加速 Bangumi、元数据市场/封面抓取与 GitHub 更新检测，**不影响 115 与 WebDAV 直连流量**。
- **NSFW 精准分级与默认隐藏**：严格依据 Bangumi 官方 `nsfw: true` 字段与扩展源分级规则（如 `gallery_v1` 严格按 `Non-H` 分类豁免，辅以未命中目录成人关键词识别与手动锁定）标记，默认隐藏；支持按用户授权与个人开关独立控制。
- **多用户分权体系、跨平台更新检测与 AI Agent API**：支持 1 个主管理员 + 多个普通用户（`argon2id` 密码哈希），支持按账号精细分配书库白名单与 NSFW 访问权限；每位用户拥有完全独立的阅读进度、阅读历史、收藏夹、阅读列表、阅读器偏好、个人 API Key 与 Bangumi 进度追踪器，兼容 Session Cookie、`HTTP Basic Auth`、`Bearer` 与 `X-API-Key`；内置 24 小时跨平台更新检测（Android 端支持 `SHA256SUMS` 校验与应用内 `PackageInstaller` 安装）、全平台白名单脱敏诊断日志导出，以及管理员专属 `/api/agent/v1` AI Agent 批量治理接口（见 [docs/AGENT_API.md](docs/AGENT_API.md)）。

---

## 尚不支持

- 固实（Solid）RAR、分卷 RAR、7z（非固实加密 ZIP / RAR4 / RAR5 已支持在网页阅读器输入并记住解压密码，同文件夹自动复用并在后台集中查看或清空）
- PDF 以及 `epub`、`mobi` 等流式排版电子书格式（当前主动排除以保持纯 Go 按需流式读取边界清晰；扫描图片打包型 PDF 仅待后续评估远程 `Range` 按页解析可行性）
- 对远程网盘或本地磁盘上的原始文件执行上传、删除、移动或重命名（书库始终只读访问你的漫画源文件）
- 公开自助注册（普通用户均由管理员在后台统一创建与授权）
- 上游 `downurl` 或 WebDAV 服务端不支持 HTTP `Range` 时静默整包下载（会明确返回 `501 RANGE_NOT_SUPPORTED`）

---

## 快速开始

**下载并启动 → 设置管理员 → 添加书库并扫描 → 开始阅读**

### 方式一：Windows 下载即用

1. 从 [Releases](https://github.com/xJogger/MangaAlpha/releases/latest) 下载 `_windows_amd64.zip` 压缩包（例如 `mangaalpha_v3.4.0_windows_amd64.zip`）并解压。
2. 双击运行 `mangaalpha.exe`（无控制台黑窗，自动常驻系统托盘）后，默认浏览器会自动打开 `http://127.0.0.1:25600/` 并引导完成首次管理员用户名与密码设置；点击托盘小鱼图标可随时打开书架，右键菜单可打开管理后台、数据目录、运行日志（`mangaalpha.log`）或安全退出服务。如需以无托盘命令行/Windows 服务模式运行，可附加 `--no-tray` 参数。
3. 数据默认存储在 `%LOCALAPPDATA%\fake-komga-115\data`（保持兼容，原地升级无需迁移目录）。

### 方式二：Linux / macOS 二进制运行

```bash
chmod +x mangaalpha
./mangaalpha
```

浏览器打开 `http://127.0.0.1:25600/` 设置管理员账号密码后，进入右上角「管理后台」（`/admin`）添加存储源与书库即可。

### 方式三：Docker 部署

若需读取宿主机本地磁盘或宿主机已挂载的 SMB 共享目录（如 `/share/manga`），将其以 `:ro` 只读方式映射进容器即可：

```bash
docker run -d \
  --name fake-komga-115 \
  --restart unless-stopped \
  -p 25600:25600 \
  -v fake-komga-115-data:/data \
  -v /share/manga:/manga:ro \
  -e TZ=Asia/Shanghai \
  blacktal/fake-komga-115:v3.4.0
```

> 从 `v2.3.0` 起，GitHub Release 桌面/服务端压缩包与本地构建产物名统一为 `mangaalpha`；Docker 镜像名 `blacktal/fake-komga-115`、容器入口、`FK115_*` 环境变量与既有默认数据路径保持不变。已有容器升级时请保留 `/data` 数据卷，建议固定版本标签升级。

### 方式四：从源码构建

完整构建（含内置 Suwayomi 阅读器静态包与 `tsc --noEmit` 校验）需要 Go 1.26+、Node.js 24+、Python 3 与 `pnpm`（或 `corepack`）：

```bash
git clone https://github.com/xJogger/MangaAlpha.git
cd MangaAlpha
make build
./mangaalpha
```

### Android 独立版

从 [Releases](https://github.com/xJogger/MangaAlpha/releases/latest) 下载官方签名包 `mangaalpha-arm64.apk`（ARM64）。它在手机本机独立运行，支持手机目录（SAF）、115 和 WebDAV，内置原生「手机端设置」页（服务状态、应用内 `SHA256SUMS` 校验更新、可选开启局域网漫画服务器、脱敏诊断日志导出）与状态栏/安全区自适应，不与家里的服务器自动同步。详情见 [docs/ANDROID.md](docs/ANDROID.md)。

**使用提示**：
- 在手机上连接同机 **CloudDrive2** 扫描大库或持续阅读时，建议在系统省电 / 应用管理中将 **CloudDrive2** 与 **MangaAlpha** 两者的后台权限均设为「无限制」，避免系统休眠冻结进程；
- 小米 MIUI / HyperOS 等系统默认单行折叠静音常驻通知，下拉或长按通知即可展开「手机端设置 / 停止」快捷按钮，也可在网页「管理后台」顶栏直接进入；
- 本地 SAF 目录与 WebDAV 归档读取已做文件大小校准、瞬态重试与 ZIP/RAR 魔数回退加固，如遇异常可在「手机端设置」开启详细排障并导出脱敏诊断日志反馈。

### 命令行参数与环境变量

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `--host` | `FK115_HOST` | `0.0.0.0` | HTTP 监听地址 |
| `--port` | `FK115_PORT` | `25600` | HTTP 监听端口 |
| `--data-dir` | `FK115_DATA_DIR` | Windows: `%LOCALAPPDATA%\fake-komga-115\data`<br>其他平台: `./data` | SQLite 数据库、Bangumi 索引、封面与缓存目录 |
| `--open-browser` | `FK115_OPEN_BROWSER` | Windows: `true` / 其他: `false` | 启动后是否自动打开浏览器 |
| `--no-tray` | `FK115_NO_TRAY` | `false` | 仅 Windows 生效：禁用系统托盘图标，以无界面后台模式运行 |
| `--version` | — | — | 打印版本号并退出 |

> 公网反代或无人值守首次部署另支持 `FK115_TRUSTED_PROXIES`、`FK115_ADMIN_USERNAME`、`FK115_ADMIN_PASSWORD`，详见 [docs/REVERSE_PROXY.md](docs/REVERSE_PROXY.md)。

---

## 存储源、刮削与客户端使用提示

### 配置三类存储源
1. **115 网盘**：打开 [OpenList 授权工具](https://api.oplist.org/)，选择 115 网盘 OAuth2 授权登录，勾选「使用 OpenList 提供的参数」获取 Token，将 `Refresh Token` 与 `Access Token`（可选）粘贴到「管理后台 → 存储源 → 115 开放平台令牌」保存并验证，随后在「书库管理」添加目录并扫描（切勿公开分享 Token）。详见 [OpenList 115 授权说明](https://doc.oplist.org/guide/drivers/115_open)。
2. **服务器本地 / SMB 挂载目录**：无需预配账号。Linux 推荐通过 `cifs` 挂载 NAS 共享（如 `/share/manga`），Windows 可用映射盘符（`Z:\Manga`）或 UNC 路径（`\\nas\manga`）。
3. **WebDAV（CloudDrive2 / OpenList / NAS）**：
   - **CloudDrive2** 默认 WebDAV 根地址通常为 `http://<IP>:19798/dav`；
   - **OpenList** 默认 WebDAV 根地址通常为 `http://<IP>:5244/dav`；
   - 挂载云盘时建议将 WebDAV 账号「请求限速」设为 `2 ~ 4` req/s 平稳防风控。

### 元数据刮削与 Bangumi 链接说明
- **推荐流程**：首次在「管理后台 → 元数据刮削」点击「一键下载并构建离线索引库」（默认 Bangumi 中文简介增强镜像），按需在社区元数据市场订阅并同步扩展离线源/词典/映射包，就绪后启动「批量自动刮削」；后续可用「重试未命中」补全未匹配条目。
- **路由器透明代理与显式代理**：外部元数据请求原生兼容标准透明代理 Fake-IP（`198.18.0.0/15`、`fc00::/18`）与双栈 DNS 屏蔽占位；若使用非标 Fake-IP 段或自建内网镜像，可在「元数据刮削 → 网络代理」中配置显式 HTTP / SOCKS5 代理。
- `+` 或 `&` 连接的标题仅在完整标题吻合或各部分同属一部作品正式标题/别名时才参与自动匹配，合集不会误匹配单部作品，仍可手动搜索或填条目 ID 绑定。
- **为什么个别作品点击 `bgm.tv` 链接提示“数据库中没有查询您所指定的条目”？** 这是 Bangumi 官网未登录访问限制，在浏览器登录 `bgm.tv` 账号后重新打开即可正常查看。

### 连接客户端（Koharia / Mihon / KOReader）与 AI Agent
- **[Koharia](https://github.com/Mister-album/Koharia)（推荐 Android Komga 客户端 / Android 墨水屏）**：原生 Komga 支持，完整兼容本服务的搜索语法、收藏夹、阅读列表与 SSE 进度同步；**性能较强的 Android 墨水屏设备**同样推荐使用 Koharia，并在设置中开启**无动画模式**以获得更佳的墨水屏翻页体验；新增 Komga 服务器填入 `http://<服务器IP>:25600` 与账号密码（或后台生成的 API Key）即可使用。
- **[Mihon](https://mihon.app/)**：安装 Komga 扩展，填入 `http://<服务器IP>:25600` 与账号密码。
- **[KOReader](https://koreader.rocks/)（封闭式墨水屏阅读器推荐）**：
  - **封闭式墨水屏阅读器**（支持安装 KOReader 的设备）推荐使用 **[KOReader](https://koreader.rocks/) + [`opdsforcomic.koplugin`](https://github.com/hugo1120/opdsforcomic.koplugin/releases)**（[@hugo1120](https://github.com/hugo1120) 开发的漫画 OPDS 插件），在 `OPDS catalog (Comic)` 中填入 `http://<服务器IP>:25600/api/opds/v1.2` 与账号密码，即可获得点击章节直读、章末自动切下一章、前后 5 章导航、零提前建索引与双向同步阅读进度体验；
  - 同时也提供标准 Komga OPDS v1.2 入口 `http://<服务器IP>:25600/opds/v1.2/catalog`（支持 OPDS-PSE 在线页流与 OpenSearch 搜索），兼容其他标准 OPDS 漫画客户端。
- **AI Agent 治理**：携带 `Authorization: Bearer <API_KEY>` 请求 `GET /api/agent/v1/manifest` 即可发现全部管理能力，详见 [docs/AGENT_API.md](docs/AGENT_API.md)。

---

## 从 `v1.x` 升级与备份

**v2 是一次不兼容升级**：首次访问需设置管理员账号密码并在客户端补填认证信息；v1 的外部跨域（CORS）白名单已移除，请改用内置书架与阅读器；NSFW 系列默认隐藏，可在后台随时调整。

1. 停止旧服务并完整备份数据目录（Docker 为 `/data` 卷，备份含凭据请妥善保管）。
2. 以**同一个数据目录**启动 v2，首次启动自动迁移 SQLite 并保留原有 115 书库 ID、阅读进度、索引与封面，无需重扫。
3. 在网页完成管理员初始化并确认书库与进度无误后再配置客户端；若需回退，请退出 v2 后用备份恢复。

Linux systemd 升级示例：

```bash
systemctl --user stop fake-komga-115
cp -a data /path/to/backup/fake-komga-115-before-v2
# 替换二进制，保留原来的 --data-dir
systemctl --user start fake-komga-115
```

---

## 项目文档

- [docs/ANDROID.md](docs/ANDROID.md)：Android 独立版安装、SAF 目录权限、局域网服务与签名构建说明
- [docs/AGENT_API.md](docs/AGENT_API.md)：AI Agent 专用书库管理接口（`/api/agent/v1`）参考手册
- [docs/METADATA_SOURCES.md](docs/METADATA_SOURCES.md)：开放元数据市场（`repo.json`）、离线索引规范、标签翻译词典、脱敏映射包与 SSRF 防护规范
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)：系统架构、鉴权、多源流式读取、多源元数据中心与内置阅读器原理
- [docs/REVERSE_PROXY.md](docs/REVERSE_PROXY.md)：公网部署安全清单与 Caddy / Nginx / NPM / Cloudflare Tunnel HTTPS 反向代理示例
- [docs/RELEASING.md](docs/RELEASING.md)：维护者的发版、产物验证与 Docker `latest` 切换流程
- [AGENTS.md](AGENTS.md)：面向开发者与 AI 编程助手的仓库规范与构建指引
- [SECURITY.md](SECURITY.md)：安全模型与敏感信息保护边界

---

## 致谢上游与开源生态

**MangaAlpha** 站在众多优秀开源项目与开放社区的肩膀上，在此向所有上游作者与维护者致以诚挚的感谢：

- **[Suwayomi / Suwayomi-WebUI](https://github.com/Suwayomi/Suwayomi-WebUI)**（MPL-2.0）：提供了体验极其出色的现代化漫画 Web 阅读器前端实现，本项目的内置阅读器（`/reader/*`）基于其源码适配构建。
- **[Komga](https://komga.org/)（[@gotson](https://github.com/gotson)）**：定义了清晰、成熟的漫画数字书库领域模型与 REST API 规范，让跨客户端生态互联成为可能。
- **[Bangumi 番组计划](https://bgm.tv/) 与 [`bangumi/Archive`](https://github.com/bangumi/Archive)**：感谢 Sai 与全体番组计划维基编辑者多年积累的高质量中文 ACGN 资料库，以及官方持续开放的离线数据快照与 Open API。
- **[OpenList](https://github.com/OpenListTeam/OpenList) 与 [`OpenListTeam/115-sdk-go`](https://github.com/OpenListTeam/115-sdk-go)**：提供了稳定易用的 115 开放平台 Go SDK、OAuth 授权工具以及优秀的 WebDAV 多存储聚合实践。
- **[CloudDrive2](https://www.clouddrive2.com/)**：提供了高性能的云盘挂载与标准 WebDAV 服务支持，让多种网盘通过 WebDAV 流式接入漫画书库更加便捷。
- **[Mihon](https://mihon.app/) 与 [Koharia](https://github.com/Mister-album/Koharia)**：优秀的 Android 漫画阅读客户端（Koharia 原生支持 Komga 生态），推动了 Komga 客户端体验的普及与打磨。
- **[KOReader](https://koreader.rocks/) 与 [`opdsforcomic.koplugin`](https://github.com/hugo1120/opdsforcomic.koplugin)**（[@hugo1120](https://github.com/hugo1120)）：为墨水屏设备带来了体验极佳的 OPDS 逐页流式漫画阅读、预取低内存解码与章节导航能力。
- **Go 核心开源库**：感谢 [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite)（纯 Go 无 CGO SQLite 引擎）、[`klauspost/compress`](https://github.com/klauspost/compress)（纯 Go Zstandard 流式解压）、[`nwaples/rardecode`](https://github.com/nwaples/rardecode)（纯 Go RAR4/RAR5 解码器）、[`go-chi/chi`](https://github.com/go-chi/chi) 与 Go 标准库及 `golang.org/x` 团队。
- **[霞鹜文楷 (LXGW WenKai)](https://github.com/lxgw/LxgwWenKai)**（SIL OFL 1.1）：大版本代号展示所用中文字形子集取材自霞鹜文楷开源字体。
- **[OpenCC](https://github.com/BYVoid/OpenCC)**（Apache-2.0）：繁简与日文异体字搜索折叠字表（`internal/cjk/table_data.go`）基于 OpenCC 开放词典生成。

<p align="center">
  <img src="docs/images/mangaalpha-stamp.png" alt="MangaAlpha 的 Café Alpha 风格线稿小鱼印章" width="96">
</p>

---

## 开源许可

- 主服务代码、内置书架与管理后台前端遵循 [MIT License](LICENSE) 开源。
- 内置网页阅读器（`web/reader/` 与 `/reader/*` 构建产物）基于上游 [Suwayomi-WebUI](https://github.com/Suwayomi/Suwayomi-WebUI) 修改集成，该子模块与衍生适配文件遵循 [Mozilla Public License 2.0 (MPL-2.0)](web/reader/LICENSE)。
- 内嵌的大版本代号字形子集（`internal/httpserver/static/ui/codename-subset.woff2`）源自 [LXGW WenKai](https://github.com/lxgw/LxgwWenKai)，遵循 [SIL Open Font License 1.1](internal/httpserver/static/ui/LICENSE-LXGW-WenKai-OFL.txt)。
- CJK 繁简与日文异体字折叠字表（`internal/cjk/table_data.go`）源自 [OpenCC](https://github.com/BYVoid/OpenCC) 开源词典，遵循 Apache License 2.0。
