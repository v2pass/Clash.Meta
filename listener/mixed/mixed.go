package mixed

import (
	"github.com/Ruk1ng001/Clash.Meta/adapter/inbound"
	"net"

	"github.com/Ruk1ng001/Clash.Meta/common/cache"
	N "github.com/Ruk1ng001/Clash.Meta/common/net"
	C "github.com/Ruk1ng001/Clash.Meta/constant"
	"github.com/Ruk1ng001/Clash.Meta/listener/http"
	"github.com/Ruk1ng001/Clash.Meta/listener/socks"
	"github.com/Ruk1ng001/Clash.Meta/transport/socks4"
	"github.com/Ruk1ng001/Clash.Meta/transport/socks5"
)

type Listener struct {
	listener net.Listener
	addr     string
	cache    *cache.LruCache[string, bool]
	closed   bool
}

// RawAddress implements C.Listener
func (l *Listener) RawAddress() string {
	return l.addr
}

// Address implements C.Listener
func (l *Listener) Address() string {
	return l.listener.Addr().String()
}

// Close implements C.Listener
func (l *Listener) Close() error {
	l.closed = true
	return l.listener.Close()
}

func New(addr string, in chan<- C.ConnContext, additions ...inbound.Addition) (*Listener, error) {
	if len(additions) == 0 {
		additions = []inbound.Addition{
			inbound.WithInName("DEFAULT-MIXED"),
			inbound.WithSpecialRules(""),
		}
	}
	l, err := inbound.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	ml := &Listener{
		listener: l,
		addr:     addr,
		cache:    cache.New[string, bool](cache.WithAge[string, bool](30)),
	}
	go func() {
		for {
			c, err := ml.listener.Accept()
			if err != nil {
				if ml.closed {
					break
				}
				continue
			}
			go handleConn(c, in, ml.cache, additions...)
		}
	}()

	return ml, nil
}

func handleConn(conn net.Conn, in chan<- C.ConnContext, cache *cache.LruCache[string, bool], additions ...inbound.Addition) {
	conn.(*net.TCPConn).SetKeepAlive(true)

	bufConn := N.NewBufferedConn(conn)
	head, err := bufConn.Peek(1)
	if err != nil {
		return
	}

	switch head[0] {
	case socks4.Version:
		socks.HandleSocks4(bufConn, in, additions...)
	case socks5.Version:
		socks.HandleSocks5(bufConn, in, additions...)
	default:
		http.HandleConn(bufConn, in, cache, additions...)
	}
}
