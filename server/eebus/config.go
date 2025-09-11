package eebus

import (
	tls "crypto/tls"
	spine_model "github.com/enbility/spine-go/model"
	time "time"
)

type Config struct {
	VendorCode            string
	DeviceBrand           string
	DeviceModel           string
	SerialNumber          string
	DeviceType            spine_model.DeviceTypeType
	EntityType            []spine_model.EntityTypeType
	AlternativeIdentifier []string

	Port             int
	Certificate      tls.Certificate
	HeartbeatTimeout time.Duration

	ConsumptionNominalMax         float64
	ConsumptionLimit              float64
	ConsumptionFailsafePowerLimit float64
	ConsumptionFailsafeDuration   time.Duration

	ProductionNominalMax         float64
	ProductionLimit              float64
	ProductionFailsafePowerLimit float64
	ProductionFailsafeDuration   time.Duration
}
