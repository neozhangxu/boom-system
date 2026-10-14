# 样式单多配色 BOM 管理系统 (BOM Management System)

本项目是一个专为鞋业制造打造的**网端一体化多配色 BOM（物料清单）管理系统**。系统实现了与 Excel 模板（如 094.xls）1:1 的视觉与功能对等，支持复杂的动态行列矩阵、单元格内联编辑、智能右键上下文菜单，并通过 Go/Gin 后端与 MariaDB 数据库实现**全方位的实时数据同步**。

---

## 🏗️ 整体系统架构

系统采用经典的前后端分离与直连架构，整体运行于 **Rocky Linux** 服务器及 **1Panel** 容器化环境中。

```text
┌────────────────────────┐         HTTP / RESTful API         ┌────────────────────────┐
│                        │ ─────────────────────────────────> │                        │
│   Frontend (Browser)   │                                    │   Backend (Go / Gin)   │
│   - VTable             │ <───────────────────────────────── │   - /api/bom/header    │
│   - 智能右键菜单       │            JSON Response           │   - /api/bom/matrix    │
│   - 实时双向同步       │                                    │   - /api/update-cell   │
└────────────────────────┘                                    └────────────────────────┘
                                                                           │
                                                                           │ SQL 驱动 (database/sql)
                                                                           ▼
                            ┌──────────────────────┐     ┌──────────────────────────────┐
                            │ bom_header.bom_header│     │ bom_item.<style-no>         │
                            │                      │     │ bom_colorway.<style-no>     │
                            │                      │     │ bom_item_color_detail.<style-no> │
                            └──────────────────────┘     └──────────────────────────────┘
```

---

## 📂 项目目录结构

```text
bom-system/
├── .env                  # 核心配置文件（集中管理数据库连接凭证与端口）
├── go.mod                # Go 模块依赖管理文件
├── go.sum                # Go 依赖校验和锁文件
├── package.json          # 前端 VTable 与插件依赖
├── vite.config.mjs       # 前端依赖打包配置
├── main.go               # Go 后端主程序（Gin 路由转发、环境变量加载、数据库操作）
├── frontend/
│   └── vendor-entry.js   # VTable、Excel 插件及编辑器入口
└── static/
    └── index.html        # 前端页面（集成 VTable 表格、样式渲染及交互逻辑）
```

---

## ⚙️ 核心技术栈

### 1. 后端 (Backend)
* **语言**：Go (Golang)
* **Web 框架**：`github.com/gin-gonic/gin`（高性能 RESTful API 路由）
* **配置管理**：`github.com/joho/godotenv`（安全读取 `.env` 环境变量）
* **数据库驱动**：`github.com/go-sql-driver/mysql`（连接 MariaDB）

### 2. 前端 (VTable)
* **表格内核**：`@visactor/vtable`（支持动态多配色矩阵、冻结基础列、列宽/行高调整、单元格编辑与剪贴板操作）
* **单元格编辑器**：`@visactor/vtable-editors`
* **Excel 导入导出**：VTable 官方 `@visactor/vtable-plugins`（`ExcelImportPlugin`、`TableExportPlugin`），底层 Excel 文件处理使用 ExcelJS，导入格式为 `.xlsx`
* **样式与交互**：HTML5 / CSS3 / 原生 JavaScript (`fetch` 异步通信)

---

## 🔌 核心 API 接口设计

