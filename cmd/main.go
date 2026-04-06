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
	syscall "syscall"
	time "time"

	cert "github.com/enbility/ship-go/cert"
	spine_model "github.com/enbility/spine-go/model"
	"github.com/gin-contrib/cors"
	gin "github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tumbleowlee/eebus-go-rest/server/eebus"
	model "github.com/tumbleowlee/eebus-go-rest/server/model"

	rand "math/rand/v2"
)

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

	idNum := "0"

	for i := 0; i < 10; i++ {
		idNum += strconv.Itoa(rand.IntN(10))
	}

	config := eebus.Config{
		VendorCode:                    "vendorCode",
		DeviceBrand:                   "EEBUSControl",
		DeviceModel:                   "Simulator",
		SerialNumber:                  "SIM-" + idNum,
		DeviceType:                    spine_model.DeviceTypeTypeEnergyManagementSystem,
		EntityType:                    []spine_model.EntityTypeType{spine_model.EntityTypeTypeCEM},
		AlternativeIdentifier:         []string{"EEBUSControl-HEMS-" + idNum},
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

	messageQueue := model.MessageQueue{}

	runtime, err := eebus.NewRuntime(config)
	if err != nil {
		log.Println(err)
		return
	}
	defer runtime.Stop()

	go runtime.Hub.Run()

	// Register MPC event callback to broadcast power updates
	runtime.SetMPCCallback(func(ski string, power float64, energy float64, current float64, voltage float64, frequency float64) {
		if runtime.Hub.SimulationRunning && runtime.Hub.IsDeviceSimulated(ski) {
			runtime.Hub.SendMessage(model.Message{Type: "mpc_update", Data: gin.H{
				"ski":       ski,
				"power":     power,
				"energy":    energy,
				"current":   current,
				"voltage":   voltage,
				"frequency": frequency,
			}})
			log.Printf("MPC Update from %s: Power=%.2f W, Energy=%.2f Wh, Current=%.2f A, Voltage=%.2f V, Frequency=%.2f Hz", ski, power, energy, current, voltage, frequency)
		} else {
			log.Printf("MPC Update from %s ignored (not in simulation)", ski)
		}
	})

	runtime.SetLPCCallback(func(ski string, consumptionNominalMax float64) {
		if runtime.Hub.SimulationRunning && runtime.Hub.IsDeviceSimulated(ski) {
			runtime.Hub.SendMessage(model.Message{Type: "lpc_update", Data: gin.H{
				"ski":                     ski,
				"consumption_nominal_max": consumptionNominalMax,
			}})
			log.Printf("LPC Update from %s: ConsumptionNominalMax=%.2f W", ski, consumptionNominalMax)
		} else {
			log.Printf("LPC Update from %s ignored (not in simulation)", ski)

			messageQueue.Enqueue(model.Message{Type: "lpc_update", Data: gin.H{
				"ski":                     ski,
				"consumption_nominal_max": consumptionNominalMax,
			}})
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
	go runtime.Hub.StartContinuousDiscovery(runtime)

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
		runtime.Hub.Register <- conn

		// Send initial connection message
		runtime.Hub.SendToClient(conn, model.Message{Type: "connected", Data: gin.H{
			"message": "WebSocket connection established",
			"ski":     runtime.GetLocalSKI(),
		}})

		// Handle incoming messages
		go func() {
			defer func() {
				runtime.Hub.Unregister <- conn
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
					runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Invalid JSON"}})
					continue
				}

				msgType, ok := msg["type"].(string)
				if !ok {
					runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Missing message type"}})
					continue
				}

				fmt.Println("Received WS message ", string(message))

				// Handle different message types
				switch msgType {
				case "get_local_ski":
					runtime.Hub.SendToClient(conn, model.Message{Type: "local_ski", Data: gin.H{
						"ski": runtime.GetLocalSKI(),
					}})

				case "get_remote_skis":
					runtime.Hub.SendToClient(conn, model.Message{Type: "remote_skis", Data: gin.H{
						"remotes": runtime.GetRemoteSKIs(),
					}})

				case "register_ski":
					data, _ := msg["data"].(map[string]interface{})
					if ski, ok := data["ski"].(string); ok {
						runtime.RegisterSKI(ski)
						runtime.Hub.SendToClient(conn, model.Message{Type: "ski_registered", Data: gin.H{"ski": ski}})
						runtime.Hub.SendMessage(model.Message{Type: "ski_registered", Data: gin.H{"ski": ski}})
					} else {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Invalid SKI data"}})
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
						runtime.Hub.SendToClient(conn, model.Message{Type: "skis_registered", Data: gin.H{"skis": skis}})
						runtime.Hub.SendMessage(model.Message{Type: "skis_registered", Data: gin.H{"skis": skis}})
					} else {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Invalid SKIs data"}})
					}

				case "get_lpp":
					lpp, err := runtime.GetLPP()
					if err != nil {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": err.Error()}})
					} else {
						runtime.Hub.SendToClient(conn, model.Message{Type: "lpp", Data: gin.H{"limit": lpp}})
					}

				case "get_lpc":
					lpc, err := runtime.GetLPC()
					if err != nil {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": err.Error()}})
					} else {
						runtime.Hub.SendToClient(conn, model.Message{Type: "lpc", Data: gin.H{"limit": lpc}})
					}

				case "get_log_level":
					runtime.Hub.SendToClient(conn, model.Message{Type: "log_level", Data: gin.H{"level": runtime.GetLogLevel()}})

				case "set_log_level":
					data, _ := msg["data"].(map[string]interface{})
					if level, ok := data["level"].(string); ok {
						intLevel, err := strconv.Atoi(level)
						if err != nil {
							runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Invalid log level"}})
							break
						}
						runtime.SetLogLevel(intLevel)
						runtime.Hub.SendToClient(conn, model.Message{Type: "log_level_changed", Data: gin.H{"level": level}})
						runtime.Hub.SendMessage(model.Message{Type: "log_level_changed", Data: gin.H{"level": level}})
					} else {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Invalid log level"}})
					}

				case "mdns_discovery":
					results, err := runtime.MDNSDiscovery(2 * time.Second)
					if err != nil {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": err.Error()}})
					} else {
						runtime.Hub.SendToClient(conn, model.Message{Type: "mdns_discovery", Data: results})
						runtime.Hub.SendMessage(model.Message{Type: "mdns_discovery", Data: results})
					}

				case "start_simulation":
					data, _ := msg["data"].(map[string]interface{})
					if !runtime.Hub.SimulationRunning {
						if devicesData, ok := data["devices"]; ok {
							jsonData, err := json.Marshal(devicesData)
							if err != nil {
								runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Invalid devices data"}})
								break
							}

							var devices []string
							if err := json.Unmarshal(jsonData, &devices); err != nil {
								runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Invalid devices format"}})
								break
							}

							for _, ski := range devices {
								runtime.Hub.SetDeviceSimulated(ski, true)
								log.Printf("Device %s added to simulation", ski)
							}

							err = runtime.StartSimulation(devices)
							if err != nil {
								runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": err.Error()}})
								runtime.Hub.SendMessage(model.Message{Type: "simulation_error", Data: gin.H{"error": err.Error()}})
							} else {
								runtime.Hub.SimulationRunning = true
								runtime.Hub.SendToClient(conn, model.Message{Type: "simulation_started", Data: gin.H{
									"status":  "Simulation started",
									"devices": devices,
								}})
								runtime.Hub.SendMessage(model.Message{Type: "simulation_started", Data: gin.H{
									"status":  "Simulation started",
									"devices": devices,
								}})

								if !messageQueue.IsEmpty() {
									log.Printf("Queued messages (%d) after simulation start", messageQueue.Size())
									for !messageQueue.IsEmpty() {
										msg, err := messageQueue.Dequeue()
										if err != nil {
											break
										}
										if data, ok := msg.Data.(map[string]interface{}); ok {
											if ski, ok := data["ski"].(string); ok && runtime.Hub.IsDeviceSimulated(ski) {
												log.Printf("Sending queued message for SKI %s (%d messages left) after simulation start: %v", ski, messageQueue.Size(), msg)
												runtime.Hub.SendMessage(msg)
											}
										}
									}
								}
							}
						} else {
							runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Missing devices data"}})
						}
					}

				case "stop_simulation":
					err := runtime.StopSimulation()
					if err != nil {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": err.Error()}})
						runtime.Hub.SendMessage(model.Message{Type: "simulation_error", Data: gin.H{"error": err.Error()}})
					}
					runtime.Hub.SimulationRunning = false
					runtime.Hub.ClearAllSimulated()
					runtime.Hub.SendToClient(conn, model.Message{Type: "simulation_stopped", Data: gin.H{"status": "Simulation stopped"}})
					runtime.Hub.SendMessage(model.Message{Type: "simulation_stopped", Data: gin.H{"status": "Simulation stopped"}})

				case "add_device":
					data, _ := msg["data"].(map[string]interface{})

					if ski, ok := data["ski"].(string); ok {
						if runtime.Hub.SimulationRunning {
							runtime.Hub.SetDeviceSimulated(ski, true)
							log.Printf("Device %s added to running simulation", ski)
						}
						runtime.Hub.SendToClient(conn, model.Message{Type: "device_added", Data: gin.H{"ski": ski}})
					} else {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Missing SKI data"}})
					}

				case "remove_device":
					data, _ := msg["data"].(map[string]interface{})

					if ski, ok := data["ski"].(string); ok {
						if runtime.Hub.SimulationRunning {
							runtime.Hub.SetDeviceSimulated(ski, false)
							log.Printf("Device %s removed from simulation", ski)
						}
						runtime.Hub.SendToClient(conn, model.Message{Type: "device_removed", Data: gin.H{"ski": ski}})
					} else {
						runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Missing SKI data"}})
					}

				default:
					runtime.Hub.SendToClient(conn, model.Message{Type: "error", Data: gin.H{"error": "Unknown message type: " + msgType}})
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
