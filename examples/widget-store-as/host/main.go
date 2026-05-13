// Command widget-store-host is a Go-based HTTP host for the widget-store
// e-commerce demo. It provides the "shop" backing service that handles
// durableCall("shop", operation, args) from cleat workflows, and also
// serves the HTTP API for client interactions.
//
// In a production deployment, the AS workflows would be compiled to WASM
// and executed by a cleat worker. This host provides the service endpoints
// that the worker calls.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rcownie/cleat/cleat"
	"github.com/rcownie/cleat/cleat/localdev"
)

// ---------------------------------------------------------------------------
// Shop domain types
// ---------------------------------------------------------------------------

type Order struct {
	ID        string    `json:"id"`
	Product   string    `json:"product"`
	Quantity  int       `json:"quantity"`
	Status    string    `json:"status"`
	PaymentID string    `json:"payment_id,omitempty"`
	Progress  string    `json:"progress,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type PaymentInfo struct {
	OrderID   string `json:"order_id"`
	PaymentID string `json:"payment_id"`
}

type Product struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
	Price    int    `json:"price_cents"`
}

// ---------------------------------------------------------------------------
// In-memory shop state
// ---------------------------------------------------------------------------

type ShopService struct {
	mu        sync.Mutex
	inventory map[string]int
	prices    map[string]int
	orders    map[string]*Order
	orderSeq  int
	orderMux  sync.Mutex
}

func NewShopService() *ShopService {
	return &ShopService{
		inventory: map[string]int{
			"widget":    100,
			"gadget":    50,
			"doohickey": 200,
		},
		prices: map[string]int{
			"widget":    1999,
			"gadget":    2999,
			"doohickey": 999,
		},
		orders: make(map[string]*Order),
	}
}

// HandleDurableCall processes a durableCall("shop", operation, requestJSON)
// and returns the response JSON or an error.
func (s *ShopService) HandleDurableCall(_ context.Context, service, operation, requestJSON string) (string, error) {
	if service != "shop" {
		return "", fmt.Errorf("unknown service: %s", service)
	}
	switch operation {
	case "subtractInventory":
		return s.subtractInventory(requestJSON)
	case "undoSubtractInventory":
		return s.undoSubtractInventory(requestJSON)
	case "createOrder":
		return s.createOrder(requestJSON)
	case "markOrderPaid":
		return s.markOrderPaid(requestJSON)
	case "errorOrder":
		return s.errorOrder(requestJSON)
	case "retrieveOrder":
		return s.retrieveOrder(requestJSON)
	case "retrieveOrders":
		return s.retrieveOrders()
	case "updateOrderProgress":
		return s.updateOrderProgress(requestJSON)
	case "setInventory":
		return s.setInventory(requestJSON)
	default:
		return "", fmt.Errorf("unknown operation: %s.%s", service, operation)
	}
}

// ServiceCaller adapts ShopService to localdev.ServiceCaller.
type serviceCaller struct {
	shop *ShopService
}

func (c *serviceCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	return c.shop.HandleDurableCall(ctx, service, operation, requestJSON)
}

// ---------------------------------------------------------------------------
// Shop operations
// ---------------------------------------------------------------------------

type reserveReq struct {
	Product  string `json:"product"`
	Quantity int    `json:"quantity"`
}

type orderReq struct {
	OrderID string `json:"order_id"`
}

type orderResp struct {
	OrderID   string `json:"order_id"`
	PaymentID string `json:"payment_id"`
}

type progressReq struct {
	OrderID  string `json:"order_id"`
	Progress string `json:"progress"`
}

func (s *ShopService) subtractInventory(reqJSON string) (string, error) {
	var req reserveReq
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return "", fmt.Errorf("bad request: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	qty, ok := s.inventory[req.Product]
	if !ok {
		return "", fmt.Errorf("unknown product: %s", req.Product)
	}
	if qty < req.Quantity {
		return "", fmt.Errorf("insufficient inventory: have %d, need %d", qty, req.Quantity)
	}
	s.inventory[req.Product] = qty - req.Quantity
	return `{"ok":true}`, nil
}

func (s *ShopService) undoSubtractInventory(reqJSON string) (string, error) {
	var req reserveReq
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return "", fmt.Errorf("bad request: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inventory[req.Product] += req.Quantity
	return `{"ok":true}`, nil
}

func (s *ShopService) createOrder(reqJSON string) (string, error) {
	var req reserveReq
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return "", fmt.Errorf("bad request: %w", err)
	}
	s.orderMux.Lock()
	s.orderSeq++
	orderID := fmt.Sprintf("order-%05d", s.orderSeq)
	paymentID := fmt.Sprintf("pay-%05d", s.orderSeq)
	s.orderMux.Unlock()

	order := &Order{
		ID:        orderID,
		Product:   req.Product,
		Quantity:  req.Quantity,
		Status:    "pending",
		PaymentID: paymentID,
		CreatedAt: time.Now(),
	}

	s.mu.Lock()
	s.orders[orderID] = order
	s.mu.Unlock()

	resp := orderResp{OrderID: orderID, PaymentID: paymentID}
	data, _ := json.Marshal(resp)
	return string(data), nil
}

func (s *ShopService) markOrderPaid(reqJSON string) (string, error) {
	var req orderReq
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		var anyMap map[string]interface{}
		if err2 := json.Unmarshal([]byte(reqJSON), &anyMap); err2 != nil {
			return "", fmt.Errorf("bad request: %w", err)
		}
		if oid, ok := anyMap["order_id"]; ok {
			req.OrderID = fmt.Sprintf("%v", oid)
		} else {
			return "", fmt.Errorf("no order_id in payload")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[req.OrderID]
	if !ok {
		return "", fmt.Errorf("order not found: %s", req.OrderID)
	}
	order.Status = "paid"
	return `{"ok":true}`, nil
}

func (s *ShopService) errorOrder(reqJSON string) (string, error) {
	var req orderReq
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return "", fmt.Errorf("bad request: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[req.OrderID]
	if !ok {
		return "", fmt.Errorf("order not found: %s", req.OrderID)
	}
	order.Status = "error"
	return `{"ok":true}`, nil
}

func (s *ShopService) retrieveOrder(reqJSON string) (string, error) {
	var req orderReq
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return "", fmt.Errorf("bad request: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[req.OrderID]
	if !ok {
		return "", fmt.Errorf("order not found: %s", req.OrderID)
	}
	data, _ := json.Marshal(order)
	return string(data), nil
}

func (s *ShopService) retrieveOrders() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	orders := make([]*Order, 0, len(s.orders))
	for _, o := range s.orders {
		orders = append(orders, o)
	}
	data, _ := json.Marshal(orders)
	return string(data), nil
}

func (s *ShopService) updateOrderProgress(reqJSON string) (string, error) {
	var req progressReq
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return "", fmt.Errorf("bad request: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[req.OrderID]
	if !ok {
		return "", fmt.Errorf("order not found: %s", req.OrderID)
	}
	order.Progress = req.Progress
	return `{"ok":true}`, nil
}

func (s *ShopService) setInventory(reqJSON string) (string, error) {
	var req map[string]int
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		return "", fmt.Errorf("bad request: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for product, qty := range req {
		s.inventory[product] = qty
	}
	return `{"ok":true}`, nil
}

// ---------------------------------------------------------------------------
// Workflow execution using localdev.LocalRunner
// ---------------------------------------------------------------------------

// WorkflowRunner manages workflow instances using localdev.
type WorkflowRunner struct {
	mu      sync.Mutex
	runners map[string]*localdev.LocalRunner
	nextID  int
}

func NewWorkflowRunner() *WorkflowRunner {
	return &WorkflowRunner{
		runners: make(map[string]*localdev.LocalRunner),
	}
}

// StartCheckout starts a checkout workflow and returns the workflow ID.
func (wr *WorkflowRunner) StartCheckout(shop *ShopService, product string, quantity int) (string, error) {
	wr.mu.Lock()
	wr.nextID++
	wfID := fmt.Sprintf("wf-%05d", wr.nextID)
	wr.mu.Unlock()

	runner := localdev.NewLocalRunner(
		localdev.WithServiceCaller(&serviceCaller{shop: shop}),
		localdev.WithWorkflowID(wfID),
	)

	wr.mu.Lock()
	wr.runners[wfID] = runner
	wr.mu.Unlock()

	wfInput := map[string]interface{}{
		"product":  product,
		"quantity": quantity,
	}
	inputJSON, _ := json.Marshal(wfInput)

	go func() {
		runCheckoutWorkflow(runner.H(), shop, string(inputJSON))
		wr.mu.Lock()
		delete(wr.runners, wfID)
		wr.mu.Unlock()
	}()

	return wfID, nil
}

// DeliverPaymentSignal delivers a payment signal to a running workflow.
func (wr *WorkflowRunner) DeliverPaymentSignal(wfID, payload string) error {
	wr.mu.Lock()
	runner, ok := wr.runners[wfID]
	wr.mu.Unlock()

	if !ok {
		return fmt.Errorf("workflow %s not found", wfID)
	}

	runner.SendSignal("PAYMENT_TOPIC", payload)
	return nil
}

// QueryState returns a query state value for a workflow.
func (wr *WorkflowRunner) QueryState(wfID, key string) (string, bool) {
	wr.mu.Lock()
	runner, ok := wr.runners[wfID]
	wr.mu.Unlock()

	if !ok {
		return "", false
	}
	_ = runner // in localdev mode, query state isn't exposed via the runner
	return "", false
}

// ---------------------------------------------------------------------------
// Go implementation of checkout workflow (for localdev testing)
// ---------------------------------------------------------------------------

// runCheckoutWorkflow is the Go equivalent of the AS checkoutWorkflow.
func runCheckoutWorkflow(h cleat.HostCalls, shop *ShopService, inputJSON string) string {
	var input struct {
		Product  string `json:"product"`
		Quantity int    `json:"quantity"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return fmt.Sprintf(`{"error":"invalid input: %s"}`, err.Error())
	}

	// Step 1: Subtract inventory
	reserveReq, _ := json.Marshal(map[string]interface{}{
		"product":  input.Product,
		"quantity": input.Quantity,
	})
	reserveReqStr := string(reserveReq)

	_, err := h.Call("shop", "subtractInventory", reserveReqStr)
	if err != nil {
		return fmt.Sprintf(`{"error":"inventory: %s"}`, err.Error())
	}

	// Step 2: Create order
	resp, err := h.Call("shop", "createOrder", reserveReqStr)
	if err != nil {
		h.Call("shop", "undoSubtractInventory", reserveReqStr)
		return fmt.Sprintf(`{"error":"order: %s"}`, err.Error())
	}

	var orderInfo struct {
		OrderID   string `json:"order_id"`
		PaymentID string `json:"payment_id"`
	}
	if err := json.Unmarshal([]byte(resp), &orderInfo); err != nil || orderInfo.OrderID == "" {
		h.Call("shop", "undoSubtractInventory", reserveReqStr)
		return `{"error":"invalid order response"}`
	}

	// Step 3: Expose payment info
	paymentInfo, _ := json.Marshal(orderInfo)
	h.SetQueryState("payment_info", string(paymentInfo))

	// Step 4: Wait for payment signal
	sr := h.AwaitSignals([]string{"PAYMENT_TOPIC"}, 120*time.Second)
	if sr.TimedOut {
		h.DurableLog("Payment timeout for order " + orderInfo.OrderID)
		h.Call("shop", "errorOrder", fmt.Sprintf(`{"order_id":"%s"}`, orderInfo.OrderID))
		h.Call("shop", "undoSubtractInventory", reserveReqStr)
		return fmt.Sprintf(`{"order_id":"%s","status":"payment_timeout"}`, orderInfo.OrderID)
	}

	// Step 5a: Payment received
	h.DurableLog("Payment received for order " + orderInfo.OrderID)
	h.Call("shop", "markOrderPaid", sr.Payload)

	// Step 6: Start dispatchOrder child workflow
	childInput, _ := json.Marshal(map[string]string{"order_id": orderInfo.OrderID})
	runID, err := h.ChildWorkflow("dispatchOrder", string(childInput))
	if err != nil {
		h.DurableLog("Warning: dispatch start failed: " + err.Error())
	} else {
		h.DurableLog("Started dispatch for order " + orderInfo.OrderID + " runId=" + runID)
	}

	// Step 7: Return success
	result, _ := json.Marshal(map[string]string{
		"order_id": orderInfo.OrderID,
		"status":   "paid",
	})
	h.SetQueryState("final_result", string(result))
	return string(result)
}

