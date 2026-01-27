package eebus

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	api "github.com/enbility/eebus-go/api"
	service "github.com/enbility/eebus-go/service"
	usecase_api "github.com/enbility/eebus-go/usecases/api"
	usecase_lpc "github.com/enbility/eebus-go/usecases/cs/lpc"
	usecase_eg_lpc "github.com/enbility/eebus-go/usecases/eg/lpc"
	usecase_ma_mpc "github.com/enbility/eebus-go/usecases/ma/mpc"
	ship_api "github.com/enbility/ship-go/api"
	cert "github.com/enbility/ship-go/cert"
	spine_api "github.com/enbility/spine-go/api"
	spine_model "github.com/enbility/spine-go/model"

	"github.com/grandcat/zeroconf"

	model "github.com/tumbleowlee/eebus-go-rest/server/model"
)

type MPCEventCallback func(ski string, power float64, energy float64, current float64, voltage float64, frequency float64)

type Runtime struct {
	loglevel int

	service    *service.Service
	local_ski  string
	remote_ski []string

	cs_lpc usecase_api.CsLPCInterface
	cs_lpp usecase_api.CsLPPInterface
	eg_lpc usecase_api.EgLPCInterface
	eg_lpp usecase_api.EgLPPInterface
	ma_mpc usecase_api.MaMPCInterface

	mpcCallback MPCEventCallback
}

func NewRuntime(config Config) (*Runtime, error) {
	configuration, err := api.NewConfiguration(config.VendorCode, config.DeviceBrand, config.DeviceModel, config.SerialNumber, config.DeviceType, config.EntityType, config.Port, config.Certificate, config.HeartbeatTimeout)
	if err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(config.Certificate.Certificate[0])
	if err != nil {
		return nil, err
	}

	localSki, err := cert.SkiFromCertificate(leaf)
	if err != nil {
		return nil, err
	}

	if len(config.AlternativeIdentifier) > 1 {
		return nil, errors.New("Provided more than one alternative identifier")
	} else if len(config.AlternativeIdentifier) > 0 {
		configuration.SetAlternateIdentifier(config.AlternativeIdentifier[0])
	}

	var runtime Runtime
	runtime.loglevel = 1 // Set default log level: 0=Error only, 1=Info, 2=Debug, 3=Trace
	runtime.service = service.NewService(configuration, &runtime)
	runtime.service.SetLogging(&runtime)
	runtime.local_ski = localSki

	err = runtime.service.Setup()
	if err != nil {
		return nil, err
	}

	localEntity := runtime.service.LocalDevice().EntityForType(spine_model.EntityTypeTypeCEM)

	runtime.eg_lpc = usecase_eg_lpc.NewLPC(localEntity, nil)
	runtime.service.AddUseCase(runtime.eg_lpc)
	runtime.ma_mpc = usecase_ma_mpc.NewMPC(localEntity, runtime.OnMPCEvent)
	runtime.service.AddUseCase(runtime.ma_mpc)

	/* 	runtime.cs_lpp.SetProductionNominalMax(config.ProductionNominalMax)
	   	runtime.cs_lpp.SetProductionLimit(usecase_api.LoadLimit{
	   		Value:        config.ProductionLimit,
	   		IsChangeable: true,
	   		IsActive:     false,
	   	})
	   	runtime.cs_lpp.SetFailsafeProductionActivePowerLimit(config.ProductionFailsafePowerLimit, true)
	   	runtime.cs_lpp.SetFailsafeDurationMinimum(config.ProductionFailsafeDuration, true) */

	runtime.service.Start()

	runtime.service.SetAutoAccept(true)

	return &runtime, nil
}

func (r *Runtime) SetLogLevel(level int) {
	r.loglevel = level
}

func (r *Runtime) GetLogLevel() int {
	return r.loglevel
}

func (r *Runtime) Stop() {
	r.service.Shutdown()
}

func (r *Runtime) GetLocalSKI() string {
	return r.local_ski
}

func (r *Runtime) GetRemoteSKIs() []string {
	return r.remote_ski
}

