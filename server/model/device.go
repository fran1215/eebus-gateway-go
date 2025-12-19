package model

type GeneralInfo struct {
	SpineDeviceAddress      string `json:"spineDeviceAddress"`
	DeviceName		string `json:"deviceName"`
	Brand 			string `json:"brand"`
	Vendor 			string `json:"vendor"`
	SerialNumber 	        string `json:"serialNumber"`
	Model			string `json:"model"`
	Type 			string `json:"type"`
}

type SHIPInfo struct {
	ShipId      string `json:"shipId"`
	InstanceName string `json:"instanceName"`
	HostAddress  string `json:"hostAddress"`
	Port	 int    `json:"port"`
}

type Device struct {
	GeneralInfo 	GeneralInfo `json:"generalInfo"`
	SHIPInfo    	SHIPInfo    `json:"shipInfo"`
	Ski       	string      `json:"ski"`
}