// runDispatchWorkflow is the Go equivalent of the AS dispatchOrder.
func runDispatchWorkflow(h cleat.HostCalls, shop *ShopService, inputJSON string) string {
	var input struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return `{"error":"invalid input"}`
	}

	for i := 0; i < 10; i++ {
		h.DurableSleep(1 * time.Second)
		step := i + 1
		progressReq, _ := json.Marshal(map[string]string{
			"order_id": input.OrderID,
			"progress": fmt.Sprintf("step %d/10", step),
		})
		h.Call("shop", "updateOrderProgress", string(progressReq))
	}

	return fmt.Sprintf(`{"order_id":"%s","status":"delivered"}`, input.OrderID)
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

type HTTPServer struct {
	shop   *ShopService
	runner *WorkflowRunner
}

func NewHTTPServer(shop *ShopService, runner *WorkflowRunner) *HTTPServer {
	return &HTTPServer{shop: shop, runner: runner}
}

// POST /checkout/{product}/{quantity}
func (s *HTTPServer) handleCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/checkout/"), "/")
	if len(parts) < 2 {
		http.Error(w, "usage: POST /checkout/{product}/{quantity}", http.StatusBadRequest)
		return
	}
	product := parts[0]
	quantity := 1
	if len(parts) >= 2 {
		fmt.Sscanf(parts[1], "%d", &quantity)
	}

	wfID, err := s.runner.StartCheckout(s.shop, product, quantity)
	if err != nil {
		http.Error(w, fmt.Sprintf("workflow start failed: %s", err), http.StatusInternalServerError)
		return
	}

	time.Sleep(200 * time.Millisecond)

	resp, _ := s.shop.retrieveOrders()
	var orders []Order
	json.Unmarshal([]byte(resp), &orders)

	var latestOrder *Order
	for i := len(orders) - 1; i >= 0; i-- {
		if orders[i].Product == product {
			latestOrder = &orders[i]
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"workflow_id": wfID,
		"order":       latestOrder,
		"message":     "Awaiting payment. Send POST /payment_webhook with payment confirmation.",
	})
}