func (r *Runtime) ServicePairingDetailUpdate(ski string, detail *ship_api.ConnectionStateDetail) {
	r.Infof("Pairing detail update for SKI %s: State=%v", ski, detail.State())

	switch detail.State() {
	case ship_api.ConnectionStateRemoteDeniedTrust:
		r.Infof("Remote service %s denied trust", ski)
		r.service.CancelPairingWithSKI(ski)
		r.service.UnregisterRemoteSKI(ski)

	case ship_api.ConnectionStateReceivedPairingRequest:
		r.Infof("Received pairing request from %s, approving...", ski)
		// r.service.AllowWaitingForTrust()

	case ship_api.ConnectionStateCompleted:
		r.Infof("Connection with %s completed successfully", ski)

	case ship_api.ConnectionStateError:
		r.Infof("Connection error with %s", ski)
	}
}

func (r *Runtime) RegisterSKI(ski string) {
	r.Infof("Registering remote SKI: %s", ski)
	r.remote_ski = append(r.remote_ski, ski)
	r.service.RegisterRemoteSKI(ski)
	r.Infof("Remote SKIs registered: %v", r.remote_ski)
}

func (r *Runtime) OnLPCEvent(ski string, device spine_api.DeviceRemoteInterface, entity spine_api.EntityRemoteInterface, event api.EventType) {
	switch event {
	case usecase_lpc.WriteApprovalRequired:
		pending := r.cs_lpc.PendingConsumptionLimits()
		for counter := range pending {
			r.cs_lpc.ApproveOrDenyConsumptionLimit(counter, true, "")
		}
	}
}

func (r *Runtime) GetLPC() (float64, error) {
	limit, err := r.cs_lpc.ConsumptionLimit()
	if err != nil {
		return 0, err
	}
	return limit.Value, nil
}

func (r *Runtime) OnLPPEvent(ski string, device spine_api.DeviceRemoteInterface, entity spine_api.EntityRemoteInterface, event api.EventType) {
	switch event {
	case usecase_lpc.WriteApprovalRequired:
		pending := r.cs_lpp.PendingProductionLimits()
		for counter := range pending {
			r.cs_lpp.ApproveOrDenyProductionLimit(counter, true, "")
		}
	}
}

func (r *Runtime) GetLPP() (float64, error) {
	limit, err := r.cs_lpp.ProductionLimit()
	if err != nil {
		return 0, err
	}
	return limit.Value, nil
}

func (r *Runtime) OnMPCEvent(ski string, device spine_api.DeviceRemoteInterface, entity spine_api.EntityRemoteInterface, event api.EventType) {
	fmt.Printf("DEBUG - MPC Event received from SKI: %s, Event: %v \n", ski, event)

	// When power data is updated, trigger callback
	if r.mpcCallback != nil {
		power, err := r.ma_mpc.Power(entity)
		if err != nil {
			r.Debugf("Failed to get power: %v", err)
		}
		energy, err := r.ma_mpc.EnergyConsumed(entity)
		if err != nil {
			r.Debugf("Failed to get energy: %v", err)
		}
		currentPerPhase, err := r.ma_mpc.CurrentPerPhase(entity)
		if err != nil {
			r.Debugf("Failed to get current: %v", err)
		}
		voltage, err := r.ma_mpc.VoltagePerPhase(entity)
		if err != nil {
			r.Debugf("Failed to get voltage: %v", err)
		}
		frequency, err := r.ma_mpc.Frequency(entity)
		if err != nil {
			r.Debugf("Failed to get frequency: %v", err)
		}

		// Calculate total current (sum of all phases)
		var totalCurrent float64
		for _, current := range currentPerPhase {
			totalCurrent += current
		}

		var totalVoltage float64
		for _, volt := range voltage {
			totalVoltage += volt
		}
		var avgVoltage float64 = totalVoltage / float64(len(voltage))

		fmt.Printf("DEBUG - Calling MPC callback with: Power=%.2f, Energy=%.2f, Current=%.2f, Voltage=%.2f, Frequency=%.2f \n", power, energy, totalCurrent, avgVoltage, frequency)
		r.mpcCallback(ski, power, energy, totalCurrent, avgVoltage, frequency)
	} else {
		r.Debugf("MPC callback is nil")
	}
}

func (r *Runtime) SetMPCCallback(callback MPCEventCallback) {
	r.mpcCallback = callback
}

func (r *Runtime) StartSimulation(skis []string) error {
	for _, ski := range skis {
		if ski == r.local_ski {
			continue
		}
		r.RegisterSKI(ski)
	}
	return nil
}

func (r *Runtime) StopSimulation() error {
	for _, ski := range r.remote_ski {
		r.service.UnregisterRemoteSKI(ski)
	}
	r.remote_ski = []string{}
	return nil
}

