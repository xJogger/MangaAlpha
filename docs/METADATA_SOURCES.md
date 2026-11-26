# 开放元数据数据源规范 (`docs/METADATA_SOURCES.md`)

本文档定义 **MangaAlpha** 外部元数据市场、离线索引库、标签翻译词典、社区脱敏映射包与归档内嵌元数据的开放技术规范与安全保护机制，面向数据源维护者、词典/映射贡献者及第三方工具开发者。

---

## 1. 概述与设计原则

1. **纯离线优先**：大规模批量刮削默认仅读取本地 SQLite（及随库构建的只读 Sidecar FTS5 + SHA-1 索引）与本地社区映射包，避免触发上游站点 API 限流或封禁。
2. **多源顺延链（Per-Library Priority Chain）**：每个顶层书库（含联合书库，自动跳过联合子库）可独立配置 `sourceIds` 优先级顺序（未配置时默认使用内置 Bangumi）；按 `#1 → #2 → ...` 顺序尝试匹配，若全部离线源未命中，则自动由已启用的社区映射包兜底。
3. **仅按需低速在线补全**：仅当管理员显式开启全局允许与书库级低速在线补全开关时，才对未命中条目执行单线程限速补全，并内置指数退避熔断保护。
4. **隐私与社区共建**：所有导出至社区的映射包必须经过严格脱敏校验（剥离本地路径、私有 IP、Token 与 URL），默认严格排除 NSFW 条目。

---

## 2. 仓库清单规范：`mangaalpha-repo-v1`

MangaAlpha 采用声明式元数据市场 `repo.json` 规范。管理后台提供「填入社区推荐仓库地址」快捷预填功能（默认指向 `https://raw.githubusercontent.com/BlackMuseCode/mangaalpha-metadata-repo/main/repo.json`），需管理员确认后才会订阅拉取。

- **体积上限**：`4 MiB`（`4194304` 字节）
- **JSON 编码**：UTF-8，`Content-Type: application/json`

### 2.1 顶层字段

| 字段名 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `schema` | `string` | 是 | 固定为 `"mangaalpha-repo-v1"` |
| `name` | `string` | 否 | 仓库显示名称 |
| `description` | `string` | 否 | 仓库简介 |
| `homepage` | `string` | 否 | 仓库主页 URL（仅允许 `http://` 或 `https://`） |
| `version` | `string` | 否 | 清单版本号（如 `"2026.11.25.1"`，优先于 `updatedAt` 展示） |
| `updatedAt` | `string` | 否 | 清单更新时间（建议 RFC3339） |
| `sources` | `RepoSource[]` | 是 | 数据源声明数组 |

### 2.2 `sources[]` 字段定义

| 字段名 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `id` | `string` | 是 | 源唯一 ID；仅允许 `[a-zA-Z0-9._-]`，长度 `1..128`，禁止 `.` / `..` 及仓库内重复 |
| `kind` | `string` | 是 | 源类型：`bangumi_archive` / `offline_sqlite` / `tag_dictionary` / `mapping_pack` |
| `schema` | `string` | 否 | 数据格式标识，如 `mangaalpha_meta_v1`、`mangaalpha_meta_v2`、`komga_sqlite_v1`、`gallery_v1`、`tag_translation_text_v1`、`mangaalpha_mapping_v1`（`offline_sqlite` 留空时自动探测） |
| `name` | `string` | 是 | 源显示名称 |
| `description` | `string` | 否 | 源描述信息 |
| `manifestUrl` | `string` | 否 | 清单地址（如 `bangumi_archive` 的 `aux/latest.json`，需 `http/https`） |
| `downloadUrl` | `string` | 否 | 离线包直链 URL（支持 `.zst`/`.zstd`、`.gz`/`.gzip`、`.zip` 或裸文件，需 `http/https`） |
| `releaseApiUrl` | `string` | 否 | GitHub Release API（或同构 JSON）端点，用于滚动 Tag（如 `nightly`）基于 `updated_at` 与 `size` 检测更新 |
| `assetName` | `string` | 否 | 配合 `releaseApiUrl` 匹配指定发布资产文件名 |
| `compression` | `string` | 否 | 压缩格式：`auto`（或留空，按 URL 后缀识别）、`zst`/`zstd`、`gz`/`gzip`、`zip`、`none`/`raw` |
| `sizeHintBytes` | `int64` | 否 | 预估文件大小（字节，非负整数） |
| `defaultTagDictId` | `string` | 否 | 默认关联的 `tag_dictionary` 源 `id` |
| `nsfwRule` | `string` | 否 | NSFW 判定规则提示（如 `non_h_safe`、`non_h_exempt`） |
| `autoUpdateDefault` | `string \| bool` | 否 | 默认自动更新周期：`"manual"` / `"weekly"` / `"monthly"`（向后兼容旧布尔值 `true` → `"weekly"`、`false` → `"manual"`） |
| `homepage` | `string` | 否 | 源项目主页 URL（需 `http/https`） |
| `contributeUrl` | `string` | 否 | 映射贡献 / PR / Issue 链接 URL（需 `http/https`） |
| `default` | `bool` | 否 | 是否标记为默认推荐源 |

