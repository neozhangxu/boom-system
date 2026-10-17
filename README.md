# 鞋款 BOM 管理系统

面向鞋业制造的 Web BOM 管理系统。前端使用 Vue 3 与 Vxe-Table，后端使用 Go/Gin，数据直接读写 PostgreSQL 的现有 `body` / `head` schema。

Web 表格支持单元格/整行编辑、键盘导航、物料编号校验、分类树、排序与分类筛选、合计行、冻结表头/底部及左侧列和自定义列。组件节点按当前鞋款已启用的分类生成；新增鞋款可右键 Color Codes 行添加组件。组件仅为分类分组，不会预先写入空白物料记录。若鞋款尚无 UPPER 物料行而编辑 Color Codes，系统会在确认保存时创建一条 UPPER 根行，确保编码可保存在 `body.color_codes`。分类、No.、Parts、Material Description、Unit、Supplier、Remark 为固定列，其余列标题可双击修改；内置列标题保存在当前浏览器，自定义列标题和行数据保存在 PostgreSQL。右键可在可操作列后添加列、复制/粘贴整列、删除列（删除自定义列并清除数据；删除内置配色/工艺列会在当前鞋款隐藏该列）。物料表支持右键添加行/节点行、复制（含子行）、粘贴和级联删除；层级与排序保存在 PostgreSQL。Vxe 工具栏提供本地 XLSX/CSV 导入导出与打印；导入会在前端校验后暂存，确认保存时通过事务整体替换 BOM，导出的 Parent Row 列可恢复层级。

表头、物料单元格与行、自定义列设置及 Excel/CSV 导入在页面中先作为未保存草稿；点击“确认保存”后，服务在单个 PostgreSQL `SERIALIZABLE` 事务中统一写入，失败时保留页面草稿。内置列标题在确认保存时写入当前浏览器。切换鞋款或离开页面时会提示处理未保存的修改。

## PostgreSQL 数据结构

每个 Style No. 在 `head` 和 `body` 中分别有一张同名表。`head` 保存鞋款表头；`body` 集中保存全部物料行，以 `module_name` 区分分类。服务不会连接 MariaDB，也不会在启动时自动创建、清空或改造数据库结构。

| Schema | 每个 Style No. 的表 |
| --- | --- |
| `head` | 鞋款表头，例如 `head.on_27q3_094w` |
| `body` | 全部 BOM 物料，例如 `body.on_27q3_094w` |

`head` 使用数据库现有鞋款字段（包括 `company_title`、`order_title`、`style_no`、`style_name`、客户、季节、品牌、鞋型、版师/模具师及日期字段）；页面表头直接编辑这些字段。旧架构中的独立签核表不属于该数据模型，因此页面不再显示或写入签核信息。

`body` 保存物料编号/描述、单位、供应商、备注、`color_codes` 及 8 组 JSONB 配色/工艺值。页面不显示独立的 Color Codes 列；表格顶部的 Color Codes 行将 UPPER（鞋面）物料 `color_codes` 数组的 8 个值分别显示在“配色 1”至“配色 8”列下，左键编辑后会在事务中同步更新该 Style No. 的全部 UPPER（鞋面）物料行，不影响其他分类，并刷新表格。Excel/CSV 导入导出仍保留每行 Color Codes 数据。每张 `body.<Style No.>` 表仅属于对应鞋款，因此不含 `style_no` 列。`module_name` 分类为 UPPER、LINING、REINFORCEMENT、SOCK LINER、INSOLE、OUTSOLE、ACCESSORIES、PACKING、STITCHING 和 REM；迁移会将既有 `remark` 分类值统一为 `rem`，页面将其显示为 REM。`parent_id` 与 `module_name` 组成同表自引用外键，确保父子行属于同一分类并级联删除；`sort_order` 保存同级物料顺序，`custom_fields` JSONB 保存自定义列数据。`head` 的 `custom_columns` / `hidden_columns` JSONB 字段保存列元数据。

数据库迁移脚本统一存放在 [`datebases_back/`](./datebases_back/)。对已存在数据库，先备份，再由数据库管理员手动执行迁移。如果尚未执行 003，依次执行 [`003_head_body_tree.sql`](./datebases_back/003_head_body_tree.sql) 和 [`004_body_parent_id.sql`](./datebases_back/004_body_parent_id.sql)；如果 003 已执行，只需执行 004，避免重置已有排序。004 会在每张现有 `body` 鞋款表中增加可空 `parent_id`、自引用外键及索引；已有行保持根节点，不删除物料数据。服务启动会检查 schema、表、所需字段和父子关系外键，不会自行执行迁移。创建新鞋款时，服务在单个事务中复制现有 `body` / `head` 表结构并创建同名表；删除鞋款时在单个事务中删除对应两表。