| 路由端点 | HTTP 方法 | 功能描述 |
| :--- | :--- | :--- |
| `/api/bom/list` | `GET` | Style No. 列表按 ID 倒序游标分页（默认 50 条，最多 100 条）；支持前缀搜索、增量同步和按 `bom_id` 查询单条记录 |
| `/api/bom/header?bom_id=...` | `GET` | 获取指定鞋款的 Excel 表头字段及签核信息 |
| `/api/bom/create` | `POST` | 新增空白鞋款：向 `bom_header` 写入表头，并在三个实体数据库中创建对应 Style No. 的空表 |
| `/api/bom/delete?bom_id=...` | `POST` | 删除指定鞋款主记录及三个实体数据库中对应的 Style No. 数据表；删除前确认，发生错误时优先恢复原表名 |
| `/api/bom/update-header?bom_id=...` | `POST` | 按白名单字段保存表头；修改 Style No. 时同步重命名该鞋款的 BOM 数据表 |
| `/api/bom/matrix?bom_id=...` | `GET` | 按主记录 ID 获取该鞋款的配色列类型、物料明细及交叉矩阵 |
| `/api/bom/import` | `POST` | 按 Style No. 新建或更新鞋款主记录，并在同一事务内全量替换该鞋款 BOM；可指定每列是否包含 Process |
| `/api/bom/update-cell` | `POST` | 单元格修改完成时触发，**实时同步**更新物料颜色规格或工艺描述 |
| `/api/bom/update-column`| `POST` | 修改配色列名时同步更新数据库表头；Process 列名仅在当前页面生效 |
| `/api/bom/update-row` | `POST` | 修改部件名称、物料描述或单位时触发，同步更新物料数据 |
| `/api/bom/add-column` | `POST` | 在指定列上方或下方新增配色列；默认新增“颜色规格 + Process”列组，也支持新增独立单列空白列，并初始化全量物料关联明细 |
| `/api/bom/delete-column`| `POST` | 删除指定配色组（颜色规格 + Process）及其所有关联明细 |
| `/api/bom/add-row` | `POST` | 在指定物料行上方或下方新增全空白行，并初始化空白配色明细；空 BOM 可省略物料锚点以创建首行 |
| `/api/bom/delete-row` | `POST` | 删除指定的物料行及其对应的明细记录 |

---

## 🗄️ 数据库核心表关系

* `bom_header.bom_header` 保留共享鞋款主记录；实体数据库仍为 `bom_item`、`bom_colorway`、`bom_item_color_detail`，每个 Style No. 在这三个数据库中分别对应一张以 Style No. 原值命名的表，例如 `bom_item.ON-27Q3-094W`、`bom_colorway.ON-27Q3-094W`、`bom_item_color_detail.ON-27Q3-094W`。部件名称直接存储在物料行 `part_name` 中，不再维护独立的 `bom_parts` 表或 `part_id` 关联。
* 关系按“鞋款 → 物料/配色 → 物料配色明细”组织。物料与配色各自按 `header_id` 归属鞋款，明细以 `(bom_item_id, colorway_id)` 保证每个物料与配色仅有一条记录，并通过两个 ID 外键确保关联记录存在；明细不重复保存 `header_id`，应用和迁移校验物料、配色属于同一鞋款。
* `bom_colorway.has_process` 标识配色列的展示形状：默认值 `1` 保持既有颜色规格 + Process 双列；值 `0` 表示独立单列。服务启动时会为旧配色表补加该字段，历史列默认保留双列。单列与配色组都通过配色 ID 关联明细，单列值保存在 `material_color_spec`。
* 每张物料/配色表继续使用 `(header_id, display_order, id)` 排序索引，明细使用 `(bom_item_id, colorway_id)` 唯一索引及 `(colorway_id, bom_item_id)` 辅助索引。Style No. 使用唯一索引并按前缀检索；鞋款列表采用 ID 游标分页，不使用深页 `OFFSET` 或一次性加载全表。

按 Style No. 分表可让矩阵查询限制在对应鞋款的数据表中，但不等同于跨服务器分片，也不能替代索引。Style No. 必须为 1–64 位英文字母、数字、下划线或连字符，不能与迁移保留表名冲突。重复 Style No.、大小写导致的表名冲突或已存在的目标表都会拒绝修改。修改 Style No. 时服务会同步重命名该鞋款在三个实体数据库中的数据表；空 Style No. 的旧记录暂存于 `_UNASSIGNED_` 表中。