### 2.3 完整示例

```json
{
  "schema": "mangaalpha-repo-v1",
  "version": "2026.11.25.1",
  "name": "社区开放元数据仓库",
  "description": "提供离线索引库、标签中文词典与脱敏映射包",
  "homepage": "https://github.com/BlackMuseCode/mangaalpha-metadata-repo",
  "updatedAt": "2026-11-25T00:00:00Z",
  "sources": [
    {
      "id": "bgm-zh",
      "kind": "bangumi_archive",
      "name": "Bangumi 中文简介增强镜像",
      "manifestUrl": "https://example.invalid/bgm/latest.json",
      "autoUpdateDefault": "weekly",
      "default": true
    },
    {
      "id": "open-meta-v1",
      "kind": "offline_sqlite",
      "schema": "mangaalpha_meta_v1",
      "name": "社区开放漫画离线库",
      "downloadUrl": "https://example.invalid/open-meta.sqlite.zst",
      "releaseApiUrl": "https://api.example.invalid/repos/org/repo/releases/latest",
      "assetName": "open-meta.sqlite.zst",
      "compression": "zst",
      "sizeHintBytes": 52428800,
      "defaultTagDictId": "zh-tags-v1",
      "nsfwRule": "non_h_safe"
    },
    {
      "id": "zh-tags-v1",
      "kind": "tag_dictionary",
      "schema": "tag_translation_text_v1",
      "name": "标签中文翻译词典",
      "downloadUrl": "https://example.invalid/db.text.json.gz",
      "compression": "gz"
    },
    {
      "id": "community-mappings-v1",
      "kind": "mapping_pack",
      "schema": "mangaalpha_mapping_v1",
      "name": "社区脱敏映射包",
      "downloadUrl": "https://example.invalid/mappings.ndjson.gz",
      "compression": "gz",
      "contributeUrl": "https://github.com/BlackMuseCode/mangaalpha-metadata-repo/pulls"
    }
  ]
}
```

---

## 3. 官方极简社区 SQLite 单表标准：`mangaalpha_meta_v1`

`mangaalpha_meta_v1` 面向开源社区与个人自建静态书目索引，只需一张 `entries` 表即可完整接入 MangaAlpha。

### 3.1 建表 SQL 示例

```sql
CREATE TABLE entries (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  title_alt TEXT DEFAULT '',
  authors TEXT DEFAULT '[]',
  summary TEXT DEFAULT '',
  tags TEXT DEFAULT '[]',
  category TEXT DEFAULT '',
  cover_url TEXT DEFAULT '',
  rating REAL DEFAULT 0,
  is_nsfw INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER DEFAULT 0,
  extra_json TEXT DEFAULT '{}'
);
```

### 3.2 字段语义与兼容性说明

