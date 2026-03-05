package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

// Config
var (
	ApiKey    = os.Getenv("BINANCE_API_KEY")
	ApiSecret = os.Getenv("BINANCE_API_SECRET")
	Symbol    = os.Getenv("SYMBOL") // e.g., "HYPEUSDT"
	BaseURL   = "https://fapi.binance.com"
	WsURL     = "wss://fstream.binance.com/ws"
)

// Data Types
type Tick struct {
	Type  string  `json:"type"`
	Price float64 `json:"price"`
	Time  int64   `json:"time"`
}

type OrderRequest struct {
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`
	Type     string  `json:"type"`
	Quantity float64 `json:"quantity"`
	Price    float64 `json:"price,omitempty"`
}

func main() {
	if Symbol == "" {
		Symbol = "HYPEUSDT"
	}

	// 1. Start HTTP Server for Orders
	go startServer()

	// 2. Connect to WebSocket
	connectWebSocket()
}

func startServer() {
	http.HandleFunc("/order", handleOrder)
	http.HandleFunc("/position", handlePosition)
	http.HandleFunc("/openOrders", handleOpenOrders)
	http.HandleFunc("/cancelAll", handleCancelAll)
	
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Go Service listening on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func connectWebSocket() {
	// Construct stream URL: e.g., hypeusdt@aggTrade
	stream := fmt.Sprintf("%s@aggTrade", Symbol) // Lowercase needed? Usually yes.
	u := url.URL{Scheme: "wss", Host: "fstream.binance.com", Path: "/ws/" + stream}
	
	log.Printf("Connecting to %s", u.String())

	for {
		c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			log.Printf("WS Dial error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		
		log.Println("WS Connected")

		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				log.Printf("WS Read error: %v", err)
				break
			}
			
			// Parse simple aggTrade to get price
			var trade map[string]interface{}
			if err := json.Unmarshal(message, &trade); err != nil {
				continue
			}
			
			// Extract price "p"
			if pStr, ok := trade["p"].(string); ok {
				// We just print JSON to stdout for Python to pick up
				// Wrap it in a standard format
				out := fmt.Sprintf(`{"type":"TICK", "symbol":"%s", "price":%s, "ts":%v}`, Symbol, pStr, time.Now().UnixMilli())
				fmt.Println(out) // STDOUT -> Python
			}
		}
		c.Close()
		time.Sleep(1 * time.Second)
	}
}

// --- HTTP Handlers (Proxy to Binance) ---

func handleOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	
	// Call Binance API
	res, err := binanceRequest("POST", "/fapi/v1/order", map[string]string{
		"symbol": req.Symbol,
		"side": req.Side,
		"type": req.Type,
		"quantity": fmt.Sprintf("%f", req.Quantity),
		"price": fmt.Sprintf("%f", req.Price),
		"timeInForce": "GTC",
	})
	
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Write(res)
}

func handleCancelAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	symbol := r.URL.Query().Get("symbol")
	res, err := binanceRequest("DELETE", "/fapi/v1/allOpenOrders", map[string]string{
		"symbol": symbol,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Write(res)
}

func handlePosition(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	res, err := binanceRequest("GET", "/fapi/v2/positionRisk", map[string]string{
		"symbol": symbol,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Write(res)
}

func handleOpenOrders(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	res, err := binanceRequest("GET", "/fapi/v1/openOrders", map[string]string{
		"symbol": symbol,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Write(res)
}

// --- Binance Helper ---

func binanceRequest(method, endpoint string, params map[string]string) ([]byte, error) {
	client := &http.Client{}
	
	// Sign
	q := url.Values{}
	for k, v := range params {
		if v != "" && v != "0.000000" { // simple filter
			q.Add(k, v)
		}
	}
	q.Add("timestamp", fmt.Sprintf("%d", time.Now().UnixMilli()))
	
	sig := sign(q.Encode(), ApiSecret)
	q.Add("signature", sig)
	
	reqURL := BaseURL + endpoint + "?" + q.Encode()
	
	req, _ := http.NewRequest(method, reqURL, nil)
	req.Header.Add("X-MBX-APIKEY", ApiKey)
	
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Binance Error: %s", string(body))
	}
	return body, nil
}

func sign(data, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}
