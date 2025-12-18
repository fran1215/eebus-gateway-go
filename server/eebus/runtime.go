package eebus

import (
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	api "github.com/enbility/eebus-go/api"
	service "github.com/enbility/eebus-go/service"
	usecase_api "github.com/enbility/eebus-go/usecases/api"
	usecase_vabd "github.com/enbility/eebus-go/usecases/cem/vabd"
	usecase_vapd "github.com/enbility/eebus-go/usecases/cem/vapd"
	usecase_lpc "github.com/enbility/eebus-go/usecases/cs/lpc"
	usecase_lpp "github.com/enbility/eebus-go/usecases/cs/lpp"
	usecase_eg_lpc "github.com/enbility/eebus-go/usecases/eg/lpc"
	usecase_eg_lpp "github.com/enbility/eebus-go/usecases/eg/lpp"
	usecase_mgcp "github.com/enbility/eebus-go/usecases/ma/mgcp"
	ship_api "github.com/enbility/ship-go/api"
	cert "github.com/enbility/ship-go/cert"
	spine_api "github.com/enbility/spine-go/api"
	spine_model "github.com/enbility/spine-go/model"

	model "github.com/fran1215/eebus-go-rest/model"

)

type Runtime struct {
	loglevel int

	service    *service.Service
	local_ski  string
	remote_ski []string

	cs_lpc   usecase_api.CsLPCInterface
	cs_lpp   usecase_api.CsLPPInterface
	eg_lpc   usecase_api.EgLPCInterface
	eg_lpp   usecase_api.EgLPPInterface
	ma_mgcp  usecase_api.MaMGCPInterface
	cem_vapd usecase_api.CemVAPDInterface
	cem_vabd usecase_api.CemVABDInterface
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
	runtime.service = service.NewService(configuration, &runtime)
	runtime.service.SetLogging(&runtime)
	runtime.local_ski = localSki

	err = runtime.service.Setup()
	if err != nil {
		return nil, err
	}

	localEntity := runtime.service.LocalDevice().EntityForType(spine_model.EntityTypeTypeCEM)

	runtime.cs_lpc = usecase_lpc.NewLPC(localEntity, runtime.OnLPCEvent)
	runtime.service.AddUseCase(runtime.cs_lpc)
	runtime.cs_lpp = usecase_lpp.NewLPP(localEntity, runtime.OnLPPEvent)
	runtime.service.AddUseCase(runtime.cs_lpp)
	runtime.eg_lpc = usecase_eg_lpc.NewLPC(localEntity, nil)
	runtime.service.AddUseCase(runtime.eg_lpc)
	runtime.eg_lpp = usecase_eg_lpp.NewLPP(localEntity, nil)
	runtime.service.AddUseCase(runtime.eg_lpp)
	runtime.ma_mgcp = usecase_mgcp.NewMGCP(localEntity, nil)
	runtime.service.AddUseCase(runtime.ma_mgcp)
	runtime.cem_vabd = usecase_vabd.NewVABD(localEntity, nil)
	runtime.service.AddUseCase(runtime.cem_vabd)
	runtime.cem_vapd = usecase_vapd.NewVAPD(localEntity, nil)
	runtime.service.AddUseCase(runtime.cem_vapd)

	runtime.cs_lpc.SetConsumptionNominalMax(config.ConsumptionNominalMax)
	runtime.cs_lpc.SetConsumptionLimit(usecase_api.LoadLimit{
		Value:        config.ConsumptionLimit,
		IsChangeable: true,
		IsActive:     false,
	})
	runtime.cs_lpc.SetFailsafeConsumptionActivePowerLimit(config.ConsumptionFailsafePowerLimit, true)
	runtime.cs_lpc.SetFailsafeDurationMinimum(config.ConsumptionFailsafeDuration, true)

	runtime.cs_lpp.SetProductionNominalMax(config.ProductionNominalMax)
	runtime.cs_lpp.SetProductionLimit(usecase_api.LoadLimit{
		Value:        config.ProductionLimit,
		IsChangeable: true,
		IsActive:     false,
	})
	runtime.cs_lpp.SetFailsafeProductionActivePowerLimit(config.ProductionFailsafePowerLimit, true)
	runtime.cs_lpp.SetFailsafeDurationMinimum(config.ProductionFailsafeDuration, true)

	runtime.service.Start()

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
	for _, remoteSki := range r.remote_ski {
		if ski == remoteSki && detail.State() == ship_api.ConnectionStateRemoteDeniedTrust {
			fmt.Println("The remote service denied trust. Exiting.")
			r.service.CancelPairingWithSKI(ski)
			r.service.UnregisterRemoteSKI(ski)
			r.service.Shutdown()
		}
	}
}

func (r *Runtime) RegisterSKI(ski string) {
	r.remote_ski = append(r.remote_ski, ski)
	r.service.RegisterRemoteSKI(ski)
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

func (r *Runtime) MDNSDiscovery(timeout time.Duration) ([]Service, error) {

}

func (r *Runtime) RemoteSKIConnected(service api.ServiceInterface, ski string) {
}

func (r *Runtime) RemoteSKIDisconnected(service api.ServiceInterface, ski string) {
}

func (r *Runtime) ServiceShipIDUpdate(ski string, shipdID string) {
}

func (r *Runtime) VisibleRemoteServicesUpdated(service api.ServiceInterface, entries []ship_api.RemoteService) {
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