- `id`：条目唯一主键（字符串，如 `"m-1024"` 或数字字符串）。
- `title` / `title_alt`：主标题与副标题（原名 / 别名），均参与 Sidecar FTS5 与繁简/日文异体字折叠索引。
- `authors` / `tags`：支持 JSON 字符串数组（推荐，如 `["作者A","作者B"]`），也兼容 `,`、`;`、`|`、`\n` 分隔文本，读取时自动清洗去重。
- `category`：作品分类文本（如 `"Manga"`、`"Doujinshi"`、`"Non-H"` 等）。
- `cover_url`：封面 URL；若包含 40 位十六进制 SHA-1 摘要（如 `/xx/yy/<40hex>-...` 或 `<40hex>.jpg`），Sidecar 会自动提取并写入 `thumb_sha1` 索引。
- `is_nsfw`：`0` 表示常规作品，非 `0` 表示 NSFW。
- `updated_at`：Unix 秒级时间戳。
- `extra_json`：可选 JSON 对象，可存放 `token`、`source_url`（或 `sourceUrl`）等扩展信息。

### 3.3 流式解压与只读 Sidecar 机制

- 下载同步支持 **纯 Go 流式解压**（`.zst`/`.zstd`、`gzip`、`.zip` 自动提取首个 `.sqlite`/`.sqlite3`/`.db` 并跳过 `__MACOSX/`，以及未压缩 `.sqlite`），解压输出受 **8 GiB**（Web 流式上传支持 **10 GiB**）安全上限约束，采用临时文件校验后原子替换，失败绝不破坏旧库。
- 服务端严格以 `mode=ro&_pragma=query_only(1)`（无 `-wal` 时附加 `immutable=1`）只读打开上游 SQLite，绝不修改上游文件；并在旁边构建伴随 Sidecar 索引（`ext_meta`、`ext_entries`、`ext_entry_keys`、`ext_fts` FTS5 虚表），实现 `< 5ms` 标题召回、多别名检索与 SHA-1 指纹反查。

### 3.4 向后兼容扩展规范：`mangaalpha_meta_v2`、可选列、`cover_blob` 与 `books` 分表

`mangaalpha_meta_v2` 是 `mangaalpha_meta_v1` 的完全向后兼容扩展别名：未声明扩展列时退化为标准 `mangaalpha_meta_v1` 单表，检测到 `entries` 存在扩展列/`cover_blob`、`books` 分表或 `meta`/`ext_meta` 表中 `key='schema'` 为 `'mangaalpha_meta_v2'` 时自动识别为 `mangaalpha_meta_v2`。

#### 可选 `entries` 扩展列

| 列名 | 类型 | 说明 |
| --- | --- | --- |
| `status` | `TEXT` | 连载状态（如 `ONGOING`、`ENDED`、`HIATUS`、`ABANDONED`） |
| `title_sort` | `TEXT` | 排序标题（参与 Sidecar 别名精确检索与 FTS5） |
| `publisher` | `TEXT` | 出版社 / 发行方 |
| `language` | `TEXT` | 语言代码（如 `zh`、`ja`、`en`） |
| `release_date` | `TEXT` | 出版/发布日期（建议 `YYYY-MM-DD`） |
| `total_book_count` | `INTEGER` | 计划总卷数 |
| `age_rating` | `INTEGER` | 年龄分级；`>= 18` 时自动保守标记 `is_nsfw = true` |
| `reading_direction` | `TEXT` | 阅读方向（如 `RIGHT_TO_LEFT`、`LEFT_TO_RIGHT`、`VERTICAL`、`WEBTOON`） |
| `links_json` | `TEXT` | 外部链接 JSON 数组，格式 `[{"label":"Bangumi","url":"https://..."}]` |
| `sharing_labels_json` | `TEXT` | 共享标签 JSON 数组或分隔文本（含成人关键词时标记 NSFW） |
| `cover_blob` | `BLOB` | 系列封面二进制图片（JPEG/PNG/WebP/GIF，上限 16 MiB） |

