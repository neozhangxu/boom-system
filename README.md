# 鞋款 BOM 管理系统

面向鞋业制造的 Web BOM 管理系统。前端使用 Vue 3 与 Vxe-Table，后端使用 Go/Gin，数据直接读写本机 PostgreSQL。

Web 表格支持单元格/整行编辑、键盘导航、物料编号校验、分类树、排序与分类筛选、合计行、冻结表头/底部及左侧列和自定义列。分类、No.、Parts、Material Description、Unit、Supplier、Remark 为固定列，其余列标题可双击修改；内置列标题保存在当前浏览器，自定义列标题和行数据保存在 PostgreSQL。右键可在可操作列后添加列、复制/粘贴整列、删除列（删除自定义列并清除数据；删除内置配色/工艺列会在当前鞋款隐藏该列）。物料表支持右键添加行/子节点、复制（含子行）、粘贴和级联删除；行层级与排序保存在 PostgreSQL。Vxe 工具栏提供本地 XLSX/CSV 导入导出与打印；导入会在前端校验后通过现有事务接口整体替换 BOM，导出的 Parent Row 列会在导入时恢复层级。

## 当前 PostgreSQL 数据结构

服务使用同一个 PostgreSQL 数据库中的现有 schema 和按 Style No. 命名的表，不会连接或迁移 MariaDB，也不会在启动时自动创建、清空或改造现有 schema。

| Schema | 每个 Style No. 的表 |
| --- | --- |
| `style` | 鞋款表头，例如 `style.on_27q3_094w` |
| `footer` | 签核信息，例如 `footer.on_27q3_094w` |
| `upper`、`stitching`、`accessories`、`insole`、`lining`、`outsole`、`packing`、`reinforcement`、`sock_liner` | 各物料分类，例如 `upper.on_27q3_094w` |

物料表使用 `id`、`style_no`、`no`、`parts`、`material_description`、`unit`、`remark`、`supplier`、`color` 和 `process`；行层级需要 `parent_id` 和 `sort_order`，自定义列数据保存在 `custom_fields` JSONB 中。`color`、`process` 为 JSONB 数组；页面展示并保存 8 组配色/工艺值。鞋款表的 `custom_columns` 和 `hidden_columns` JSONB 字段分别保存自定义列元数据和隐藏的内置配色/工艺列。签核表使用 `approved_by`、`checked_by`、`prepared_by` 和 `approval_date`。

服务启动时会检查所需 schema 与现有 Style No. 表；如果结构不完整会明确报错，不会自动迁移。启用行操作与列操作前，请由数据库管理员连接目标 PostgreSQL 并按顺序手动执行 [`migrations/001_bom_hierarchy.sql`](./migrations/001_bom_hierarchy.sql) 和 [`migrations/002_custom_columns.sql`](./migrations/002_custom_columns.sql)。前者为现有物料表增加行层级字段、级联外键和排序索引；后者增加自定义列元数据及 JSONB 数据字段。服务启动只校验这些结构，不会自动执行迁移。创建新鞋款时，服务在单个事务中按同一 schema 的现有表复制表结构并创建该鞋款各分类表；所有表创建和数据写入成功后才提交。删除鞋款会在单个事务中删除对应的所有鞋款表。

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
go run .
```

访问 `http://127.0.0.1:8080`。

修改前端依赖或构建入口后运行：

```bash
npm install
npm run build
```

前端构建需要 Node.js 20.19.0 或更高版本。

## 一致性与 API

所有写操作在 PostgreSQL `SERIALIZABLE` 事务中执行，服务会对序列化冲突和死锁进行有限重试；事务失败会回滚，不会返回成功响应。跨客户端并发修改无法自动合并时，接口返回冲突，前端显示错误并保留数据库中的已提交状态。读取表头与 BOM 矩阵使用一致性快照。

| 接口 | 方法 | 用途 |
| --- | --- | --- |
| `/api/bom/list?q=...` | GET | 按 Style No. 前缀检索现有鞋款 |
| `/api/bom/header?bom_id=...` | GET | 读取鞋款表头与签核信息 |
| `/api/bom/matrix?bom_id=...` | GET | 读取 9 个物料分类及 8 组 JSONB 配色/工艺值 |
| `/api/bom/create` | POST | 在事务中创建鞋款及其分类表 |
| `/api/bom/delete?bom_id=...` | POST | 在事务中删除鞋款及其分类表 |
| `/api/bom/update-header?bom_id=...` | POST | 保存表头或签核字段；修改 Style No. 时同步重命名各 schema 表 |
| `/api/bom/update-row?bom_id=...` | POST | 保存物料行字段 |
| `/api/bom/update-cell?bom_id=...` | POST | 保存指定配色/工艺 JSONB 数组元素 |
| `/api/bom/row-insert?bom_id=...` | POST | 在指定位置新增物料行或粘贴一棵物料子树 |
| `/api/bom/row-delete?bom_id=...` | POST | 删除物料行及其全部子行，并重排同级物料 |
| `/api/bom/update-custom-cell?bom_id=...` | POST | 保存自定义列的单元格值 |
| `/api/bom/custom-column?bom_id=...` | POST | 添加、重命名、复制数据到新列或删除/隐藏列 |
| `/api/bom/import?bom_id=...` | POST | 事务内整体替换当前鞋款 BOM 数据 |

历史 MariaDB 配置、容器、迁移脚本及其数据不再由本项目使用。
