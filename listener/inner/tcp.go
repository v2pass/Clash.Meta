package inner

import (
	"github.com/Ruk1ng001/Clash.Meta/adapter/inbound"
	C "github.com/Ruk1ng001/Clash.Meta/constant"
	"net"
)

var tcpIn chan<- C.ConnContext

func New(in chan<- C.ConnContext) {
	tcpIn = in
}

func HandleTcp(dst string, host string) net.Conn {
	conn1, conn2 := net.Pipe()
	context := inbound.NewInner(conn2, dst, host)
	tcpIn <- context
	return conn1
}
