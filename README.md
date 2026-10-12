# 样式单多配色 BOM 管理系统 (BOM Management System)

本项目是一个专为鞋业制造打造的**网端一体化多配色 BOM（物料清单）管理系统**。系统实现了与 Excel 模板（如 094.xls）1:1 的视觉与功能对等，支持复杂的动态行列矩阵、单元格内联编辑、智能右键上下文菜单，并通过 Go/Gin 后端与 MariaDB 数据库实现**全方位的实时数据同步**。

---

## 🏗️ 整体系统架构

系统采用经典的前后端分离与直连架构，整体运行于 **Rocky Linux** 服务器及 **1Panel** 容器化环境中。

```text
┌────────────────────────┐         HTTP / RESTful API         ┌────────────────────────┐
│                        │ ─────────────────────────────────> │                        │
│   Frontend (Browser)   │                                    │   Backend (Go / Gin)   │
│   - Handsontable       │ <───────────────────────────────── │   - /api/bom/header    │
│   - 智能右键菜单       │            JSON Response           │   - /api/bom/matrix    │
│   - 实时双向同步       │                                    │   - /api/update-cell   │
└────────────────────────┘                                    └────────────────────────┘
                                                                           │
                                                                           │ SQL 驱动 (database/sql)
                                                                           ▼
                                                              ┌────────────────────────┐
                                                              │   Database (MariaDB)   │
                                                              │   - bom_header         │
                                                              │   - bom_parts          │
                                                              │   - bom_item           │
                                                              │   - bom_colorway       │
                                                              │   - bom_item_color     │
                                                              └────────────────────────┘
```

---

## 📂 项目目录结构

```text
bom-system/
├── .env                  # 核心配置文件（集中管理数据库连接凭证与端口）
├── go.mod                # Go 模块依赖管理文件
├── go.sum                # Go 依赖校验和锁文件
├── main.go               # Go 后端主程序（Gin 路由转发、环境变量加载、数据库操作）
└── static/
    └── index.html        # 前端页面（集成 Handsontable 电子表格、样式渲染及交互逻辑）
```

---

## ⚙️ 核心技术栈

### 1. 后端 (Backend)
* **语言**：Go (Golang)
* **Web 框架**：`github.com/gin-gonic/gin`（高性能 RESTful API 路由）
* **配置管理**：`github.com/joho/godotenv`（安全读取 `.env` 环境变量）
* **数据库驱动**：`github.com/go-sql-driver/mysql`（连接 MariaDB）

### 2. 前端 (Handsontable)
* **表格内核**：`Handsontable`（提供媲美 Excel 的虚拟滚动、列宽拖拽、复制粘贴、单元格选区体验）
* **样式与交互**：HTML5 / CSS3 / 原生 JavaScript (`fetch` 异步通信)

---

## 🔌 核心 API 接口设计

| 路由端点 | HTTP 方法 | 功能描述 |
| :--- | :--- | :--- |
| `/api/bom/header` | `GET` | 获取顶部鞋款基础主档信息（订单号、样品类型、型体编号等） |
| `/api/bom/matrix` | `GET` | 动态获取多配色列头、物料明细及单元格交叉矩阵数据 |
| `/api/bom/update-cell` | `POST` | 单元格修改完成时触发，**实时同步**更新物料颜色规格或工艺描述 |
| `/api/bom/update-column`| `POST` | 修改配色列或 Process 列名称时触发，同步更新数据库表头 |
| `/api/bom/add-column` | `POST` | 在左侧或右侧新增配色列，自动初始化全量物料关联明细 |
| `/api/bom/delete-column`| `POST` | 删除指定的配色列及其下属所有工艺明细 |
| `/api/bom/add-row` | `POST` | 新增一行物料数据并初始化对应配色的明细结构 |
| `/api/bom/delete-row` | `POST` | 删除指定的物料行及其对应的明细记录 |

---

## 🗄️ 数据库核心表关系

* **`bom_header`**：存储订单与鞋款基本元数据。
* **`bom_parts`**：存储鞋款部件大类（如 Upper, Outsole 等）。
* **`bom_item`**：存储具体物料明细（关联 `bom_parts`）。
* **`bom_colorway`**：存储多配色代码列表（如 `3WH12345678`）。
* **`bom_item_color_detail`**：核心交叉矩阵表，通过 `bom_item_id` 和 `colorway_id` 联合索引，存储每个物料在不同配色下的 `material_color_spec`（颜色规格）与 `process_desc`（工艺描述）。

---

## 🚀 快速部署与运行指南

1. **环境初始化**：
   确保服务器已安装 Go 环境。在项目根目录下初始化模块并安装依赖：
   ```bash
   go mod init bom-system
   go get github.com/joho/godotenv
   go get github.com/gin-gonic/gin
   go get github.com/go-sql-driver/mysql
   ```

2. **配置环境变量 (`.env`)**：
   在项目根目录下创建并配置 `.env` 文件：
   ```env
   DB_USER=root
   DB_PASSWORD=你的数据库密码
   DB_HOST=127.0.0.1
   DB_PORT=3306
   DB_NAME=boom
   SERVER_PORT=:8080
   ```

3. **启动后端服务**：
   ```bash
   go run main.go
   ```

4. **访问系统**：
   打开浏览器访问 `http://服务器IP:8080` 即可开始使用全功能动态 BOM 矩阵系统。