// POST /payment_webhook/{workflowID}
func (s *HTTPServer) handlePaymentWebhook(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	wfID := strings.TrimPrefix(req.URL.Path, "/payment_webhook/")
	if wfID == "" {
		http.Error(w, "workflow ID required in path", http.StatusBadRequest)
		return
	}

	var payload map[string]interface{}
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("bad JSON: %s", err), http.StatusBadRequest)
		return
	}

	payloadJSON, _ := json.Marshal(payload)

	if err := s.runner.DeliverPaymentSignal(wfID, string(payloadJSON)); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "payment signal delivered to workflow",
	})
}

// GET /order/{orderID}
func (s *HTTPServer) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	orderID := strings.TrimPrefix(r.URL.Path, "/order/")
	if orderID == "" {
		http.Error(w, "order ID required", http.StatusBadRequest)
		return
	}

	resp, err := s.shop.retrieveOrder(fmt.Sprintf(`{"order_id":"%s"}`, orderID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(resp))
}

// GET /orders
func (s *HTTPServer) handleListOrders(w http.ResponseWriter, r *http.Request) {
	resp, _ := s.shop.retrieveOrders()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(resp))
}

// GET /product
func (s *HTTPServer) handleGetProducts(w http.ResponseWriter, r *http.Request) {
	s.shop.mu.Lock()
	products := make([]Product, 0)
	for name, qty := range s.shop.inventory {
		price := s.shop.prices[name]
		products = append(products, Product{Name: name, Quantity: qty, Price: price})
	}
	s.shop.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(products)
}

