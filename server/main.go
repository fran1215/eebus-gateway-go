package main

import (
	context "context"
	cert "github.com/enbility/ship-go/cert"
	spine_model "github.com/enbility/spine-go/model"
	gin "github.com/gin-gonic/gin"
	runtime "github.com/tumbleowlee/eebus-go-rest/server/eebus"
	model "github.com/tumbleowlee/eebus-go-rest/server/model"
	log "log"
	http "net/http"
	os "os"
	signal "os/signal"
	syscall "syscall"
	time "time"
	"github.com/gin-contrib/cors"
)

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

	router.GET("/api/ski/local", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"ski": runtime.GetLocalSKI(),
		})
	})

	router.GET("/api/ski/remotes", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"remotes": runtime.GetRemoteSKIs(),
		})
	})

	router.POST("/api/ski/remote", func(c *gin.Context) {
		var remote model.Ski
		if err := c.ShouldBindJSON(&remote); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		} else {
			runtime.RegisterSKI(remote.Ski)
		}
	})

	router.POST("/api/ski/remotes", func(c *gin.Context) {
		var remotes model.SkiList
		if err := c.ShouldBindJSON(&remotes); err != nil || len(remotes.Ski) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		} else {
			for _, ski := range remotes.Ski {
				runtime.RegisterSKI(ski)
			}
		}
	})

	router.GET("/api/lpp", func(c *gin.Context) {
		lpp, err := runtime.GetLPP()
		if err != nil {
			c.JSON(http.StatusTooEarly, gin.H{
				"error": err.Error(),
			})
		} else {
			c.JSON(http.StatusOK, gin.H{
				"limit": lpp,
			})
		}
	})

	router.GET("/api/lpc", func(c *gin.Context) {
		lpc, err := runtime.GetLPC()
		if err != nil {
			c.JSON(http.StatusTooEarly, gin.H{
				"error": err.Error(),
			})
		} else {
			c.JSON(http.StatusOK, gin.H{
				"limit": lpc,
			})
		}
	})

	router.GET("/api/log", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"level": runtime.GetLogLevel()})
	})

	router.GET("/api/mdns/discovery", func(c *gin.Context) {
		results, err := runtime.MDNSDiscovery(2 * time.Second)
		
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusOK, results)
		}
	})

	router.POST("/api/log", func(c *gin.Context) {
		var level model.LogLevel
		if err := c.ShouldBindJSON(&level); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		} else {
			runtime.SetLogLevel(level.Level)
		}
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