组件管理还需要由数据库管理员手动执行 [`005_head_components.sql`](./datebases_back/005_head_components.sql) 和 [`006_component_availability.sql`](./datebases_back/006_component_availability.sql)。005 为每张 `head` 鞋款表增加 JSONB 组件清单；006 将既有鞋款清单初始化为当前 BOM 中实际存在的分类，并为新鞋款设置空清单。服务创建新鞋款时会明确初始化全部默认组件，因此页面立即显示所有分类节点；已有鞋款则可按需添加或删除组件。删除组件时，其分类物料会在点击“确认保存”时与其余修改一并事务替换。服务启动会检查 `head.components` 字段，不会自行执行迁移。

## 配置

将 `.env.example` 复制为 `.env`，并在本机填写 PostgreSQL 密码：

```dotenv
PG_HOST=127.0.0.1
PG_PORT=5432
PG_USER=postgres
PG_PASSWORD=请在本机填写数据库密码
PG_DATABASE=db
PG_SSLMODE=disable
PORT=8080
```

数据库密码只保存在本机 `.env` 或环境变量中，不要提交到版本控制。也可以通过 `PG_PASSWORD` 或 `PGPASSWORD` 环境变量提供密码。

## 运行

```bash
go mod download
make build
./dist/boom-system
```

访问 `http://127.0.0.1:8080`。

`make build` 会依次运行 `npm install`、`npm run build` 和 `go build`，生成的 `dist/boom-system` 已嵌入前端页面及静态资源；部署时只需分发该二进制和数据库配置，不需要单独携带 `static/` 文件夹。Go 工具本身不支持在原生 `go build` 命令前配置自动运行 npm 的钩子，因此请用 `make build` 作为完整构建入口。若只需单独构建前端：

```bash
npm install
npm run build
```

前端构建需要 Node.js 20.19.0 或更高版本。直接运行 `go build` 只会嵌入当前已有的 `static/index.html` 和 `static/build/` 文件，不会自动刷新前端资源。

## 一致性与 API

所有写操作在 PostgreSQL `SERIALIZABLE` 事务中执行，服务会对序列化冲突和死锁进行有限重试；事务失败会回滚，不会返回成功响应。跨客户端并发修改无法自动合并时，接口返回冲突，前端显示错误并保留数据库中的已提交状态。读取表头与 BOM 矩阵使用一致性快照。

| 接口 | 方法 | 用途 |
| --- | --- | --- |
| `/api/bom/list?q=...` | GET | 按 Style No. 前缀检索 `head` 鞋款 |
| `/api/bom/header?bom_id=...` | GET | 读取 `head` 鞋款表头 |
| `/api/bom/matrix?bom_id=...` | GET | 读取 `body` 分类树、鞋款组件清单及 8 组 JSONB 配色/工艺值 |
| `/api/bom/create` | POST | 在事务中创建 `head` 与 `body` 表 |
| `/api/bom/delete?bom_id=...` | POST | 在事务中删除对应 `head` 与 `body` 表 |
| `/api/bom/update-header?bom_id=...` | POST | 保存 `head` 表头；修改 Style No. 时重命名两张表 |
| `/api/bom/update-row?bom_id=...` | POST | 保存物料行字段 |
| `/api/bom/update-cell?bom_id=...` | POST | 保存指定配色/工艺 JSONB 数组元素 |
| `/api/bom/update-color-code?bom_id=...` | POST | 保存 Color Codes 行指定位置，并同步到该鞋款所有 UPPER 物料行 |
| `/api/bom/row-insert?bom_id=...` | POST | 在指定位置新增物料行或粘贴物料子树 |
| `/api/bom/row-delete?bom_id=...` | POST | 删除物料行及其全部子行，并重排同级物料 |
| `/api/bom/update-custom-cell?bom_id=...` | POST | 保存自定义列的单元格值 |
| `/api/bom/custom-column?bom_id=...` | POST | 添加、重命名、复制数据到新列或删除/隐藏列 |
| `/api/bom/import?bom_id=...` | POST | 确认保存时，在事务内整体替换当前鞋款表头、组件清单、列设置和 BOM 数据 |

旧版 `style` / `footer` / 分类 schema 迁移脚本仅用于旧数据布局，不要在当前 `head` / `body` 数据库执行。
