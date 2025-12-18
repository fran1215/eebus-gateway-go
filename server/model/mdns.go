package model

type Service struct {
	Instance string 	`json:"instance"`
	HostName string 	`json:"hostName"`
	IPs      []string 	`json:"ips"`
	Port     int 		`json:"port"`
	Text     []string 	`json:"text"`
}