当 `entries.cover_url` 为空且存在非空 `cover_blob` 时，Sidecar 仅编码虚拟引用 `sqlite-blob://<sourceID>/open-entry/<entryID>`（不在同步阶段提取 BLOB 到磁盘），仅在封面首次访问时按需懒加载、校验证书魔数并原子落盘缓存。

#### 可选 `books` 分表结构

```sql
CREATE TABLE books (
  series_entry_id TEXT NOT NULL,
  book_key TEXT NOT NULL,
  file_name TEXT DEFAULT '',
  number TEXT DEFAULT '',
  number_sort REAL,
  title TEXT DEFAULT '',
  summary TEXT DEFAULT '',
  release_date TEXT DEFAULT '',
  authors_json TEXT DEFAULT '[]',
  tags_json TEXT DEFAULT '[]',
  isbn TEXT DEFAULT '',
  links_json TEXT DEFAULT '[]',
  cover_url TEXT DEFAULT '',
  cover_blob BLOB,
  PRIMARY KEY (series_entry_id, book_key)
);
```

- 当系列通过自动刮削或手工绑定命中含 `books` 分表的 `mangaalpha_meta_v1` / `v2` 条目时，系统会自动将旗下分册与本地单册按三层瀑布流对齐：
  1. 归一化文件名精确匹配（去归档扩展名、剥离常见方括号组名标签、繁简/日文异体字折叠）；
  2. 明确无歧义卷号匹配（识别 `Vol.XX`、`第XX巻/卷` 及结构化 `number`/`number_sort`，隔离章节与卷号防串书）；
  3. 归一化标题相似度或唯一单册（One-shot）安全一对一对齐。
- 写入 `book_metadata` 时严格遵守 9 个分册字段锁（`TitleLock`、`SummaryLock`、`NumberLock`、`NumberSortLock`、`ReleaseDateLock`、`ISBNLock`、`AuthorsLock`、`TagsLock`、`LinksLock`），自动刮削严格跳过 `match_origin IN ('manual', 'agent')` 的已锁定单册；`books.cover_blob`（当 `books.cover_url` 为空时）同样编码为 `sqlite-blob://<sourceID>/open-book/<seriesEntryID>:<bookKey>` 懒加载缓存，非 Bangumi 外部源写入期间始终保留系列已有 `bgm_subject_id`。

---

## 4. Komga 离线数据库直连规范：`komga_sqlite_v1`

MangaAlpha 原生支持将导出的只读 Komga `database.sqlite` 作为离线元数据与封面来源，支持 **Web 流式上传** 与 **本机 `localPath` 只读挂载** 两种接入方式。服务端使用 `ProbeSQLiteFile` 检测包含 `SERIES`、`SERIES_METADATA`、`BOOK`、`BOOK_METADATA` 等核心表时自动识别为 `komga_sqlite_v1`。

### 4.1 Web 流式大文件上传接入

- **接口端点**：`POST /admin/api/metadata/sources/upload`（受 `requireAdmin`、同源 CSRF 与 `X-FK115-User-Id` 校验保护）。
- **流式协议**：使用 `multipart/form-data`（可传 `name`、`sourceId`、`description`、`enabled`、`tagDictSourceId`、`tagDisplayMode`、`autoUpdate` 与 `file` 字段）；服务端基于流式 `MultipartReader` 逐块读写落盘（**不触发 `ParseMultipartForm` 双份落盘与内存缓冲**），支持直接上传 `.sqlite` / `.sqlite3` / `.db` 以及 `.zip` / `.gz` / `.zst` 压缩包，单文件流式上限 **10 GiB**。
- **格式校验**：落盘前校验 SQLite3 文件头魔数 `SQLite format 3\x00` 与 `ProbeSQLiteFile` 结构合法性，非法格式直接拒绝并原子清理临时文件；支持创建新源（`201 Created`）或覆盖更新已有源并自动重建 Sidecar 索引。
- **反向代理配置提示**：上传大型 Komga `database.sqlite`（数百 MB 至数 GB）时，请务必调大前置反向代理的请求体上限与超时设置，例如：
  - **Nginx**：`client_max_body_size 10G;` 并适当调高 `proxy_read_timeout` / `proxy_send_timeout`；
  - **Caddy**：在站点块配置 `request_body { max_size 10GB }`。

