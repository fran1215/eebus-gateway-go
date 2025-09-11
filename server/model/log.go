package model

type LogLevel struct {
	Level int `form:"level" json:"level" xml:"level" binding:"required"`
}