// POST /restock
func (s *HTTPServer) handleRestock(w http.ResponseWriter, r *http.Request) {
	s.shop.mu.Lock()
	s.shop.inventory["widget"] = 100
	s.shop.inventory["gadget"] = 50
	s.shop.inventory["doohickey"] = 200
	s.shop.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "restocked"})
}

// POST /crash
func (s *HTTPServer) handleCrash(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"crashing"}`))
	w.(http.Flusher).Flush()
	log.Println("Crash requested, exiting...")
	os.Exit(1)
}

// GET /
func (s *HTTPServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(htmlPage))
}

// ---------------------------------------------------------------------------
// router
// ---------------------------------------------------------------------------

func (s *HTTPServer) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			s.handleIndex(w, r)
		case r.URL.Path == "/product" || r.URL.Path == "/products":
			s.handleGetProducts(w, r)
		case r.URL.Path == "/orders":
			s.handleListOrders(w, r)
		case r.URL.Path == "/restock":
			s.handleRestock(w, r)
		case r.URL.Path == "/crash":
			s.handleCrash(w, r)
		case strings.HasPrefix(r.URL.Path, "/checkout/"):
			s.handleCheckout(w, r)
		case strings.HasPrefix(r.URL.Path, "/payment_webhook/"):
			s.handlePaymentWebhook(w, r)
		case strings.HasPrefix(r.URL.Path, "/order/"):
			s.handleGetOrder(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	shop := NewShopService()
	runner := NewWorkflowRunner()
	server := NewHTTPServer(shop, runner)

	addr := ":8090"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	log.Printf("Widget-store host listening on %s", addr)
	log.Printf("  POST /checkout/{product}/{quantity}  Start checkout workflow")
	log.Printf("  POST /payment_webhook/{wfID}         Deliver payment signal")
	log.Printf("  GET  /order/{id}                     Get order details")
	log.Printf("  GET  /orders                         List all orders")
	log.Printf("  GET  /product                        List products/inventory")
	log.Printf("  POST /restock                        Reset inventory")
	log.Printf("  POST /crash                          Kill process (crash test)")
	log.Printf("  GET  /                               HTML dashboard")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: server.Router(),
	}

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		httpServer.Close()
	}()

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTML dashboard
// ---------------------------------------------------------------------------

const htmlPage = `<!DOCTYPE html>
<html>
<head>
  <title>Widget Store</title>
  <style>
    body { font-family: sans-serif; max-width: 800px; margin: 40px auto; padding: 0 20px; }
    h1 { color: #333; }
    .endpoint { background: #f5f5f5; padding: 10px; margin: 10px 0; border-radius: 5px; }
    code { background: #e8e8e8; padding: 2px 6px; border-radius: 3px; }
  </style>
</head>
<body>
  <h1>Widget Store (cleat reimplementation)</h1>
  <p>This is a port of the DBOS widget-store demo using cleat's durable execution framework.</p>

  <h2>API Endpoints</h2>
  <div class="endpoint">
    <strong>POST</strong> <code>/checkout/{product}/{quantity}</code><br>
    Start a checkout workflow for the given product.
  </div>
  <div class="endpoint">
    <strong>POST</strong> <code>/payment_webhook/{workflow_id}</code><br>
    Deliver a payment signal to complete a checkout workflow.
  </div>
  <div class="endpoint">
    <strong>GET</strong> <code>/order/{order_id}</code><br>
    Get order details.
  </div>
  <div class="endpoint">
    <strong>GET</strong> <code>/product</code><br>
    List products with current inventory levels.
  </div>
  <div class="endpoint">
    <strong>POST</strong> <code>/restock</code><br>
    Reset inventory to default levels.
  </div>

  <h2>Usage</h2>
  <pre>
# Start a checkout
curl -X POST http://localhost:8090/checkout/widget/1

# Complete payment
curl -X POST http://localhost:8090/payment_webhook/wf-00001 \
  -H "Content-Type: application/json" \
  -d '{"order_id":"order-00001","transaction_id":"txn_abc"}'

# Check order status
curl http://localhost:8090/order/order-00001

# List products
curl http://localhost:8090/product
  </pre>
</body>
</html>`