### 4.2 本机 `localPath` 挂载与 `-wal` Checkpoint 注意事项

- 支持在管理后台创建或编辑 `offline_sqlite` 源时填写绝对路径 `localPath`（禁止软链接逃逸与 `..` 路径穿越）。
- **WAL 日志与 Checkpoint 告警**：运行中的 Komga 可能将最新修改写入同目录的 `database.sqlite-wal` 尚未合并回主库。MangaAlpha 对只读挂载源采用安全只读打开策略（仅当 `-wal` 与 `-shm` 同时可读时附带 WAL 读取，若仅存在孤立 `-wal` 则以 `immutable=1` 打开避免在只读挂载点创建 `-shm` 报错）。同时源列表（`GET /admin/api/metadata/sources`）与概览（`GET /admin/api/metadata/overview`）会返回：
  - `schemaFormat`：识别出的数据库格式（如 `komga_sqlite_v1`、`mangaalpha_meta_v2`）；
  - `walWarning` / `walWarningBytes` / `walWarningMessage`：当同目录检测到非空 `-wal` 文件时置为 `true` 并提示管理员先停止 Komga 或执行 `PRAGMA wal_checkpoint(TRUNCATE);` 后再复制或同步，避免缺失最新增量数据。

### 4.3 系列/分册刮削与 BLOB 封面按需懒缓存

- **系列全量字段映射**：同步索引系列主标题、目录名、排序标题、`alternateTitles`、URL 路径末段、`authors`（保留 `writer`/`penciller` 等角色）、`genres`、`tags`、`sharingLabels`、`links`、`publisher`、`language`、`status`、`ageRating`、`readingDirection`、`totalBookCount`、`releaseDate` 与 `isNsfw`（尊重年龄分级、`AGE_RATING >= 18` 与分享标签判定，同时在系列写入时严格保留已有 `bgm_subject_id`）。
- **分册同步对齐**：刮削或手工绑定命中 Komga 系列时，自动使用与 `3.4` 相同的三层瀑布流（文件名 → 卷号 → 标题/单册）将 Komga `BOOK` / `BOOK_METADATA` 对齐写入 `book_metadata` 并严格遵守 9 项分册字段锁。
- **零预解压 BLOB 封面懒加载**：Sidecar 索引与系列/分册元数据仅保存虚拟引用：
  - 系列封面：`sqlite-blob://<sourceID>/komga-series/<thumbnailID>`（对应 `thumbnail_series`）
  - 分册封面：`sqlite-blob://<sourceID>/komga-book/<thumbnailID>`（对应 `thumbnail_book`）
  索引同步阶段绝不提取或转存任何 BLOB 图片到磁盘；仅当用户或阅读器实际请求某系列/分册封面，或管理员通过 `GET /admin/api/metadata/sources/{sourceID}/entries/{entryID}/cover` 预览条目封面时，才按需读取对应行 BLOB、校验 JPEG/PNG/WebP/GIF 魔数（上限 16 MiB）并以 `singleflight` 原子缓存到 `<DataDir>/thumbnails/extmeta/`，全程零外网请求、零 SSRF 误拦截。

---

## 5. 通用四表图库 SQLite 兼容规范：`gallery_v1`

`gallery_v1` 兼容开源社区广泛存在的四表归档图库离线数据库结构（由 `sqlite_master` 检测存在 `gallery` 表时自动识别，或显式配置 `schemaType = "gallery_v1"`）。

### 5.1 标准四表结构

