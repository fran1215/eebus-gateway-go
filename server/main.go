package main

import (
	context "context"
	json "encoding/json"
	"fmt"
	log "log"
	http "net/http"
	os "os"
	signal "os/signal"
	strconv "strconv"
	"sync"
	syscall "syscall"
	time "time"

	cert "github.com/enbility/ship-go/cert"
	spine_model "github.com/enbility/spine-go/model"
	"github.com/gin-contrib/cors"
	gin "github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	runtime "github.com/tumbleowlee/eebus-go-rest/server/eebus"
	model "github.com/tumbleowlee/eebus-go-rest/server/model"
)

// WebSocket Hub for managing connections
type Hub struct {
	clients           map[*websocket.Conn]bool
	broadcast         chan []byte
	register          chan *websocket.Conn
	unregister        chan *websocket.Conn
	mu                sync.RWMutex
	discoveredDevices map[string]model.Device // Track discovered devices by SKI
	devicesMu         sync.RWMutex
	simulationDevices map[string]bool // Track which devices are in simulation
	simulationMu      sync.RWMutex
}

func newHub() *Hub {
	return &Hub{
		clients:           make(map[*websocket.Conn]bool),
		broadcast:         make(chan []byte, 256),
		register:          make(chan *websocket.Conn),
		unregister:        make(chan *websocket.Conn),
		discoveredDevices: make(map[string]model.Device),
		simulationDevices: make(map[string]bool),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			log.Printf("Client connected. Total clients: %d", len(h.clients))
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.Close()
			}
			h.mu.Unlock()
			log.Printf("Client disconnected. Total clients: %d", len(h.clients))
		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				err := client.WriteMessage(websocket.TextMessage, message)
				if err != nil {
					log.Printf("Error broadcasting to client: %v", err)
					client.Close()
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *Hub) sendMessage(messageType string, data interface{}) {
	msg := map[string]interface{}{
		"type":      messageType,
		"data":      data,
		"timestamp": time.Now().Unix(),
	}
	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling message: %v", err)
		return
	}
	h.broadcast <- jsonMsg
}

func (h *Hub) sendToClient(conn *websocket.Conn, messageType string, data interface{}) {
	msg := map[string]interface{}{
		"type":      messageType,
		"data":      data,
		"timestamp": time.Now().Unix(),
	}
	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling message: %v", err)
		return
	}
	conn.WriteMessage(websocket.TextMessage, jsonMsg)
}

// Continuous mDNS discovery
func (h *Hub) startContinuousDiscovery(runtime *runtime.Runtime) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	log.Println("Starting continuous mDNS discovery...")

	for range ticker.C {
		results, err := runtime.MDNSDiscovery(3 * time.Second)
		if err != nil {
			log.Printf("mDNS discovery error: %v", err)
			continue
		}

		h.devicesMu.Lock()
		hasNewDevices := false
		newDevices := []model.Device{}

		// Check for new devices
		for _, device := range results {
			if _, exists := h.discoveredDevices[device.Ski]; !exists {
				h.discoveredDevices[device.Ski] = device
				newDevices = append(newDevices, device)
				hasNewDevices = true
				log.Printf("New device discovered: %s (%s)", device.SHIPInfo.InstanceName, device.Ski)
			}
		}
		h.devicesMu.Unlock()

		// Broadcast all devices to all clients
		h.sendMessage("mdns_discovery", results)

		// If there are new devices, send a specific notification
		if hasNewDevices {
			h.sendMessage("new_devices_discovered", gin.H{
				"newDevices": newDevices,
				"allDevices": results,
				"newCount":   len(newDevices),
			})
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development
	},
}

func waitForSignal(srv *http.Server) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutdown Server ...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Println("Server Shutdown:", err)
	}
	log.Println("Server exiting")
}

