package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dbURL := os.Getenv("DBOS_SYSTEM_DATABASE_URL")
	if dbURL == "" {
		slog.Error("DBOS_SYSTEM_DATABASE_URL required")
		os.Exit(1)
	}

	var err error
	db, err = pgxpool.New(context.Background(), dbURL)
	if err != nil {
		slog.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	initLogger()

	r := gin.Default()

	r.StaticFile("/", "./html/app.html")

	r.GET("/product", func(c *gin.Context) { getProduct(c, db, logger) })
	r.GET("/orders", func(c *gin.Context) { getOrders(c, db, logger) })
	r.POST("/restock", func(c *gin.Context) { restock(c, db, logger) })
	r.POST("/crash_application", func(c *gin.Context) { crashApplication(c, logger) })

	slog.Info("Starting server on :8080")
	if err := r.Run(":8080"); err != nil {
		slog.Error("server start failed", "error", err)
		os.Exit(1)
	}
}