```sql
CREATE TABLE gallery (
  gid INTEGER PRIMARY KEY,
  token TEXT,
  title TEXT,
  title_jpn TEXT,
  category TEXT,
  thumb TEXT,
  uploader TEXT,
  posted INTEGER,
  filecount INTEGER,
  filesize INTEGER,
  expunged INTEGER,
  removed INTEGER,
  replaced INTEGER,
  rating REAL,
  root_gid INTEGER
);

CREATE TABLE tag (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE TABLE gid_tid (
  gid INTEGER NOT NULL,
  tid INTEGER NOT NULL,
  PRIMARY KEY (gid, tid)
);

CREATE TABLE gallery_meta (
  key TEXT PRIMARY KEY,
  value TEXT
);
```

### 5.2 标签拆分与作者提取规则

`tag.name` 通常采用 `namespace:tag` 形式（通过 `gid_tid` 关联）：
- `artist:<name>` 与 `group:<name>`：自动提取并去重归入作品 `authors` 列表，不再混入普通标签；
- 其余 `namespace:tag`（如 `female:...`、`parody:...`、`language:...`、`other:...`）：保留在 `tags` 列表中，并可由关联的 `tag_dictionary` 按展示模式翻译。

### 5.3 严格的 NSFW 分类规则

为确保前台默认隐藏 NSFW 策略不发生漏判，`gallery_v1` 采用保守安全判定：
- **仅当 `gallery.category` 去除首尾空白并忽略大小写后严格等于 `"Non-H"` 时，判定 `isNsfw = false`**；
- 其他所有分类（包含 `Manga`、`Doujinshi`、`Artist CG`、`Game CG`、`Image Set`、`Cosplay`、`Western`、`Misc` 或空值）**一律默认标记 `isNsfw = true`**。

---

## 6. 离线标签翻译词典：`tag_translation_text_v1`

支持纯文本 JSON（`db.text.json`）或 Gzip 压缩（`db.text.json.gz`），解压上限为 `64 MiB`。

### 6.1 文件格式

```json
{
  "data": [
    {
      "namespace": "female",
      "data": {
        "glasses": { "name": "眼镜" },
        "schoolgirl uniform": { "name": "水手服" }
      }
    },
    {
      "namespace": "other",
      "data": {
        "full color": { "name": "全彩" }
      }
    }
  ]
}
```

### 6.2 查找与显示模式 (`tagDisplayMode`)

1. 带命名空间的标签（如 `female:glasses`）：精确按 `namespace` + 小写 `tag` 查表；
2. 无命名空间的裸标签（如 `full color`）：从非 `rows` 命名空间的汇总映射表中兜底查找。
3. 支持三种展示模式：
   - `zh`（默认）：命中则返回中文译名（如 `眼镜`），未收录保留原标签；
   - `bilingual`：命中且译名与原文不同时返回 `中文 (原文)`（如 `眼镜 (female:glasses)`）；
   - `raw`：始终保留原始标签。

---

## 7. 社区脱敏映射包：`mangaalpha_mapping_v1`

`mangaalpha_mapping_v1` 是轻量级换行分隔 JSON（`.ndjson` 或 `.ndjson.gz`，流式上限 `64 MiB`），用于在多个用户间安全共享“目录/作品名 → 目标元数据 ID”的人工校对成果。空映射包（`packs: []` 或空 `.ndjson`）可零报错正常同步（计为 `0` 条记录，但不会单独满足“存在已就绪离线源”的批量刮削前置校验）。

### 7.1 单行记录字段

导入时兼容 camelCase 与 snake_case 字段，导出统一输出标准 camelCase：