首次按 Style No. 迁移会扫描旧的实体共享表并逐鞋款复制、校验数据量，全部成功后才清理共享表；首次为大型历史表补建索引、外键或按鞋款迁移数据可能产生大量 I/O、额外磁盘占用并影响写入。请先备份、预估空间并在维护窗口执行，不要把首次迁移当作无锁在线变更。迁移也会校验明细的物料和配色属于同一鞋款，并移除冗余的 `bom_item_color_detail.header_id`。

原始旧库环境需先备份并停止服务，再执行以下旧库重命名（确保目标库名都未被占用），更新 `DB_NAME` 后启动。服务会在启动时将旧实体共享表的数据迁移为按 Style No. 命名的数据表：

```sql
CREATE DATABASE bom_header;
CREATE DATABASE bom_parts;
CREATE DATABASE bom_item;
CREATE DATABASE bom_colorway;
CREATE DATABASE bom_item_color_detail;

RENAME TABLE
  boom.bom_header TO bom_header.bom_header,
  boom.bom_parts TO bom_parts.bom_parts,
  boom.bom_item TO bom_item.bom_item,
  boom.bom_colorway TO bom_colorway.bom_colorway,
  boom.bom_item_color_detail TO bom_item_color_detail.bom_item_color_detail;

DROP DATABASE boom;
```

启动时服务会创建缺少的迁移数据库；若旧共享表与目标 Style No. 表冲突，服务会停止并报错，不覆盖数据。数据库账号需具备创建数据库、迁移表、修改/删除旧表字段、跨数据库重命名表和创建外键的权限。服务随后执行原有字段迁移、`header_id` 回填、旧部件名称复制及旧部件表清理、配色明细鞋款归属校验和外键/唯一索引检查；发现孤立或跨鞋款关联、无效/重复 Style No.、重复物料配色明细或其他关联错误时会报错并停止启动。导入 Excel 时按 Style No. 更新已有鞋款或新建主记录，所有子记录继续使用主记录 `id` 关联。

页面左侧按批次显示数据库内非空 Style No. 原值，滚动加载历史编号，并每 10 秒按递增 ID 检查新增鞋款；点击编号后右侧加载对应表头和 BOM 矩阵。点击“新增”会打开空白鞋款页面，填写 Style No. 和表头后确认，服务先创建三个对应 Style No. 的空表，再新增 `bom_header` 主记录并打开新鞋款；重名或遗留同名数据表会拒绝覆盖。表头字段使用 `stage` 存储 Stage 信息；首次启动迁移时从旧 `shoetype` 字段复制已有值以保留数据。页面表头字段、排列顺序与所提供 Excel 一致；表头和表尾宽度随 VTable 可见宽度同步。签核信息实时保存到当前鞋款数据库记录。页面提供 Excel 导入/导出；Excel 读写由 VTable 官方插件执行。导出为单张工作表，标签名称和文件名均使用当前 Style No.；公司名称及鞋款信息置于 BOM 矩阵上方，签核信息位于矩阵下方同一行，且不冻结窗格。导入前会二次确认，在事务中按 Style No. upsert 主记录、替换该鞋款物料/配色/明细，并保持其余鞋款数据和外键关系不变。

---

## 🚀 快速部署与运行指南

1. **环境初始化**：
   仓库已包含前端 VTable 运行包，可直接启动 Go 服务。若修改前端依赖或构建入口，确保安装 Node.js 后重新构建：
   ```bash
   npm install
   npm run build
   ```

   下载 Go 依赖：
   ```bash
   go mod download
   ```

2. **配置环境变量 (`.env`)**：
   在项目根目录下创建并配置 `.env` 文件：
   ```env
   DB_USER=root
   DB_PASSWORD=你的数据库密码
   DB_HOST=127.0.0.1
   DB_PORT=3306
   DB_NAME=bom_header
   SERVER_PORT=:8080
   ```

3. **启动后端服务**：
   ```bash
   go run main.go
   ```

4. **访问系统**：
   打开浏览器访问 `http://服务器IP:8080` 即可开始使用全功能动态 BOM 矩阵系统。