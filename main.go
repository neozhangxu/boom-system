package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
)

func main() {
	// 1. 加载 .env 配置文件
	if err := godotenv.Load(); err != nil {
		log.Println("未找到 .env 文件，将尝试读取系统环境变量")
	}

	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	serverPort := os.Getenv("SERVER_PORT")

	if serverPort == "" {
		serverPort = ":8080"
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", dbUser, dbPassword, dbHost, dbPort, dbName)

	// 2. 初始化数据库连接
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("数据库连接初始化失败: %v", err)
	}

	if err = db.Ping(); err != nil {
		log.Fatalf("数据库连接测试失败: %v", err)
	}
	fmt.Printf("=== 成功连通 MariaDB 数据库 (%s) ===\n", dbName)

	// 3. 初始化 Gin 路由
	r := gin.Default()

	r.Static("/static", "./static")
	r.StaticFile("/", "./static/index.html")

	// 获取头部基本信息接口
	r.GET("/api/bom/header", func(c *gin.Context) {
		row := db.QueryRow("SELECT sample_order_no, sample_type, style_no, style_name, last_no, bottom_no FROM bom_header WHERE id = 1")
		var orderNo, sampleType, styleNo, styleName, lastNo, bottomNo string
		if err := row.Scan(&orderNo, &sampleType, &styleNo, &styleName, &lastNo, &bottomNo); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"sample_order_no": orderNo,
			"sample_type":     sampleType,
			"style_no":        styleNo,
			"style_name":      styleName,
			"last_no":         lastNo,
			"bottom_no":       bottomNo,
		})
	})

	// 获取多配色 BOM 矩阵展开接口
	r.GET("/api/bom/matrix", func(c *gin.Context) {
		colorRows, err := db.Query("SELECT id, color_code FROM bom_colorway ORDER BY id")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取配色失败: " + err.Error()})
			return
		}
		defer colorRows.Close()

		var colorCodes []string
		colorMap := make(map[int]string)
		for colorRows.Next() {
			var id int
			var code string
			colorRows.Scan(&id, &code)
			colorCodes = append(colorCodes, code)
			colorMap[id] = code
		}

		itemRows, err := db.Query(`
			SELECT i.id, p.part_name, i.material_desc, i.unit 
			FROM bom_item i
			JOIN bom_parts p ON i.part_id = p.id
			ORDER BY i.id
		`)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取物料失败: " + err.Error()})
			return
		}
		defer itemRows.Close()

		type ColorDetail struct {
			ColorDesc   string `json:"color_desc"`
			ProcessDesc string `json:"process_desc"`
		}

		type ItemRow struct {
			ItemID       int                    `json:"item_id"`
			PartName     string                 `json:"part_name"`
			MaterialDesc string                 `json:"material_desc"`
			Unit         string                 `json:"unit"`
			ColorDetails map[string]ColorDetail `json:"color_details"`
		}

		itemMap := make(map[int]*ItemRow)
		var itemList []*ItemRow

		for itemRows.Next() {
			var item ItemRow
			item.ColorDetails = make(map[string]ColorDetail)
			itemRows.Scan(&item.ItemID, &item.PartName, &item.MaterialDesc, &item.Unit)
			itemMap[item.ItemID] = &item
			itemList = append(itemList, &item)
		}

		detailRows, err := db.Query(`
			SELECT bom_item_id, colorway_id, COALESCE(material_color_spec, ''), COALESCE(process_desc, '') 
			FROM bom_item_color_detail
		`)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取明细失败: " + err.Error()})
			return
		}
		defer detailRows.Close()

		for detailRows.Next() {
			var itemID, colorwayID int
			var colorDesc, processDesc string
			detailRows.Scan(&itemID, &colorwayID, &colorDesc, &processDesc)

			if item, exists := itemMap[itemID]; exists {
				if code, ok := colorMap[colorwayID]; ok {
					item.ColorDetails[code] = ColorDetail{
						ColorDesc:   colorDesc,
						ProcessDesc: processDesc,
					}
				}
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"columns": colorCodes,
			"data":    itemList,
		})
	})

	// 修改列名接口
	r.POST("/api/bom/update-column", func(c *gin.Context) {
		var req struct {
			OldCode string `json:"old_code"`
			NewCode string `json:"new_code"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		_, err := db.Exec("UPDATE bom_colorway SET color_code = ? WHERE color_code = ?", req.NewCode, req.OldCode)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库更新失败: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// 新增列接口
	r.POST("/api/bom/add-column", func(c *gin.Context) {
		var req struct {
			ColorCode string `json:"color_code"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		res, err := db.Exec("INSERT INTO bom_colorway (color_code) VALUES (?)", req.ColorCode)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "新增配色失败: " + err.Error()})
			return
		}

		colorwayID64, _ := res.LastInsertId()
		colorwayID := int(colorwayID64)

		_, _ = db.Exec(`
			INSERT INTO bom_item_color_detail (bom_item_id, colorway_id, material_color_spec, process_desc)
			SELECT id, ?, '', '' FROM bom_item
		`, colorwayID)

		c.JSON(http.StatusOK, gin.H{"success": true, "colorway_id": colorwayID})
	})

	// 删除列接口
	r.POST("/api/bom/delete-column", func(c *gin.Context) {
		var req struct {
			ColorCode string `json:"color_code"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		var colorwayID int
		err := db.QueryRow("SELECT id FROM bom_colorway WHERE color_code = ?", req.ColorCode).Scan(&colorwayID)
		if err == nil {
			_, _ = db.Exec("DELETE FROM bom_item_color_detail WHERE colorway_id = ?", colorwayID)
			_, _ = db.Exec("DELETE FROM bom_colorway WHERE id = ?", colorwayID)
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// 单元格实时修改接口
	r.POST("/api/bom/update-cell", func(c *gin.Context) {
		var req struct {
			ItemID    int    `json:"item_id"`
			ColorCode string `json:"color_code"`
			FieldType string `json:"field_type"` // "color" 或 "process"
			Value     string `json:"value"`
		}
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		var colorwayID int
		err := db.QueryRow("SELECT id FROM bom_colorway WHERE color_code = ?", req.ColorCode).Scan(&colorwayID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到对应的配色代码"})
			return
		}

		var query string
		if req.FieldType == "color" {
			query = "UPDATE bom_item_color_detail SET material_color_spec = ? WHERE bom_item_id = ? AND colorway_id = ?"
		} else {
			query = "UPDATE bom_item_color_detail SET process_desc = ? WHERE bom_item_id = ? AND colorway_id = ?"
		}

		_, err = db.Exec(query, req.Value, req.ItemID, colorwayID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库单元格更新失败: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	fmt.Printf("=== 后端服务已成功启动，正在监听端口 %s ===\n", serverPort)
	r.Run(serverPort)
}