```json
{"schema":"mangaalpha_mapping_v1","key":"水星领航员","rawKey":"[天野梢] ARIA Vol.01","author":"天野梢","targetSource":"bangumi","targetId":"539","isNsfw":false,"origin":"manual"}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `schema` | `string` | 是 | 固定为 `"mangaalpha_mapping_v1"` |
| `key` | `string` | 是 | 清洗归一后的核心作品名（参与 `ExactTitleKey` 索引） |
| `rawKey` | `string` | 否 | 原始目录名或副标题键（同时参与 `SearchKey` 与 `ExactTitleKey` 索引） |
| `author` | `string` | 否 | 作者名，用于同标题消歧 |
| `targetSource` | `string` | 是 | 目标源标识：`"bangumi"`、`"gallery_v1"` 或 `"mangaalpha_meta_v1"` |
| `targetId` | `string` | 是 | 目标条目 ID |
| `isNsfw` | `bool` | 否 | 是否 NSFW；导出时默认严格排除 |
| `origin` | `string` | 否 | 绑定来源标识，如 `"manual"`、`"agent"`、`"auto"` |

### 7.2 四级双键匹配与作者消歧逻辑

映射索引（`MappingIndex.Lookup`）严格按以下优先级查询：
1. **`normKey + normAuthor`**：主键与作者同时命中；
2. **`normKey`（无作者冲突）**：调用方指定作者时仅匹配无作者记录，未指定作者时要求全部同名记录指向同一目标（或存在唯一的无作者记录）；
3. **`normRawKey + normAuthor`**：回退匹配 `rawKey`（同时覆盖去噪键与精确键）与作者；
4. **`normRawKey`（无作者冲突）**：`rawKey` 无作者冲突回退。

若同一作品名存在多条不同作者映射到不同 `targetId` 的冲突记录，系统拒绝盲目猜测，确保不串书。

### 7.3 脱敏红线规则

导入与导出均强制执行以下脱敏过滤，凡命中任一条的记录将被自动丢弃：
- 含有控制字符；
- 含有 URL Scheme（`http://`、`https://`、`ftp://`、`dav://`、`davs://`、`smb://`、`file://`、`socks5://`、`magnet:?` 等）；
- 含有 Windows 盘符/UNC 路径（如 `C:\...`、`D:/...`、`\\nas\...`）或常见 Unix 系统路径（如 `/home/...`、`/users/...`、`/mnt/...`、`/share/...`、`/var/...` 等）；
- 含有合法 IPv4 或 IPv6 地址字面量；
- 含有 JWT 字符串或凭据关键字（如 `fk115_`、`Bearer `、`access_token`、`refresh_token`、`downurl`、`password=`、`api_key=`、`ghp_`、`github_pat_` 等）。

### 7.4 导出与社区仓库提 PR 共建流程

1. 进入管理后台「元数据刮削 → 社区脱敏映射包导出」；
2. 选择要导出的书库范围，保留默认勾选的绑定来源（`manual` 与 `agent`，人工与 AI 审核准确度最高）；
3. 保持「包含 NSFW 条目」处于未勾选状态（若需贡献特定合规分类请先确认目标仓库规则）；
4. 点击 **「预览脱敏结果」**，核对可导出条数、已排除 NSFW 数、隐私规则拦截数，并人工抽检前 30 条样本确认无私人目录名称或敏感信息；
5. 点击 **「下载 .ndjson」** 保存到本地；
6. Fork 目标社区元数据仓库（如 `BlackMuseCode/mangaalpha-metadata-repo`），按仓库目录结构放入或追加 `.ndjson` 文件，提交 Pull Request 即可与社区共享。

---

## 8. 包内嵌入式元数据支持（`ComicInfo.xml` / `galleryinfo.txt`）

为严守 **零隐式拆包** 铁律，批量多源刮削阶段绝不隐式打开或读取漫画归档内部字节，而是仅复用数据库中已缓存的首卷 `book_metadata`（如用户/管理员此前打开阅读或显式执行批量维护任务时解析保存的 `ComicInfo.xml` 标题、作者与链接）作为辅助匹配线索。对单文件安全上限为 `512 KiB`（自动剥离 UTF-8 BOM）：

1. **`ComicInfo.xml`**：
   - 使用标准 `encoding/xml` 严格模式解析，天然免疫外部实体注入（XXE）；
   - 读取 `<Title>`、`<Series>`、`<Summary>`、`<Genre>`、`<Tags>`，并聚合去重 `<Writer>`、`<Penciller>`、`<Inker>`、`<Colorist>`、`<Letterer>`、`<CoverArtist>`、`<Editor>`、`<Translator>` 作为作者列表；
   - 读取 `<Web>`（仅接受 `http/https` URL），自动提取末端数字 ID 或标准 `/subject/<id>` / `/g/<id>/<token>` 条目 ID；
   - `<AgeRating>` 为 `Adults Only 18+`、`18+`、`R18`、`R18+`、`X` 或 `Explicit`（忽略大小写）时标记 `isNsfw = true`。
