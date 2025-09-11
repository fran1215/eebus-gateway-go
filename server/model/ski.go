package model

type Ski struct {
	Ski string `form:"ski" json:"ski" xml:"ski" binding:"required"`
}

type SkiList struct {
	Ski []string `form:"ski" json:"ski" xml:"ski" binding:"required"`
}