func (r *Runtime) MDNSDiscovery(timeout time.Duration) ([]model.Device, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}

	entries := make(chan *zeroconf.ServiceEntry)
	foundServices := []model.Service{}

	results := []model.Device{}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	go func() {
		for entry := range entries {
			service := model.Service{
				Instance: entry.Instance,
				HostName: entry.HostName,
				Port:     entry.Port,
				Text:     entry.Text,
			}

			for _, ip := range entry.AddrIPv4 {
				service.IPs = append(service.IPs, ip.String())
			}

			for _, ip := range entry.AddrIPv6 {
				service.IPs = append(service.IPs, ip.String())
			}

			foundServices = append(foundServices, service)
		}
	}()

	if err := resolver.Browse(ctx, "_ship._tcp", "local.", entries); err != nil {
		return nil, err
	}

	<-ctx.Done()

	// fmt.Println("Discovered services: ", len(foundServices))

	for _, service := range foundServices {
		// fmt.Printf("- %s (%s:%d) | TEXT: %s\n", service.Instance, service.HostName, service.Port, service.Text)

		generalInfo := model.GeneralInfo{}
		shipInfo := model.SHIPInfo{}
		ski := ""

		shipInfo.HostAddress = service.IPs[0]
		shipInfo.Port = service.Port
		shipInfo.InstanceName = strings.ReplaceAll(service.Instance, "\\", "")

		for _, text := range service.Text {
			parts := strings.SplitN(text, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := parts[0]
			value := parts[1]
			switch key {
			case "id":
				shipInfo.ShipId = value
			case "DeviceName":
				generalInfo.DeviceName = value
			case "brand":
				generalInfo.Brand = value
			case "vendor":
				generalInfo.Vendor = value
			case "serial":
				generalInfo.SerialNumber = value
			case "model":
				generalInfo.Model = value
			case "type":
				generalInfo.Type = value
			case "SpineDeviceAddress":
				generalInfo.SpineDeviceAddress = value
			case "ski":
				ski = value
			}
		}

		// Ignore own device (skip if SKI matches local_ski)
		if ski == r.local_ski {
			continue
		}

		device := model.Device{
			GeneralInfo: generalInfo,
			SHIPInfo:    shipInfo,
			Ski:         ski,
		}

		results = append(results, device)
	}

	return results, nil
}

func (r *Runtime) RemoteSKIConnected(service api.ServiceInterface, ski string) {
	r.Infof("Remote SKI connected: %s", ski)
}

func (r *Runtime) RemoteSKIDisconnected(service api.ServiceInterface, ski string) {
	r.Infof("Remote SKI disconnected: %s", ski)
}

func (r *Runtime) ServiceShipIDUpdate(ski string, shipID string) {
	r.Debugf("SHIP ID updated for SKI %s: %s", ski, shipID)
}

func (r *Runtime) VisibleRemoteServicesUpdated(service api.ServiceInterface, entries []ship_api.RemoteService) {
	r.Debugf("Visible remote services updated: %d entries", len(entries))
}
func (r *Runtime) Trace(args ...any) {
	if r.loglevel > 2 {
		r.print("TRACE", args...)
	}
}

func (r *Runtime) Tracef(format string, args ...any) {
	if r.loglevel > 2 {
		r.printFormat("TRACE", format, args...)
	}
}

func (r *Runtime) Debug(args ...any) {
	if r.loglevel > 1 {
		r.print("DEBUG", args...)
	}
}

func (r *Runtime) Debugf(format string, args ...any) {
	if r.loglevel > 1 {
		r.printFormat("DEBUG", format, args...)
	}
}

func (r *Runtime) Info(args ...any) {
	if r.loglevel > 0 {
		r.print("INFO ", args...)
	}
}

func (r *Runtime) Infof(format string, args ...any) {
	if r.loglevel > 0 {
		r.printFormat("INFO ", format, args...)
	}
}

func (r *Runtime) Error(args ...any) {
	r.print("ERROR", args...)
}

func (r *Runtime) Errorf(format string, args ...any) {
	r.printFormat("ERROR", format, args...)
}

func (r *Runtime) currentTimestamp() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func (r *Runtime) print(msgType string, args ...any) {
	value := fmt.Sprintln(args...)
	fmt.Printf("%s %s %s", r.currentTimestamp(), msgType, value)
}

func (r *Runtime) printFormat(msgType, format string, args ...any) {
	value := fmt.Sprintf(format, args...)
	fmt.Println(r.currentTimestamp(), msgType, value)
}
