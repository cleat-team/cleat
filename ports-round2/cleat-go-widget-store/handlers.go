package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rcownie/cleat/cleat"
)

// NOTE: handlers.go is provided as a reference for how the original HTTP handlers
// would map to the Cleat SDK. In the Cleat model, external HTTP handlers
// communicate with workflows via signals and queries through the
// Cleat runtime API.
//
// The complete port of handlers.go requires the Cleat runtime's
// workflow-management API (start workflow, send signal, query state),
// which is available through the host runtime but not through the
// cleattest.TestEnv. The workflow functions and tests below are
// the primary deliverable of this port.

var (
	db     *pgxpool.Pool
	logger *slog.Logger
)

func initLogger() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

func getProduct(c *gin.Context, db *pgxpool.Pool, logger *slog.Logger) {
	var product Product
	err := db.QueryRow(c.Request.Context(),
		"SELECT product_id, product, description, inventory, price FROM products LIMIT 1").
		Scan(&product.ProductID, &product.Product, &product.Description, &product.Inventory, &product.Price)
	if err != nil {
		logger.Error("product query failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch product"})
		return
	}
	c.JSON(http.StatusOK, product)
}

func getOrders(c *gin.Context, db *pgxpool.Pool, logger *slog.Logger) {
	rows, err := db.Query(c.Request.Context(),
		"SELECT order_id, order_status, last_update_time, progress_remaining FROM orders")
	if err != nil {
		logger.Error("orders query failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch orders"})
		return
	}
	defer rows.Close()

	orders := []Order{}
	for rows.Next() {
		var order Order
		if err := rows.Scan(&order.OrderID, &order.OrderStatus, &order.LastUpdateTime, &order.ProgressRemaining); err != nil {
			logger.Error("order scan failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process orders"})
			return
		}
		orders = append(orders, order)
	}
	c.JSON(http.StatusOK, orders)
}

func getOrder(c *gin.Context, db *pgxpool.Pool, logger *slog.Logger) {
	// Placeholder -- see original handlers.go for the full implementation
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Not implemented in Cleat port"})
}

func restock(c *gin.Context, db *pgxpool.Pool, logger *slog.Logger) {
	_, err := db.Exec(c.Request.Context(), "UPDATE products SET inventory = 100")
	if err != nil {
		logger.Error("restock failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to restock inventory"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Restocked successfully"})
}

// checkoutEndpoint starts the checkout workflow.
// In the Cleat model, workflows are started via the runtime API.
// This placeholder shows the intended integration point.
func checkoutEndpoint(c *gin.Context, h cleat.HostCalls, logger *slog.Logger) {
	// In production, start the workflow via the Cleat runtime API:
	//   runID, err := runtime.StartWorkflow("checkoutWorkflow", "")
	// Then poll for the PAYMENT_ID query state to return to the caller.
	//
	// For now, see workflows_test.go for the test-driven equivalent.
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Cleat runtime integration required"})
}

// paymentEndpoint sends a payment signal to the checkout workflow.
// In the Cleat model, signals are sent via the runtime API:
//   runtime.SignalWorkflow(runID, PAYMENT_STATUS, paymentStatus)
func paymentEndpoint(c *gin.Context, logger *slog.Logger) {
	// Placeholder -- requires Cleat runtime API integration
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Cleat runtime integration required"})
}

func crashApplication(c *gin.Context, logger *slog.Logger) {
	logger.Warn("application crash requested")
	c.JSON(http.StatusOK, gin.H{"message": "Crashing application..."})
	go func() {
		time.Sleep(100 * time.Millisecond)
		logger.Error("intentional crash for demo")
		os.Exit(1)
	}()
}