func main() {
	// For now we recreate a new certificate (and ski) on each start
	// We have to alternatively load it from disk to keep the same SKI
	certificate, err := cert.CreateCertificate("OrganizationUnit", "Organization", "Country", "CommonName")
	if err != nil {
		log.Println(err)
		return
	}

	config := runtime.Config{
		VendorCode:                    "vendorCode",
		DeviceBrand:                   "brand",
		DeviceModel:                   "model",
		SerialNumber:                  "serial",
		DeviceType:                    spine_model.DeviceTypeTypeEnergyManagementSystem,
		EntityType:                    []spine_model.EntityTypeType{spine_model.EntityTypeTypeCEM},
		AlternativeIdentifier:         []string{"Demo-HEMS-123456789"},
		Port:                          1024,
		Certificate:                   certificate,
		HeartbeatTimeout:              4 * time.Second,
		ConsumptionNominalMax:         32000,
		ConsumptionLimit:              32000,
		ConsumptionFailsafePowerLimit: 8000,
		ConsumptionFailsafeDuration:   2 * time.Hour,
		ProductionNominalMax:          32000,
		ProductionLimit:               32000,
		ProductionFailsafePowerLimit:  8000,
		ProductionFailsafeDuration:    2 * time.Hour,
	}

	runtime, err := runtime.NewRuntime(config)
	if err != nil {
		log.Println(err)
		return
	}
	defer runtime.Stop()

	// Initialize WebSocket hub
	hub := newHub()
	go hub.run()

	// Register MPC event callback to broadcast power updates
	runtime.SetMPCCallback(func(ski string, power float64, energy float64, current float64) {
		// Only broadcast MPC updates for devices in the simulation
		hub.simulationMu.RLock()
		isInSimulation := hub.simulationDevices[ski]
		hub.simulationMu.RUnlock()

		if isInSimulation {
			hub.sendMessage("mpc_update", gin.H{
				"ski":     ski,
				"power":   power,
				"energy":  energy,
				"current": current,
			})
			log.Printf("MPC Update from %s: Power=%.2f W, Energy=%.2f Wh, Current=%.2f A", ski, power, energy, current)
		} else {
			log.Printf("MPC Update from %s ignored (not in simulation)", ski)
		}
	})

	// Poll MPC data from all connected devices periodically
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			remoteSkis := runtime.GetRemoteSKIs()
			for _, ski := range remoteSkis {
				// Try to get MPC data for this device
				// The OnMPCEvent callback will be triggered if data is available
				log.Printf("Polling MPC data for device: %s", ski)
			}
		}
	}()

	// Start continuous mDNS discovery
	go hub.startContinuousDiscovery(runtime)

	router := gin.Default()

	// Allow all origins for development
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"http://localhost:4321"}, // your Astro dev server
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// WebSocket endpoint - all communication goes through here
	router.GET("/ws", func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Printf("Failed to upgrade connection: %v", err)
			return
		}
		hub.register <- conn

		// Send initial connection message
		hub.sendToClient(conn, "connected", gin.H{
			"message": "WebSocket connection established",
			"ski":     runtime.GetLocalSKI(),
		})

		// Handle incoming messages
		go func() {
			defer func() {
				hub.unregister <- conn
			}()
			for {
				_, message, err := conn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						log.Printf("WebSocket error: %v", err)
					}
					break
				}

				// Parse incoming message
				var msg map[string]interface{}
				if err := json.Unmarshal(message, &msg); err != nil {
					hub.sendToClient(conn, "error", gin.H{"error": "Invalid JSON"})
					continue
				}

				msgType, ok := msg["type"].(string)
				if !ok {
					hub.sendToClient(conn, "error", gin.H{"error": "Missing message type"})
					continue
				}

				fmt.Println("Received WS message ", string(message))

				// Handle different message types
				switch msgType {
				case "get_local_ski":
					hub.sendToClient(conn, "local_ski", gin.H{
						"ski": runtime.GetLocalSKI(),
					})

				case "get_remote_skis":
					hub.sendToClient(conn, "remote_skis", gin.H{
						"remotes": runtime.GetRemoteSKIs(),
					})

				case "register_ski":
					data, _ := msg["data"].(map[string]interface{})
					if ski, ok := data["ski"].(string); ok {
						runtime.RegisterSKI(ski)
						hub.sendToClient(conn, "ski_registered", gin.H{"ski": ski})
						hub.sendMessage("ski_registered", gin.H{"ski": ski})
					} else {
						hub.sendToClient(conn, "error", gin.H{"error": "Invalid SKI data"})
					}

				case "register_skis":
					data, _ := msg["data"].(map[string]interface{})
					if skisInterface, ok := data["skis"].([]interface{}); ok {
						var skis []string
						for _, s := range skisInterface {
							if ski, ok := s.(string); ok {
								skis = append(skis, ski)
								runtime.RegisterSKI(ski)
							}
						}
						hub.sendToClient(conn, "skis_registered", gin.H{"skis": skis})
						hub.sendMessage("skis_registered", gin.H{"skis": skis})
					} else {
						hub.sendToClient(conn, "error", gin.H{"error": "Invalid SKIs data"})
					}

				case "get_lpp":
					lpp, err := runtime.GetLPP()
					if err != nil {
						hub.sendToClient(conn, "error", gin.H{"error": err.Error()})
					} else {
						hub.sendToClient(conn, "lpp", gin.H{"limit": lpp})
					}

				case "get_lpc":
					lpc, err := runtime.GetLPC()
					if err != nil {
						hub.sendToClient(conn, "error", gin.H{"error": err.Error()})
					} else {
						hub.sendToClient(conn, "lpc", gin.H{"limit": lpc})
					}

				case "get_log_level":
					hub.sendToClient(conn, "log_level", gin.H{"level": runtime.GetLogLevel()})

				case "set_log_level":
					data, _ := msg["data"].(map[string]interface{})
					if level, ok := data["level"].(string); ok {
						intLevel, err := strconv.Atoi(level)
						if err != nil {
							hub.sendToClient(conn, "error", gin.H{"error": "Invalid log level"})
							break
						}
						runtime.SetLogLevel(intLevel)
						hub.sendToClient(conn, "log_level_changed", gin.H{"level": level})
						hub.sendMessage("log_level_changed", gin.H{"level": level})
					} else {
						hub.sendToClient(conn, "error", gin.H{"error": "Invalid log level"})
					}

				case "mdns_discovery":
					results, err := runtime.MDNSDiscovery(2 * time.Second)
					if err != nil {
						hub.sendToClient(conn, "error", gin.H{"error": err.Error()})
					} else {
						hub.sendToClient(conn, "mdns_discovery", results)
						hub.sendMessage("mdns_discovery", results)
					}

				case "start_simulation":
					data, _ := msg["data"].(map[string]interface{})
					if devicesData, ok := data["devices"]; ok {
						// Convert to JSON and back to properly unmarshal
						jsonData, err := json.Marshal(devicesData)
						if err != nil {
							hub.sendToClient(conn, "error", gin.H{"error": "Invalid devices data"})
							break
						}

						fmt.Println(string(jsonData))

						var devices []model.Device
						if err := json.Unmarshal(jsonData, &devices); err != nil {
							hub.sendToClient(conn, "error", gin.H{"error": "Invalid devices format"})
							break
						}

						// Update the simulation devices list
						hub.simulationMu.Lock()
						hub.simulationDevices = make(map[string]bool)
						for _, device := range devices {
							hub.simulationDevices[device.Ski] = true
							log.Printf("Device %s added to simulation", device.Ski)
						}
						hub.simulationMu.Unlock()

						err = runtime.StartSimulation(devices)
						if err != nil {
							hub.sendToClient(conn, "error", gin.H{"error": err.Error()})
							hub.sendMessage("simulation_error", gin.H{"error": err.Error()})
						} else {
							hub.sendToClient(conn, "simulation_started", gin.H{
								"status":  "Simulation started",
								"devices": devices,
							})
							hub.sendMessage("simulation_started", gin.H{
								"status":  "Simulation started",
								"devices": devices,
							})
						}
					} else {
						hub.sendToClient(conn, "error", gin.H{"error": "Missing devices data"})
					}

				default:
					hub.sendToClient(conn, "error", gin.H{"error": "Unknown message type: " + msgType})
				}
			}
		}()
	})

	srv := &http.Server{
		Addr:    ":8080",
		Handler: router.Handler(),
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			runtime.Errorf("listen: %s\n", err)
		}
	}()

	waitForSignal(srv)
}
