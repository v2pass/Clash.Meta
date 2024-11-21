// Modified from: https://github.com/Qv2ray/gun-lite
// License: MIT

package gun

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Ruk1ng001/Clash.Meta/common/buf"
	"github.com/Ruk1ng001/Clash.Meta/common/pool"
	tlsC "github.com/Ruk1ng001/Clash.Meta/component/tls"
	"go.uber.org/atomic"
	"golang.org/x/net/http2"
)

var (
	ErrInvalidLength = errors.New("invalid length")
	ErrSmallBuffer   = errors.New("buffer too small")
)

var defaultHeader = http.Header{
	"content-type": []string{"application/grpc"},
	"user-agent":   []string{"grpc-go/1.36.0"},
}

type DialFn = func(network, addr string) (net.Conn, error)

type Conn struct {
	response  *http.Response
	request   *http.Request
	transport *TransportWrap
	writer    *io.PipeWriter
	once      sync.Once
	close     *atomic.Bool
	err       error
	remain    int
	br        *bufio.Reader
	// deadlines
	deadline *time.Timer
}

type Config struct {
	ServiceName       string
	Host              string
	ClientFingerprint string
}

func (g *Conn) initRequest() {
	response, err := g.transport.RoundTrip(g.request)
	if err != nil {
		g.err = err
		g.writer.Close()
		return
	}

	if !g.close.Load() {
		g.response = response
		g.br = bufio.NewReader(response.Body)
	} else {
		response.Body.Close()
	}
}

func (g *Conn) Read(b []byte) (n int, err error) {
	g.once.Do(g.initRequest)
	if g.err != nil {
		return 0, g.err
	}

	if g.remain > 0 {
		size := g.remain
		if len(b) < size {
			size = len(b)
		}

		n, err = io.ReadFull(g.br, b[:size])
		g.remain -= n
		return
	} else if g.response == nil {
		return 0, net.ErrClosed
	}

	// 0x00 grpclength(uint32) 0x0A uleb128 payload
	_, err = g.br.Discard(6)
	if err != nil {
		return 0, err
	}

	protobufPayloadLen, err := binary.ReadUvarint(g.br)
	if err != nil {
		return 0, ErrInvalidLength
	}

	size := int(protobufPayloadLen)
	if len(b) < size {
		size = len(b)
	}

	n, err = io.ReadFull(g.br, b[:size])
	if err != nil {
		return
	}

	remain := int(protobufPayloadLen) - n
	if remain > 0 {
		g.remain = remain
	}

	return n, nil
}

func (g *Conn) Write(b []byte) (n int, err error) {
	protobufHeader := [binary.MaxVarintLen64 + 1]byte{0x0A}
	varuintSize := binary.PutUvarint(protobufHeader[1:], uint64(len(b)))
	var grpcHeader [5]byte
	grpcPayloadLen := uint32(varuintSize + 1 + len(b))
	binary.BigEndian.PutUint32(grpcHeader[1:5], grpcPayloadLen)

	buf := pool.GetBuffer()
	defer pool.PutBuffer(buf)
	buf.Write(grpcHeader[:])
	buf.Write(protobufHeader[:varuintSize+1])
	buf.Write(b)

	_, err = g.writer.Write(buf.Bytes())
	if err == io.ErrClosedPipe && g.err != nil {
		err = g.err
	}

	return len(b), err
}

func (g *Conn) WriteBuffer(buffer *buf.Buffer) error {
	defer buffer.Release()
	dataLen := buffer.Len()
	varLen := UVarintLen(uint64(dataLen))
	header := buffer.ExtendHeader(6 + varLen)
	header[0] = 0x00
	binary.BigEndian.PutUint32(header[1:5], uint32(1+varLen+dataLen))
	header[5] = 0x0A
	binary.PutUvarint(header[6:], uint64(dataLen))
	_, err := g.writer.Write(buffer.Bytes())

	if err == io.ErrClosedPipe && g.err != nil {
		err = g.err
	}

	return err
}

func (g *Conn) FrontHeadroom() int {
	return 6 + binary.MaxVarintLen64
}

func (g *Conn) Close() error {
	g.close.Store(true)
	if r := g.response; r != nil {
		r.Body.Close()
	}

	return g.writer.Close()
}

func (g *Conn) LocalAddr() net.Addr                { return g.transport.LocalAddr() }
func (g *Conn) RemoteAddr() net.Addr               { return g.transport.RemoteAddr() }
func (g *Conn) SetReadDeadline(t time.Time) error  { return g.SetDeadline(t) }
func (g *Conn) SetWriteDeadline(t time.Time) error { return g.SetDeadline(t) }

