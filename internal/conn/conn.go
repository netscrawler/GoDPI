package conn

import (
	"fmt"
	"time"
)

type ConnectionInfo struct {
	SrcIP      string
	DstDomain  string
	Method     string
	StartTime  time.Time
	TrafficIN  uint64
	TrafficOUT uint64
}

func (c *ConnectionInfo) String() string {
	return fmt.Sprintf("%s %s %s %s",
		c.StartTime.Format("2006-01-02 15:04:05"),
		c.SrcIP,
		c.Method,
		c.DstDomain)
}

func NewConnectionInfo(srcIP string, dstDomain string, method string) *ConnectionInfo {
	return &ConnectionInfo{
		SrcIP:      srcIP,
		DstDomain:  dstDomain,
		Method:     method,
		StartTime:  time.Now(),
		TrafficIN:  0,
		TrafficOUT: 0,
	}
}