2. **`galleryinfo.txt`**：
   - 支持键值行解析：`Title`、`Japanese Title`/`title_jpn`、`Series`、`Artist`/`Author`/`Group`/`Circle`、`Tags`/`Tag`（按 `artist:` / `group:` 拆分作者）、`Category`（沿用 `Non-H` 规则判定 NSFW）、`URL`/`Source`、`GID`/`Gallery ID`，以及 `Uploader Comment` / `Comment` / `Summary` 多行简介。

---

## 9. 安全、SSRF 防护与限流熔断机制

1. **防 SSRF 网络客户端与透明代理 Fake-IP 兼容**：
   - 仓库拉取、离线包下载、在线补全与外部封面代理统一使用安全 HTTP 客户端：
     - 仅允许 `http://` 与 `https://` 协议；
     - **字面量 IP 与内网主机名严格拦截**：无论直连还是代理模式，均拒绝 `localhost`、单段主机名、`*.local`/`*.internal`/`*.lan`/`*.home.arpa` 等内网后缀，以及任何指向回环、RFC1918 私网、ULA（`fc00::/7`）、链路本地（`169.254.0.0/16`、`fe80::/10`）、CGNAT（`100.64.0.0/10`）、未指定地址（`0.0.0.0`/`::`）、组播或 `198.18.0.0/15` 的字面量 IP URL；
     - **直连模式 DNS 校验与透明代理兼容（v3.2.1）**：针对合法公网域名的本地 DNS 解析结果，自动跳过屏蔽占位地址（`0.0.0.0` / `::`，绝不拨号；若全部为占位地址则拒绝），放行公网单播 IP 及标准透明代理 Fake-IP 段（IPv4 `198.18.0.0/15`、IPv6 `fc00::/18`，双栈并存时优先拨号非 Fake-IPv6 地址）；若解析结果混入任何真实回环、RFC1918 私网、局域网 ULA（`fd00::/8`）、链路本地、CGNAT 或非法作用域 IPv6，立即整次拒绝以阻断 DNS Rebinding 攻击；
     - **显式代理模式**：支持 `http` / `https` / `socks5` / `socks5h`，同样禁止目标请求指向字面量非公网 IP 或内网主机名；若用户使用自定义非标 Fake-IP 段（如私网段）或自建内网镜像，可在「网络代理设置」中配置显式代理；
     - 重定向最多允许 5 跳且逐跳复检目标地址安全性；跨域或 HTTPS 降级 HTTP 时自动剥离 `Authorization`、`Proxy-Authorization` 与 `Cookie` 请求头；
     - 仓库订阅/刷新、源同步与封面异常（SSRF 拦截、熔断）均记录严格脱敏的 `diagnostics` 诊断事件，并向前端返回安全可读的中文分类提示。
2. **封面抓取限流、校验与路径隔离**：
   - 并发上限 `2`、令牌桶默认速率 `1 req/s`（burst `2`）、`singleflight` 合并重复并发请求；
   - 单张封面体积上限 `10 MiB`，强制校验 JPEG / PNG / WebP / GIF 文件头魔数（拒绝 HTML 错误页冒充图片）；
   - 磁盘缓存路径经哈希清洗与相对路径校验，严格锁定在 `<DataDir>/thumbnails/extmeta/` 目录内。
3. **负缓存与自动熔断器**：
   - 404 或非图片魔数等失败写入 **5 分钟** 负缓存，避免短时间反复请求失效地址；
   - 上游返回 HTTP `403`、`429` 或 `503` 时自动打开熔断器并按指数退避进入冷却：**`30s → 60s → 180s → 10m`（封顶）**，冷却期内直接拦截请求直到恢复 `200 OK` 后自动复位。