func (g *Conn) SetDeadline(t time.Time) error {
	d := time.Until(t)
	if g.deadline != nil {
		g.deadline.Reset(d)
		return nil
	}
	g.deadline = time.AfterFunc(d, func() {
		g.Close()
	})
	return nil
}

func NewHTTP2Client(dialFn DialFn, tlsConfig *tls.Config, Fingerprint string) *TransportWrap {
	wrap := TransportWrap{}

	dialFunc := func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
		pconn, err := dialFn(network, addr)
		if err != nil {
			return nil, err
		}

		wrap.remoteAddr = pconn.RemoteAddr()

		if len(Fingerprint) != 0 {
			if fingerprint, exists := tlsC.GetFingerprint(Fingerprint); exists {
				utlsConn := tlsC.UClient(pconn, cfg, fingerprint)
				if err := utlsConn.(*tlsC.UConn).HandshakeContext(ctx); err != nil {
					pconn.Close()
					return nil, err
				}
				state := utlsConn.(*tlsC.UConn).ConnectionState()
				if p := state.NegotiatedProtocol; p != http2.NextProtoTLS {
					utlsConn.Close()
					return nil, fmt.Errorf("http2: unexpected ALPN protocol %s, want %s", p, http2.NextProtoTLS)
				}
				return utlsConn, nil
			}
		}

		conn := tls.Client(pconn, cfg)
		if err := conn.HandshakeContext(ctx); err != nil {
			pconn.Close()
			return nil, err
		}
		state := conn.ConnectionState()
		if p := state.NegotiatedProtocol; p != http2.NextProtoTLS {
			conn.Close()
			return nil, fmt.Errorf("http2: unexpected ALPN protocol %s, want %s", p, http2.NextProtoTLS)
		}
		return conn, nil
	}

	wrap.Transport = &http2.Transport{
		DialTLSContext:     dialFunc,
		TLSClientConfig:    tlsConfig,
		AllowHTTP:          false,
		DisableCompression: true,
		PingTimeout:        0,
	}

	return &wrap
}

func StreamGunWithTransport(transport *TransportWrap, cfg *Config) (net.Conn, error) {
	serviceName := "GunService"
	if cfg.ServiceName != "" {
		serviceName = cfg.ServiceName
	}

	reader, writer := io.Pipe()
	request := &http.Request{
		Method: http.MethodPost,
		Body:   reader,
		URL: &url.URL{
			Scheme: "https",
			Host:   cfg.Host,
			Path:   fmt.Sprintf("/%s/Tun", serviceName),
			// for unescape path
			Opaque: fmt.Sprintf("//%s/%s/Tun", cfg.Host, serviceName),
		},
		Proto:      "HTTP/2",
		ProtoMajor: 2,
		ProtoMinor: 0,
		Header:     defaultHeader,
	}
	// 使用 http.Client 自动处理重定向
	client := &http.Client{
		Timeout:   time.Second * 30,
		Transport: transport, // 使用自定义 Transport
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			// 这里可以记录重定向的地址
			fmt.Println("重定向到:", req.URL)
			return nil // 继续跟随重定向
		},
	}

	resp, err := client.Do(request)
	if err != nil {
		return nil, err // 处理请求错误
	}
	defer resp.Body.Close()

	// 处理 302 重定向响应
	if resp.StatusCode == http.StatusFound { // 302
		redirectURL := resp.Header.Get("Location")
		fmt.Println("服务器重定向到:", redirectURL)

		// 根据需要，可以创建新的请求并发起到重定向的 URL
		newRequest, err := http.NewRequest(http.MethodPost, redirectURL, reader)
		if err != nil {
			return nil, err
		}
		newRequest.Header = request.Header // 保持头部一致
		newResp, err := client.Do(newRequest)
		if err != nil {
			return nil, err
		}
		defer newResp.Body.Close()
		// 继续处理新的响应
	}

	conn := &Conn{
		request:   request,
		transport: transport,
		writer:    writer,
		close:     atomic.NewBool(false),
	}

	go conn.once.Do(conn.initRequest)
	return conn, nil
}

func StreamGunWithConn(conn net.Conn, tlsConfig *tls.Config, cfg *Config) (net.Conn, error) {
	dialFn := func(network, addr string) (net.Conn, error) {
		return conn, nil
	}

	transport := NewHTTP2Client(dialFn, tlsConfig, cfg.ClientFingerprint)
	return StreamGunWithTransport(transport, cfg)
}
